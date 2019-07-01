// Copyright 2018 Google Inc. All Rights Reserved.
// This file is available under the Apache license.

package tailer

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
	"unicode/utf8"

	log "github.com/sgtsquiggs/tail/logger"
	"github.com/sgtsquiggs/tail/logline"

	"github.com/pkg/errors"
)

// File provides an abstraction over files and named pipes being tailed
// by `mtail`.
type File struct {
	Name     string    // Given name for the file (possibly relative, used for displau)
	Pathname string    // Full absolute path of the file used internally
	LastRead time.Time // time of the last read received on this handle
	regular  bool      // Remember if this is a regular file (or a pipe)
	file     *os.File
	partial  *bytes.Buffer
	lines    chan<- *logline.LogLine // output channel for lines read
	logger   log.Logger
}

// NewFile returns a new File named by the given pathname.  `seenBefore` indicates
// that mtail believes it's seen this pathname before, indicating we should
// retry on error to open the file. `seekToStart` indicates that the file
// should be tailed from offset 0, not EOF; the latter is true for rotated
// files and for files opened when mtail is in oneshot mode.
func NewFile(pathname string, lines chan<- *logline.LogLine, seekToStart bool, logger log.Logger) (*File, error) {
	if logger == nil {
		logger = log.DefaultLogger
	}
	logger.Infof("file.New(%s, %v)", pathname, seekToStart)
	absPath, err := filepath.Abs(pathname)
	if err != nil {
		return nil, err
	}
	f, err := open(absPath, false, logger)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		// Stat failed, log error and return.
		return nil, errors.Wrapf(err, "Failed to stat %q", absPath)
	}
	regular := false
	switch m := fi.Mode(); {
	case m.IsRegular():
		regular = true
		seekWhence := io.SeekEnd
		if seekToStart {
			seekWhence = io.SeekCurrent
		}
		if _, err := f.Seek(0, seekWhence); err != nil {
			return nil, errors.Wrapf(err, "Seek failed on %q", absPath)
		}
		// Named pipes are the same as far as we're concerned, but we can't seek them.
		fallthrough
	case m&os.ModeType == os.ModeNamedPipe:
	default:
		return nil, errors.Errorf("Can't open files with mode %v: %s", m&os.ModeType, absPath)
	}
	return &File{pathname, absPath, time.Now(), regular, f, bytes.NewBufferString(""), lines, logger}, nil
}

func open(pathname string, seenBefore bool, logger log.Logger) (*os.File, error) {
	retries := 3
	retryDelay := 1 * time.Millisecond
	shouldRetry := func() bool {
		// seenBefore indicates also that we're rotating a file that previously worked, so retry.
		if !seenBefore {
			return false
		}
		return retries > 0
	}
	var f *os.File
Retry:
	f, err := os.OpenFile(pathname, os.O_RDONLY|syscall.O_NONBLOCK, 0600)
	if err != nil {
		if shouldRetry() {
			retries--
			time.Sleep(retryDelay)
			retryDelay += 1
			goto Retry
		}
	}
	if err != nil {
		logger.Infof("open failed all retries")
		return nil, err
	}
	logger.Infof("open succeeded %s", pathname)
	return f, nil
}

// Follow reads from the file until EOF.  It tracks log rotations (i.e new inode or device).
func (f *File) Follow() error {
	s1, err := f.file.Stat()
	if err != nil {
		f.logger.Infof("Stat failed on %q: %s", f.Name, err)
		// We have a fd but it's invalid, handle as a rotation (delete/create)
		err := f.doRotation()
		if err != nil {
			return err
		}
	}
	s2, err := os.Stat(f.Pathname)
	if err != nil {
		f.logger.Infof("Stat failed on %q: %s", f.Pathname, err)
		return nil
	}
	if !os.SameFile(s1, s2) {
		f.logger.Infof("New inode detected for %s, treating as rotation", f.Pathname)
		err = f.doRotation()
		if err != nil {
			return err
		}
	} else {
		f.logger.Infof("Path %s already being watched, and inode not changed.",
			f.Pathname)
	}

	f.logger.Info("doing the normal read")
	return f.Read()
}

// doRotation reads the remaining content of the currently opened file, then reopens the new one.
func (f *File) doRotation() error {
	f.logger.Info("doing the rotation flush read")
	if err := f.Read(); err != nil {
		f.logger.Infof("%s: %s", f.Name, err)
	}
	newFile, err := open(f.Pathname, true /*seenBefore*/, f.logger)
	if err != nil {
		return err
	}
	f.file = newFile
	return nil
}

// Read blocks of 4096 bytes from the File, sending LogLines to the given
// channel as newlines are encountered.  If EOF is read, the partial line is
// stored to be concatenated to on the next call.  At EOF, checks for
// truncation and resets the file offset if so.
func (f *File) Read() error {
	b := make([]byte, 0, 4096)
	totalBytes := 0
	for {
		if err := f.file.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
			f.logger.Infof("%s: %s", f.Name, err)
		}
		n, err := f.file.Read(b[:cap(b)])
		f.logger.Infof("Read count %v err %v", n, err)
		totalBytes += n
		b = b[:n]

		// If this time we've read no bytes at all and then hit an EOF, and
		// we're a regular file, check for truncation.
		if err == io.EOF && totalBytes == 0 && f.regular {
			f.logger.Info("Suspected truncation.")
			truncated, terr := f.checkForTruncate()
			if terr != nil {
				f.logger.Infof("checkForTruncate returned with error '%v'", terr)
			}
			if truncated {
				// Try again: offset was greater than filesize and now we've seeked to start.
				continue
			}
		}

		var (
			rune  rune
			width int
		)
		for i := 0; i < len(b) && i < n; i += width {
			rune, width = utf8.DecodeRune(b[i:])
			switch {
			case rune != '\n':
				f.partial.WriteRune(rune)
			default:
				f.sendLine()
			}
		}

		// Return on any error, including EOF.
		if err != nil {
			// Update the last read time if we were able to read anything.
			if totalBytes > 0 {
				f.LastRead = time.Now()
			}
			return err
		}
	}
}

// sendLine sends the contents of the partial buffer off for processing.
func (f *File) sendLine() {
	f.lines <- logline.NewLogLine(f.Name, f.partial.String())
	// reset partial accumulator
	f.partial.Reset()
}

// checkForTruncate checks to see if the current offset into the file
// is past the end of the file based on its size, and if so seeks to
// the start again.
func (f *File) checkForTruncate() (bool, error) {
	currentOffset, err := f.file.Seek(0, io.SeekCurrent)
	f.logger.Infof("current seek position at %d", currentOffset)
	if err != nil {
		return false, err
	}

	fi, err := f.file.Stat()
	if err != nil {
		return false, err
	}

	f.logger.Infof("File size is %d", fi.Size())
	if currentOffset == 0 || fi.Size() >= currentOffset {
		f.logger.Info("no truncate appears to have occurred")
		return false, nil
	}

	// We're about to lose all data because of the truncate so if there's
	// anything in the buffer, send it out.
	if f.partial.Len() > 0 {
		f.sendLine()
	}

	p, serr := f.file.Seek(0, io.SeekStart)
	f.logger.Infof("Truncated?  Seeked to %d: %v", p, serr)
	return true, serr
}

func (f *File) Stat() (os.FileInfo, error) {
	return f.file.Stat()
}

func (f *File) Close() error {
	return f.file.Close()
}
