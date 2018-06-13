package mish

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/nsf/termbox-go"

	"github.com/windmilleng/mish/bridge/fs"
	"github.com/windmilleng/mish/bridge/fs/fss"
	"github.com/windmilleng/mish/cli/dirs"
	"github.com/windmilleng/mish/data"
	"github.com/windmilleng/mish/data/db/db2"
	"github.com/windmilleng/mish/data/db/dbint"
	"github.com/windmilleng/mish/data/db/dbpath"
	"github.com/windmilleng/mish/data/db/storage/storages"
	"github.com/windmilleng/mish/data/pathutil"
	"github.com/windmilleng/mish/logging"
	"github.com/windmilleng/mish/mish/shmill"
	"github.com/windmilleng/mish/os/ospath"
	"github.com/windmilleng/mish/os/temp"
)

// the shell is the controller of our MVC
type Shell struct {
	ctx    context.Context
	dir    string // TODO(dbentley): support a different Millfile
	db     dbint.DB2
	fs     fs.FSBridge
	shmill *shmill.Shmill
	model  *Model
	view   *View

	editCh       chan data.PointerAtRev
	editErrCh    chan error
	termEventCh  chan termbox.Event
	timeCh       <-chan time.Time
	panicCh      chan error
	shmillCh     chan shmill.Event
	shmillCancel context.CancelFunc
}

var ptrID = data.MustNewPointerID(data.AnonymousID, "mirror", data.UserPtr)

func Setup() (*Shell, error) {
	ctx := context.Background()

	wmDir, err := dirs.GetWindmillDir()
	if err != nil {
		return nil, err
	}
	if err := logging.SetupLogger(filepath.Join(wmDir, "mish")); err != nil {
		return nil, err
	}

	dir, err := filepath.Abs(".")
	if err != nil {
		return nil, err
	}
	recipes := storages.NewTestMemoryRecipeStore()
	ptrs := storages.NewMemoryPointers()
	db := db2.NewDB2(recipes, ptrs)
	tmp, err := temp.NewDir("mish")
	if err != nil {
		return nil, err
	}
	opt := db2.NewOptimizer(db, recipes)
	fs := fss.NewLocalFSBridge(ctx, db, opt, tmp)

	_, err = db.AcquirePointer(ctx, ptrID)
	if err != nil {
		return nil, err
	}

	if err := setupMirror(ctx, fs, dir, ptrID); err != nil {
		return nil, err
	}

	if err := termbox.Init(); err != nil {
		return nil, err
	}

	panicCh := make(chan error)

	return &Shell{
		ctx:    ctx,
		dir:    dir,
		db:     db,
		fs:     fs,
		shmill: shmill.NewShmill(fs, ptrID, dir, panicCh),
		model: &Model{
			File:      filepath.Join(dir, pathutil.WMShMill),
			Now:       time.Now(),
			HeadSnap:  data.EmptySnapshotID,
			Collapsed: make(map[int]bool),
			Spinner:   &Spinner{Chars: []rune{'⠋', '⠙', '⠹', '⠸', '⠼', '⠴', '⠦', '⠧', '⠇', '⠏'}},
		},
		view: &View{},

		editCh:      make(chan data.PointerAtRev),
		editErrCh:   make(chan error),
		termEventCh: make(chan termbox.Event),
		panicCh:     panicCh,
	}, nil
}

func setupMirror(ctx context.Context, fs fs.FSBridge, dir string, ptrID data.PointerID) error {
	// TODO(dbentley): allow specifying a different file on the command-line
	matcher, err := ospath.NewFileMatcher(pathutil.WMShMill)
	if err != nil {
		return err
	}
	return fs.ToWMStart(ctx, dir, ptrID, matcher)
}

func (sh *Shell) cancelCmd() {
	if sh.shmillCancel != nil {
		// TODO(dmiller) maybe a ui if it takes too long?
		c := make(chan interface{}, 1)
		sh.shmillCancel()
		if sh.shmillCh != nil {
			go func() {
				// wait for os/exec to tell us that this is done
				for _ = range sh.shmillCh {
				}
				time.Sleep(4 * time.Second)
				c <- struct{}{}
			}()

			select {
			case _ = <-c:
				return
			case <-time.After(3 * time.Second):
				return
			}
		}
	}
}

