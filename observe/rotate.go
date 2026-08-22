package observe

import (
	"os"
	"path/filepath"
	"sync"
)

// rotatedSuffix is what [rotatingFile.rotateIfFull] renames the full file to.
const rotatedSuffix = ".1"

// rotatingFile is an append-only file with a ceiling on it, and one previous
// generation kept beside it.
//
// It is the store behind both of the files rig writes — the spans and the log
// lines — because the promise they make is the same one and it is easier to
// keep once: no file ever exceeds the cap, and the disk cost is twice it and
// never more. Numbered generations and a count to configure would be a log
// rotation policy, and a deployment that wants one has one already and should
// point rig at somewhere it can see.
//
// Writes are unbuffered. There is nothing to flush, so a process that is killed
// loses at most the line it was in the middle of — which is why every reader
// here skips a line that does not parse.
type rotatingFile struct {
	mu   sync.Mutex
	path string
	max  int64
	// f is nil once this file has been closed, or once a rotation could not
	// reopen it. Both mean "stop writing" rather than "try again per line".
	f    *os.File
	size int64
}

// openRotating opens the file, creating the directory it lives in.
//
// Opened here rather than at the first line, so that a path nothing can write
// is a startup error naming the path rather than a file that quietly never
// appears and a monitoring page that is quietly empty.
func openRotating(path string, max int64) (*rotatingFile, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}

	// Appending to what a previous run left, so the ceiling is on the file and
	// not on this process's share of it.
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}

	return &rotatingFile{path: path, max: max, f: f, size: info.Size()}, nil
}

// write appends lines, each of which is expected to end in a newline already.
//
// A batch under one lock, because the span exporter arrives with one: a
// hundred spans should not be a hundred rounds of contention with whatever
// else is logging.
func (r *rotatingFile) write(lines ...[]byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.f == nil {
		return nil
	}

	for _, line := range lines {
		if err := r.rotateIfFull(int64(len(line))); err != nil {
			return err
		}

		n, err := r.f.Write(line)
		r.size += int64(n)
		if err != nil {
			return err
		}
	}
	return nil
}

// rotateIfFull moves the file aside when the next line would not fit. Called
// with the lock held.
//
// Rotating before the line rather than after keeps the promise exact: no file
// ever exceeds the cap, so nobody has to reason about how much one line can
// overshoot by.
func (r *rotatingFile) rotateIfFull(next int64) error {
	if r.size+next <= r.max {
		return nil
	}

	if err := r.f.Close(); err != nil {
		return err
	}
	if err := os.Rename(r.path, r.path+rotatedSuffix); err != nil {
		return err
	}

	f, err := os.OpenFile(r.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		// The file is gone and nothing is open: give up on writing rather than
		// retry per line, which would be one failed syscall per line forever.
		r.f = nil
		return err
	}

	r.f, r.size = f, 0
	return nil
}

// close closes the file, and is safe to call twice.
func (r *rotatingFile) close() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.f == nil {
		return nil
	}

	err := r.f.Close()
	r.f = nil
	return err
}
