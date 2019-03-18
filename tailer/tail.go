// Copyright 2011 Google Inc. All Rights Reserved.
// This file is available under the Apache license.

// Package tailer provides a class that is responsible for tailing log files
// and extracting new log lines to be passed into the virtual machines.
package tailer

// For regular files, mtail gets notified on modifications (i.e. appends) to
// log files that are being watched, in order to read the new lines. Log files
// can also be rotated, so mtail is also notified of creates in the log file
// directory.

import (
	"expvar"
	"html/template"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/pkg/errors"

	log "github.com/sgtsquiggs/tail/logger"
	"github.com/sgtsquiggs/tail/logline"
	"github.com/sgtsquiggs/tail/watcher"
)

var (
	// logCount records the number of logs that are being tailed
	logCount = expvar.NewInt("log_count")
)

// Tailer receives notification of changes from a Watcher and extracts new log
// lines from files. It also handles new log file creation events and log
// rotations.
type Tailer struct {
	lines chan<- *logline.LogLine // Logfile lines being emitted.
	w     watcher.Watcher

	handlesMu sync.RWMutex     // protects `handles'
	handles   map[string]*File // File handles for each pathname.

	globPatternsMu sync.RWMutex        // protects `globPatterns'
	globPatterns   map[string]struct{} // glob patterns to match newly created files in dir paths against

	runDone chan struct{} // Signals termination of the run goroutine.

	eventsHandle int // record the handle with which to add new log files to the watcher

	oneShot bool

	logger log.Logger
}

// Option used to set tailer options.
type Option func(*Tailer) error

// OneShot puts the tailer in one-shot mode.
func OneShot(t *Tailer) error {
	t.oneShot = true
	return nil
}

// Logger defines the logger.
func Logger(l log.Logger) Option {
	return func(t *Tailer) error {
		t.logger = l
		return nil
	}
}

// New creates a new Tailer.
func New(lines chan<- *logline.LogLine, w watcher.Watcher, options ...Option) (*Tailer, error) {
	if lines == nil {
		return nil, errors.New("can't create tailer without lines channel")
	}
	if w == nil {
		return nil, errors.New("can't create tailer without W")
	}
	t := &Tailer{
		lines:        lines,
		w:            w,
		handles:      make(map[string]*File),
		globPatterns: make(map[string]struct{}),
		runDone:      make(chan struct{}),
		logger:       log.DefaultLogger,
	}
	if err := t.SetOption(options...); err != nil {
		return nil, err
	}
	handle, eventsChan := t.w.Events()
	t.eventsHandle = handle
	go t.run(eventsChan)
	return t, nil
}

// SetOption takes one or more option functions and applies them in order to Tailer.
func (t *Tailer) SetOption(options ...Option) error {
	for _, option := range options {
		if err := option(t); err != nil {
			return err
		}
	}
	return nil
}

// setHandle sets a file handle under it's pathname
func (t *Tailer) setHandle(pathname string, f *File) error {
	absPath, err := filepath.Abs(pathname)
	if err != nil {
		return errors.Wrapf(err, "Failed to lookup abspath of %q", pathname)
	}
	t.handlesMu.Lock()
	defer t.handlesMu.Unlock()
	t.handles[absPath] = f
	return nil
}

// handleForPath retrives a file handle for a pathname.
func (t *Tailer) handleForPath(pathname string) (*File, bool) {
	absPath, err := filepath.Abs(pathname)
	if err != nil {
		t.logger.Infof("Couldn't resolve path %q: %s", pathname, err)
		return nil, false
	}
	t.handlesMu.Lock()
	defer t.handlesMu.Unlock()
	fd, ok := t.handles[absPath]
	return fd, ok
}

func (t *Tailer) hasHandle(pathname string) bool {
	_, ok := t.handleForPath(pathname)
	return ok
}

// AddPattern adds a pattern to the list of patterns to filter filenames against.
func (t *Tailer) AddPattern(pattern string) error {
	absPath, err := filepath.Abs(pattern)
	if err != nil {
		t.logger.Infof("Couldn't canonicalize path %q: %s", pattern, err)
		return err
	}
	t.logger.Infof("AddPattern: %s", absPath)
	t.globPatternsMu.Lock()
	t.globPatterns[absPath] = struct{}{}
	t.globPatternsMu.Unlock()
	return nil
}

