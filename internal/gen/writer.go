package gen

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
)

// RotateMode selects how a full log file is rotated.
type RotateMode string

const (
	// RotateRename renames app.log → app.log.1 and creates a fresh app.log
	// (new inode). This is what logrotate's default and Python's
	// RotatingFileHandler do; tailers follow it by noticing the inode change.
	RotateRename RotateMode = "rename"
	// RotateTruncate copies app.log → app.log.1 and truncates app.log in place
	// (same inode), like logrotate's copytruncate. Lines written between the
	// copy and the truncate are lost, and tailers must notice the size drop.
	RotateTruncate RotateMode = "truncate"
)

// lineWriter is the sink a stream writes rendered lines to.
type lineWriter interface {
	Write(p []byte) error
	Flush() error
	Close() error
	Path() string
}

// rotatingFile is an append-only, size-rotated log file with a large buffer.
type rotatingFile struct {
	path      string
	maxBytes  int64
	keep      int
	mode      RotateMode
	f         *os.File
	w         *bufio.Writer
	size      int64
	bufSize   int
	rotations *atomic.Int64
}

func openRotating(path string, maxBytes int64, keep int, mode RotateMode, bufSize int, rotations *atomic.Int64) (*rotatingFile, error) {
	r := &rotatingFile{path: path, maxBytes: maxBytes, keep: keep, mode: mode, bufSize: bufSize, rotations: rotations}
	if err := r.open(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *rotatingFile) open() error {
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	r.f = f
	r.size = st.Size()
	if r.w == nil {
		r.w = bufio.NewWriterSize(f, r.bufSize)
	} else {
		r.w.Reset(f)
	}
	return nil
}

func (r *rotatingFile) Path() string { return r.path }

func (r *rotatingFile) Write(p []byte) error {
	if r.maxBytes > 0 && r.size > 0 && r.size+int64(len(p)) > r.maxBytes {
		if err := r.rotate(); err != nil {
			return err
		}
	}
	n, err := r.w.Write(p)
	r.size += int64(n)
	return err
}

func (r *rotatingFile) Flush() error { return r.w.Flush() }

func (r *rotatingFile) Close() error {
	if err := r.w.Flush(); err != nil {
		r.f.Close()
		return err
	}
	return r.f.Close()
}

func backupName(path string, n int) string { return path + "." + strconv.Itoa(n) }

// shiftBackups renames path.N-1 → path.N for N = keep..2 and removes the
// oldest, leaving path.1 free.
func (r *rotatingFile) shiftBackups() {
	if r.keep <= 0 {
		return
	}
	os.Remove(backupName(r.path, r.keep))
	for i := r.keep - 1; i >= 1; i-- {
		src := backupName(r.path, i)
		if _, err := os.Stat(src); err == nil {
			os.Rename(src, backupName(r.path, i+1))
		}
	}
}

func (r *rotatingFile) rotate() error {
	if err := r.w.Flush(); err != nil {
		return err
	}
	r.shiftBackups()
	switch r.mode {
	case RotateTruncate:
		if r.keep > 0 {
			if err := copyFile(r.path, backupName(r.path, 1)); err != nil {
				return fmt.Errorf("copytruncate %s: %w", r.path, err)
			}
		}
		if err := r.f.Truncate(0); err != nil {
			return err
		}
		r.size = 0
	default:
		if err := r.f.Close(); err != nil {
			return err
		}
		if r.keep > 0 {
			if err := os.Rename(r.path, backupName(r.path, 1)); err != nil {
				return err
			}
		} else {
			os.Remove(r.path)
		}
		if err := r.open(); err != nil {
			return err
		}
	}
	r.rotations.Add(1)
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// stdoutWriter multiplexes every stream onto one buffered stdout, so a
// container's log driver sees a single interleaved stream. Each Write is one
// or more whole lines, so lines never interleave mid-way.
type stdoutWriter struct {
	mu sync.Mutex
	w  *bufio.Writer
}

var sharedStdout = &stdoutWriter{w: bufio.NewWriterSize(os.Stdout, 256<<10)}

func (s *stdoutWriter) Path() string { return "stdout" }

func (s *stdoutWriter) Write(p []byte) error {
	s.mu.Lock()
	_, err := s.w.Write(p)
	s.mu.Unlock()
	return err
}

func (s *stdoutWriter) Flush() error {
	s.mu.Lock()
	err := s.w.Flush()
	s.mu.Unlock()
	return err
}

// Close only flushes: stdout itself stays open for other streams.
func (s *stdoutWriter) Close() error { return s.Flush() }
