// Copyright 2015 Google Inc. All Rights Reserved.
// This file is available under the Apache license.

package watcher

import (
	"expvar"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sync"
	"time"

	log "github.com/sgtsquiggs/tail/logger"

	"github.com/fsnotify/fsnotify"
	"github.com/pkg/errors"
)

var (
	errorCount = expvar.NewInt("log_watcher_error_count")
)

type watch struct {
	c  chan Event
	fi os.FileInfo
}

// LogWatcher implements a Watcher for watching real filesystems.
type LogWatcher struct {
	watcher    *fsnotify.Watcher
	pollTicker *time.Ticker

	eventsMu sync.RWMutex
	events   []chan Event

	watchedMu sync.RWMutex // protects `watched'
	watched   map[string]*watch

	stopTicks chan struct{} // Channel to notify ticker to stop.

	ticksDone  chan struct{} // Channel to notify when the ticks handler is done.
	eventsDone chan struct{} // Channel to notify when the events handler is done.

	closeOnce sync.Once

	logger log.Logger
}

// Option used to set trailer options.
type Option func(*LogWatcher) error

// Logger defines the logger.
func Logger(l log.Logger) Option {
	return func(t *LogWatcher) error {
		t.logger = l
		return nil
	}
}

// NewLogWatcher returns a new LogWatcher, or returns an error.
func NewLogWatcher(pollInterval time.Duration, enableFsnotify bool, options ...Option) (*LogWatcher, error) {
	var f *fsnotify.Watcher
	var fsErr error
	if enableFsnotify {
		// if there is an error, log it after options are applied
		f, fsErr = fsnotify.NewWatcher()
	}
	if f == nil && pollInterval == 0 {
		pollInterval = time.Millisecond * 250
	}
	w := &LogWatcher{
		watcher: f,
		events:  make([]chan Event, 0),
		watched: make(map[string]*watch),
		logger:  log.DefaultLogger,
	}
	if err := w.SetOption(options...); err != nil {
		return nil, err
	}
	if fsErr != nil {
		w.logger.Warning(fsErr)
	}
	if pollInterval > 0 {
		w.pollTicker = time.NewTicker(pollInterval)
		w.stopTicks = make(chan struct{})
		w.ticksDone = make(chan struct{})
		go w.runTicks()
	}
	if f != nil {
		w.eventsDone = make(chan struct{})
		go w.runEvents()
	}
	return w, nil
}

// SetOption takes one or more option functions and applies them in order to Tailer.
func (w *LogWatcher) SetOption(options ...Option) error {
	for _, option := range options {
		if err := option(w); err != nil {
			return err
		}
	}
	return nil
}

// Events returns a new readable channel of events from this watcher.
func (w *LogWatcher) Events() (int, <-chan Event) {
	w.eventsMu.Lock()
	handle := len(w.events)
	ch := make(chan Event)
	w.events = append(w.events, ch)
	w.eventsMu.Unlock()
	return handle, ch
}

func (w *LogWatcher) sendEvent(e Event) {
	w.watchedMu.RLock()
	watch, ok := w.watched[e.Pathname]
	w.watchedMu.RUnlock()
	if !ok {
		d := filepath.Dir(e.Pathname)
		w.watchedMu.RLock()
		watch, ok = w.watched[d]
		w.watchedMu.RUnlock()
	}
	if ok {
		watch.c <- e
		return
	}
	w.logger.Infof("No channel for path %q", e.Pathname)
}

func (w *LogWatcher) runTicks() {
	defer close(w.ticksDone)

	if w.pollTicker == nil {
		return
	}

Exit:
	for {
		select {
		case _ = <-w.pollTicker.C:
			w.watchedMu.Lock()
			for n, watched := range w.watched {
				w.pollWatchedPathLocked(n, watched)
			}
			w.watchedMu.Unlock()
		case <-w.stopTicks:
			w.pollTicker.Stop()
			break Exit
		}
	}
}

// pollWatchedPathLocked polls an already-watched path for updates.  w.watchedMu must be locked when called.
func (w *LogWatcher) pollWatchedPathLocked(pathname string, watched *watch) {
	w.logger.Info("stat")
	fi, err := os.Stat(pathname)
	if err != nil {
		w.logger.Info(err)
		return
	}

	// fsnotify does not send update events for the directory itself.
	if fi.IsDir() {
		w.pollDirectoryLocked(watched.c, pathname)
	} else if watched.fi == nil || fi.ModTime().Sub(watched.fi.ModTime()) > 0 {
		w.logger.Infof("sending update for %s", pathname)
		watched.c <- Event{Update, pathname}
	}

	w.logger.Info("Update fi")
	watched.fi = fi
}