func (sh *Shell) Run() error {
	defer termbox.Close()
	go sh.waitForEdits()
	go sh.waitForTermEvents()
	sh.timeCh = time.Tick(time.Second)
	defer sh.cancelCmd()

	// run what the mill script currently contains
	sh.startRun()
	// then await input
	for {
		select {
		case head := <-sh.editCh:
			if err := sh.handleEdit(head); err != nil {
				return err
			}
		case err := <-sh.editErrCh:
			return err
		case event, ok := <-sh.shmillCh:
			if !ok {
				sh.shmillCh = nil
			}
			if err := sh.handleShmill(event); err != nil {
				return err
			}
		case event := <-sh.termEventCh:
			if event.Type == termbox.EventKey && event.Ch == 'q' {
				return nil
			}
			sh.handleTerminal(event)
		case t := <-sh.timeCh:
			sh.model.Now = t
			sh.model.Spinner.Incr()
		case err := <-sh.panicCh:
			return err
		}
		sh.model.BlockSizes, sh.model.ShmillHeight = sh.view.Render(sh.model)
	}
}

func concatenateAndDedupe(old, new []string) []string {
	for _, n := range new {
		dupe := false
		for _, o := range old {
			if o == n {
				dupe = true
				break
			}
		}
		if dupe {
			continue
		}
		old = append(old, n)
	}
	return old
}

func (sh *Shell) handleEdit(head data.PointerAtRev) error {
	sh.model.Rev = int(head.Rev)

	ptsAtSnap, err := sh.db.Get(sh.ctx, head)
	if err != nil {
		return err
	}

	pathsChanged, err := sh.db.PathsChanged(sh.ctx, sh.model.HeadSnap, ptsAtSnap.SnapID, data.RecipeRTagForPointer(ptsAtSnap.ID), dbpath.NewAllMatcher())
	if err != nil {
		return err
	}

	sh.model.HeadSnap = ptsAtSnap.SnapID

	sh.model.QueuedFiles = concatenateAndDedupe(sh.model.QueuedFiles, pathsChanged)

	return nil
}

func (sh *Shell) startRun() {
	sh.model.Shmill = NewShmill()
	sh.model.QueuedFiles = nil
	if sh.shmillCh != nil {
		sh.cancelCmd()
	}

	ctx, cancelFunc := context.WithCancel(sh.ctx)
	sh.shmillCancel = cancelFunc
	sh.shmillCh = sh.shmill.Start(ctx, sh.model.SelectedTarget)
}

func stringsEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i, ae := range a {
		if b[i] != ae {
			return false
		}
	}
	return true
}

func (sh *Shell) handleShmill(ev shmill.Event) error {
	m := sh.model.Shmill
	switch ev := ev.(type) {
	case shmill.TargetsFoundEvent:
		sh.model.Targets = ev.Targets
	case shmill.CmdStartedEvent:
		m.Evals = append(m.Evals, &Run{
			cmd:   ev.Cmd,
			start: time.Now(),
		})
	case shmill.CmdOutputEvent:
		run := m.Evals[len(m.Evals)-1].(*Run)
		run.output += ev.Output
	case shmill.CmdDoneEvent:
		run := m.Evals[len(m.Evals)-1].(*Run)
		run.done = true
		run.err = ev.Err
		run.duration = time.Now().Sub(run.start)
	case shmill.ExecDoneEvent:
		m.Err = ev.Err
		m.Done = true
		m.Duration = time.Now().Sub(m.Start)
	}
	return nil
}

func (sh *Shell) handleTerminal(event termbox.Event) {
	if event.Type != termbox.EventKey {
		return
	}

	if event.Ch == 't' {
		sh.model.ShowFlowChooser = !sh.model.ShowFlowChooser
	}

	if sh.model.ShowFlowChooser {
		sh.handleTermForFlowChooser(event)
		return
	}

	sh.handleTerminalForShmill(event)
}

func (sh *Shell) handleTermForFlowChooser(event termbox.Event) {
	switch event.Key {
	case termbox.KeyArrowUp:
		sh.model.FlowChooserPos--
		sh.cycleFlowChooserPos()
	case termbox.KeyArrowDown:
		sh.model.FlowChooserPos++
		sh.cycleFlowChooserPos()
	}

	switch event.Ch {
	case 'r':
		sh.model.ShowFlowChooser = !sh.model.ShowFlowChooser
		sh.runFlow()
	}
}