// TailPattern registers a pattern to be tailed.  If pattern is a plain
// file then it is watched for updates and opened.  If pattern is a glob, then
// all paths that match the glob are opened and watched, and the directories
// containing those matches, if any, are watched.
func (t *Tailer) TailPattern(pattern string) error {
	if err := t.AddPattern(pattern); err != nil {
		return err
	}
	// Add a watch on the containing directory, so we know when a rotation
	// occurs or something shows up that matches this pattern.
	if err := t.watchDirname(pattern); err != nil {
		return err
	}
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return err
	}
	t.logger.Infof("glob matches: %v", matches)
	// Error if there are no matches, but if they show up later, they'll get picked up by the directory watch set above.
	if len(matches) == 0 {
		return errors.Errorf("No matches for pattern %q", pattern)
	}
	for _, pathname := range matches {
		err := t.TailPath(pathname)
		if err != nil {
			return errors.Wrapf(err, "attempting to tail %q", pathname)
		}
	}
	return nil
}

// TailPath registers a filesystem pathname to be tailed.
func (t *Tailer) TailPath(pathname string) error {
	if t.hasHandle(pathname) {
		t.logger.Infof("already watching %q", pathname)
		return nil
	}
	if err := t.w.Add(pathname, t.eventsHandle); err != nil {
		return err
	}
	// New file at start of program, seek to EOF.
	return t.openLogPath(pathname, false)
}

// handleLogEvent is dispatched when an Event is received, causing the tailer
// to read all available bytes from an already-opened file and send each log
// line onto lines channel.  Because we handle rotations and truncates when
// reaching EOF in the file reader itself, we don't care what the signal is
// from the filewatcher.
func (t *Tailer) handleLogEvent(pathname string) {
	t.logger.Infof("handleLogUpdate %s", pathname)
	fd, ok := t.handleForPath(pathname)
	if !ok {
		t.logger.Infof("No file handle found for %q, but is being watched", pathname)
		// We want to open files we have watches on in case the file was
		// unreadable before now; but we have to copmare against the glob to be
		// sure we don't just add all the files in a watched directory as they
		// get modified.
		t.handleCreateGlob(pathname)
		return
	}
	doFollow(fd, t.logger)
}

// doFollow performs the Follow on an existing file descriptor, logging any errors
func doFollow(fd *File, logger log.Logger) {
	err := fd.Follow()
	if err != nil && err != io.EOF {
		logger.Info(err)
	}
}

// watchDirname adds the directory containing a path to be watched.
func (t *Tailer) watchDirname(pathname string) error {
	absPath, err := filepath.Abs(pathname)
	if err != nil {
		return err
	}
	d := filepath.Dir(absPath)
	return t.w.Add(d, t.eventsHandle)
}

// openLogPath opens a log file named by pathname.
func (t *Tailer) openLogPath(pathname string, seekToStart bool) error {
	t.logger.Infof("openlogPath %s %v", pathname, seekToStart)
	if err := t.watchDirname(pathname); err != nil {
		return err
	}
	f, err := NewFile(pathname, t.lines, seekToStart || t.oneShot, t.logger)
	if err != nil {
		// Doesn't exist yet. We're watching the directory, so we'll pick it up
		// again on create; return successfully.
		if os.IsNotExist(err) {
			t.logger.Infof("pathname %q doesn't exist (yet?)", f.Pathname)
			return nil
		}
		return err
	}
	t.logger.Infof("Adding a file watch on %q", f.Pathname)
	if err := t.w.Add(f.Pathname, t.eventsHandle); err != nil {
		return err
	}
	if err := t.setHandle(pathname, f); err != nil {
		return err
	}
	if err := f.Read(); err != nil && err != io.EOF {
		return err
	}
	t.logger.Infof("Tailing %s", f.Pathname)
	logCount.Add(1)
	return nil
}