func (w *LogWatcher) pollDirectoryLocked(c chan Event, pathname string) {
	matches, err := filepath.Glob(path.Join(pathname, "*"))
	if err != nil {
		w.logger.Info(err)
		return
	}
	// TODO(jaq): how do we avoid duplicate notifies for things that are already in the watch list?
	for _, match := range matches {
		fi, err := os.Stat(match)
		if err != nil {
			w.logger.Info(err)
			continue
		}

		watched, ok := w.watched[match]
		switch {
		case !ok:
			w.logger.Infof("sending create for %s", match)
			c <- Event{Create, match}
			w.watched[match] = &watch{c: c, fi: fi}
		case watched.fi != nil && fi.ModTime().Sub(watched.fi.ModTime()) > 0:
			w.logger.Infof("sending update for %s", match)
			c <- Event{Update, match}
			w.watched[match].fi = fi
		default:
			w.logger.Infof("No modtime change for %s, no send", match)
		}
		if fi.IsDir() {
			w.pollDirectoryLocked(c, match)
		}
	}
}

// runEvents assumes that w.watcher is not nil
func (w *LogWatcher) runEvents() {
	defer close(w.eventsDone)

	// Suck out errors and dump them to the error log.
	go func() {
		for err := range w.watcher.Errors {
			errorCount.Add(1)
			w.logger.Errorf("fsnotify error: %s\n", err)
		}
	}()

	for e := range w.watcher.Events {
		w.logger.Infof("watcher event %v", e)
		switch {
		case e.Op&fsnotify.Create == fsnotify.Create:
			w.sendEvent(Event{Create, e.Name})
		case e.Op&fsnotify.Write == fsnotify.Write,
			e.Op&fsnotify.Chmod == fsnotify.Chmod:
			w.sendEvent(Event{Update, e.Name})
		case e.Op&fsnotify.Remove == fsnotify.Remove:
			w.sendEvent(Event{Delete, e.Name})
		case e.Op&fsnotify.Rename == fsnotify.Rename:
			// Rename is only issued on the original file path; the new name receives a Create event
			w.sendEvent(Event{Delete, e.Name})
		default:
			panic(fmt.Sprintf("unknown op type %v", e.Op))
		}
	}
	w.logger.Infof("Shutting down log watcher.")
}

// Close shuts down the LogWatcher.  It is safe to call this from multiple clients.
func (w *LogWatcher) Close() (err error) {
	w.closeOnce.Do(func() {
		if w.watcher != nil {
			err = w.watcher.Close()
			<-w.eventsDone
		}
		if w.pollTicker != nil {
			close(w.stopTicks)
			<-w.ticksDone
		}
		w.logger.Info("Closing events channels")
		w.eventsMu.Lock()
		for _, c := range w.events {
			close(c)
		}
		w.eventsMu.Unlock()
	})
	return nil
}

// Add adds a path to the list of watched items.
// If the path is already being watched, then nothing is changed -- the new handle does not replace the old one.
func (w *LogWatcher) Add(path string, handle int) error {
	w.eventsMu.RLock()
	if handle > len(w.events) {
		return errors.Errorf("no such event handle %d", handle)
	}
	w.eventsMu.RUnlock()
	if w.IsWatching(path) {
		return nil
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return errors.Wrapf(err, "Failed to lookup absolutepath of %q", path)
	}
	w.logger.Infof("Adding a watch on resolved path %q", absPath)
	if w.watcher != nil {
		err = w.watcher.Add(absPath)
		if err != nil {
			if os.IsPermission(err) {
				w.logger.Infof("Skipping permission denied error on adding a watch.")
			} else {
				return errors.Wrapf(err, "Failed to create a new watch on %q", absPath)
			}
		}
	}
	w.watchedMu.Lock()
	w.eventsMu.RLock()
	w.watched[absPath] = &watch{c: w.events[handle]}
	w.eventsMu.RUnlock()
	w.watchedMu.Unlock()
	return nil
}

// IsWatching indicates if the path is being watched. It includes both
// filenames and directories.
func (w *LogWatcher) IsWatching(path string) bool {
	absPath, err := filepath.Abs(path)
	if err != nil {
		w.logger.Infof("Couldn't resolve path %q: %s", absPath, err)
		return false
	}
	w.logger.Infof("Resolved path for lookup %q", absPath)
	w.watchedMu.RLock()
	_, ok := w.watched[absPath]
	w.watchedMu.RUnlock()
	return ok
}

func (w *LogWatcher) Remove(path string) error {
	w.watchedMu.Lock()
	delete(w.watched, path)
	w.watchedMu.Unlock()
	if w.watcher != nil {
		return w.watcher.Remove(path)
	}
	return nil
}
