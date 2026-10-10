// Package logging configures the process-wide slog logger.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Setup installs a default slog logger. With logFile set, logs go to that
// file (rotated at 10 MB, keeping 3 old files); otherwise to stderr.
func Setup(level, logFile string) (io.Closer, error) {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(strings.ToUpper(level))); err != nil {
		return nil, fmt.Errorf("log level %q: %w", level, err)
	}
	var w io.Writer = os.Stderr
	var closer io.Closer = nopCloser{}
	if logFile != "" {
		rf, err := OpenRotating(logFile, 10<<20, 3)
		if err != nil {
			return nil, err
		}
		w, closer = rf, rf
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: lvl})))
	return closer, nil
}

type nopCloser struct{}

func (nopCloser) Close() error { return nil }

// RotatingFile is an append-only log file that rotates by size:
// app.log → app.log.1 → … → app.log.<keep>.
type RotatingFile struct {
	mu   sync.Mutex
	path string
	max  int64
	keep int
	f    *os.File
	size int64
}

func OpenRotating(path string, maxBytes int64, keep int) (*RotatingFile, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	r := &RotatingFile{path: path, max: maxBytes, keep: keep}
	return r, r.open()
}

func (r *RotatingFile) open() error {
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	r.f, r.size = f, fi.Size()
	return nil
}

func (r *RotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.size+int64(len(p)) > r.max && r.size > 0 {
		r.rotate()
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

func (r *RotatingFile) rotate() {
	r.f.Close()
	for i := r.keep - 1; i >= 1; i-- {
		os.Rename(fmt.Sprintf("%s.%d", r.path, i), fmt.Sprintf("%s.%d", r.path, i+1))
	}
	os.Rename(r.path, r.path+".1")
	if err := r.open(); err != nil {
		// Fall back to stderr rather than losing logs entirely.
		r.f = os.Stderr
	}
}

func (r *RotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.f.Close()
}