// handleCreateGlob matches the pathname against the glob patterns and starts tailing the file.
func (t *Tailer) handleCreateGlob(pathname string) {
	t.globPatternsMu.RLock()
	defer t.globPatternsMu.RUnlock()

	for pattern := range t.globPatterns {
		matched, err := filepath.Match(pattern, pathname)
		if err != nil {
			t.logger.Warningf("Unexpected bad pattern %q not detected earlier", pattern)
			continue
		}
		if !matched {
			t.logger.Infof("%q did not match pattern %q", pathname, pattern)
			continue
		}
		t.logger.Infof("New file %q matched existing glob %q", pathname, pattern)
		// If this file was just created, read from the start of the file.
		if err := t.openLogPath(pathname, true); err != nil {
			t.logger.Infof("Failed to tail new file %q: %s", pathname, err)
		}
		t.logger.Infof("started tailing %q", pathname)
		return
	}
	t.logger.Infof("did not start tailing %q", pathname)
}

// run the main event loop for the Tailer.  It receives notification of
// log file changes from the watcher channel, and dispatches the log event
// handler.
func (t *Tailer) run(events <-chan watcher.Event) {
	defer close(t.runDone)
	defer close(t.lines)

	for e := range events {
		t.logger.Infof("Event type %#v", e)
		t.handleLogEvent(e.Pathname)
	}
	t.logger.Infof("Shutting down tailer.")
}

// Close signals termination to the watcher.
func (t *Tailer) Close() error {
	if err := t.w.Close(); err != nil {
		return err
	}
	<-t.runDone
	return nil
}

const tailerTemplate = `
<h2 id="tailer">Log Tailer</h2>
<h3>Patterns</h3>
<ul>
{{range $name, $val := $.Patterns}}
<li><pre>{{$name}}</pre></li>
{{end}}
</ul>
<h3>Log files watched</h3>
<table border=1>
<tr>
<th>pathname</th>
<th>errors</th>
<th>rotations</th>
<th>truncations</th>
<th>lines read</th>
</tr>
{{range $name, $val := $.Handles}}
<tr>
<td><pre>{{$name}}</pre></td>
<td>{{index $.Errors $name}}</td>
<td>{{index $.Rotations $name}}</td>
<td>{{index $.Truncs $name}}</td>
<td>{{index $.Lines $name}}</td>
</tr>
{{end}}
</table>
</ul>
`

// WriteStatusHTML emits the Tailer's state in HTML format to the io.Writer w.
func (t *Tailer) WriteStatusHTML(w io.Writer) error {
	tpl, err := template.New("tailer").Parse(tailerTemplate)
	if err != nil {
		return err
	}
	t.handlesMu.RLock()
	defer t.handlesMu.RUnlock()
	t.globPatternsMu.RLock()
	defer t.globPatternsMu.RUnlock()
	data := struct {
		Handles   map[string]*File
		Patterns  map[string]struct{}
		Rotations map[string]string
		Lines     map[string]string
		Errors    map[string]string
		Truncs    map[string]string
	}{
		t.handles,
		t.globPatterns,
		make(map[string]string),
		make(map[string]string),
		make(map[string]string),
		make(map[string]string),
	}
	for _, pair := range []struct {
		v *expvar.Map
		m map[string]string
	}{
		{logErrors, data.Errors},
		{logRotations, data.Rotations},
		{logTruncs, data.Truncs},
		{lineCount, data.Lines},
	} {
		pair.v.Do(func(kv expvar.KeyValue) {
			pair.m[kv.Key] = kv.Value.String()
		})
	}
	return tpl.Execute(w, data)
}

// Gc removes file handles that have had no reads for 24h or more.
func (t *Tailer) Gc() error {
	t.handlesMu.Lock()
	defer t.handlesMu.Unlock()
	for k, v := range t.handles {
		if time.Since(v.LastRead) > (time.Hour * 24) {
			if err := t.w.Remove(v.Pathname); err != nil {
				t.logger.Info(err)
			}
			if err := v.Close(); err != nil {
				t.logger.Info(err)
			}
			delete(t.handles, k)
		}
	}
	return nil
}

// StartExpiryLoop runs a permanent goroutine to expire metrics every duration.
func (t *Tailer) StartGcLoop(duration time.Duration) {
	if duration <= 0 {
		t.logger.Info("Log handle expiration disabled")
		return
	}
	go func() {
		t.logger.Infof("Starting log handle expiry loop every %s", duration.String())
		ticker := time.NewTicker(duration)
		for range ticker.C {
			if err := t.Gc(); err != nil {
				t.logger.Info(err)
			}
		}
	}()
}