func (sh *Shell) handleTerminalForShmill(event termbox.Event) {
	switch event.Key {
	case termbox.KeyArrowUp:
		sh.model.Cursor.Line--
		sh.snapCursorToBlock()
	case termbox.KeyArrowDown:
		sh.model.Cursor.Line++
		sh.snapCursorToBlock()
	case termbox.KeyPgdn:
		sh.model.Cursor.Line += sh.model.ShmillHeight
		sh.snapCursorToBlock()
	case termbox.KeyPgup:
		sh.model.Cursor.Line -= sh.model.ShmillHeight
		sh.snapCursorToBlock()
	}

	switch event.Ch {
	case 'r':
		sh.startRun()
	case 'j':
		sh.model.Cursor.Block++
		sh.model.Cursor.Line = 0
		sh.snapCursorToBlock()
	case 'k':
		sh.model.Cursor.Block--
		sh.model.Cursor.Line = 0
		sh.snapCursorToBlock()
	case 'o':
		if sh.model.Collapsed[sh.model.Cursor.Block] {
			delete(sh.model.Collapsed, sh.model.Cursor.Block)
		} else {
			sh.model.Collapsed[sh.model.Cursor.Block] = true
			sh.model.Cursor.Line = 0
		}
	}
}

func (sh *Shell) cycleFlowChooserPos() {
	if sh.model.FlowChooserPos < 0 {
		sh.model.FlowChooserPos = len(sh.model.Targets)
	}

	if sh.model.FlowChooserPos > len(sh.model.Targets) {
		sh.model.FlowChooserPos = 0
	}
}

func (sh *Shell) runFlow() {
	defer sh.startRun()

	if sh.model.FlowChooserPos == 0 {
		sh.model.SelectedTarget = ""
		return
	}

	sh.model.SelectedTarget = sh.model.Targets[sh.model.FlowChooserPos-1]
}

// snapCursorToBlock makes the cursor point to a sensible position.
// E.g., if the Cursor is (4, -1), it will fix it to point to (3, <last line of block 3>)
func (sh *Shell) snapCursorToBlock() {
	c := sh.model.Cursor
	blocks := sh.model.BlockSizes

	sh.model.Cursor = snapCursorToBlock(c, blocks)
}

func snapCursorToBlock(c Cursor, blocks []int) Cursor {
	if c.Block >= len(blocks) {
		// fallen off edge of last block, go to end
		lastBlock := len(blocks) - 1
		c.Block = lastBlock
		c.Line = blocks[c.Block] // subtracting 1 doesn't work here?
		return c
	}
	if c.Block < 0 {
		c.Block = 0
		c.Line = 0
		return c
	}
	// increment to a successive block based on the size of the current block
	if c.Line > blocks[c.Block] {
		c.Line = c.Line - blocks[c.Block]
		c.Block++
		return snapCursorToBlock(c, blocks)
	}
	// decrement to a previous block based on size of previous block
	if c.Line < 0 {
		if c.Block < 1 {
			c.Block = 0
			c.Line = 0
			return c
		}
		c.Line = c.Line + blocks[c.Block-1]
		c.Block--
		return snapCursorToBlock(c, blocks)
	}
	return c
}

func (sh *Shell) lastBlockLine(i int) int {
	if i < 0 {
		return 0
	}
	if i >= len(sh.model.BlockSizes) {
		return 0
	}
	l := sh.model.BlockSizes[i] - 1
	if l < 0 {
		return 0
	}
	return l
}

func (sh *Shell) prevBlock(i int) int {
	if i >= len(sh.model.BlockSizes) {
		return len(sh.model.BlockSizes) - 1
	}
	return i - 1
}

// Below here is code that happens on goroutines other than Run()

func (sh *Shell) waitForEdits() (outerErr error) {
	// TODO(dbentley): the Millfile might run a command that edits this dir, which would cause an edit, which would cause us to start rerunning.
	// That is silly; how can we filter out, while not missing intentional user edits?
	defer func() {
		if outerErr != nil {
			sh.editErrCh <- outerErr
		}
		close(sh.editErrCh)
		close(sh.editCh)
		if r := recover(); r != nil {
			sh.panicCh <- fmt.Errorf("edit panic: %v", r)
		}
	}()

	head := data.PointerAtRev{ID: ptrID}
	for {
		if err := sh.db.Wait(sh.ctx, head); err != nil {
			return err
		}

		var err error
		head, err = sh.db.Head(sh.ctx, ptrID)
		if err != nil {
			return err
		}

		sh.editCh <- head
	}
}

func (sh *Shell) waitForTermEvents() {
	defer func() {
		if r := recover(); r != nil {
			sh.panicCh <- fmt.Errorf("term panic: %v", r)
		}
	}()
	for {
		sh.termEventCh <- termbox.PollEvent()
	}
}
