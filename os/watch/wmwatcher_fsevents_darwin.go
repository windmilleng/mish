// +build fsevents

package watch

import (
	"path/filepath"
	"sync"
	"time"

	"github.com/windmilleng/fsevents"
	"github.com/windmilleng/fsnotify"
	"github.com/windmilleng/mish/os/ospath"
)

type darwinNotify struct {
	stream *fsevents.EventStream
	events chan fsnotify.Event
	errors chan error
	stop   chan struct{}

	// TODO(nick): This mutex is needed for the case where we add paths after we
	// start watching. But because fsevents supports recursive watches, we don't
	// actually need this feature. We should change the api contract of wmNotify
	// so that, for recursive watches, we can guarantee that the path list doesn't
	// change.
	sm *sync.Mutex
}

func (d *darwinNotify) isTrackingPath(path string) bool {
	d.sm.Lock()
	defer d.sm.Unlock()
	for _, p := range d.stream.Paths {
		if p == path {
			return true
		}
	}
	return false
}

func (d *darwinNotify) loop() {
	lastCreate := ""
	for {
		select {
		case <-d.stop:
			return
		case x, ok := <-d.stream.Events:
			if !ok {
				return
			}

			for _, y := range x {
				y.Path = filepath.Join("/", y.Path)
				op := eventFlagsToOp(y.Flags)

				// Sometimes we get duplicate CREATE events.
				//
				// This is exercised by TestEventOrdering, which creates lots of files
				// and generates duplicate CREATE events for some of them.
				if op == fsnotify.Create {
					if lastCreate == y.Path {
						continue
					}
					lastCreate = y.Path
				}

				// ignore events that say the watched directory
				// has been created. these are fired spuriously
				// on initiation.
				if op == fsnotify.Create || op == fsnotify.Write {
					if d.isTrackingPath(y.Path) {
						continue
					}
				}

				d.events <- fsnotify.Event{
					Name: y.Path,
					Op:   op,
				}
			}
		}
	}
}

func (d *darwinNotify) Add(name string) error {
	d.sm.Lock()
	defer d.sm.Unlock()

	es := d.stream

	// Check if this is a subdirectory of any of the paths
	// we're already watching.
	for _, parent := range es.Paths {
		_, isChild := ospath.Child(parent, name)
		if isChild {
			return nil
		}
	}

	es.Paths = append(es.Paths, name)
	if len(es.Paths) == 1 {
		go d.loop()
		es.Start()
	} else {
		es.Restart()
	}

	return nil
}

func (d *darwinNotify) Close() error {
	d.sm.Lock()
	defer d.sm.Unlock()

	d.stream.Stop()
	close(d.errors)
	close(d.stop)

	return nil
}

func (d *darwinNotify) Events() chan fsnotify.Event {
	return d.events
}

func (d *darwinNotify) Errors() chan error {
	return d.errors
}

func newWMWatcher() (wmNotify, error) {
	dw := &darwinNotify{
		stream: &fsevents.EventStream{
			Latency: 1 * time.Millisecond,
			Flags:   fsevents.FileEvents,
		},
		sm:     &sync.Mutex{},
		events: make(chan fsnotify.Event),
		errors: make(chan error),
		stop:   make(chan struct{}),
	}

	return dw, nil
}

func eventFlagsToOp(flags fsevents.EventFlags) fsnotify.Op {
	if flags&fsevents.ItemCreated != 0 {
		return fsnotify.Create
	}
	if flags&fsevents.ItemRemoved != 0 {
		return fsnotify.Remove
	}
	if flags&fsevents.ItemRenamed != 0 {
		return fsnotify.Rename
	}
	if flags&fsevents.ItemChangeOwner != 0 {
		return fsnotify.Chmod
	}

	return fsnotify.Write
}
