// Package watcher signals when the Messages database may have changed.
//
// It watches chat.db, chat.db-wal and chat.db-shm with fsnotify (kqueue on
// macOS) plus their directory (the WAL is deleted and recreated on
// checkpoints, which file watches alone would miss). Bursts of writes are
// debounced into one tick. A periodic safety tick covers anything the
// kernel notifications miss; the consumer's ROWID high-water mark makes
// late detection harmless.
package watcher

import (
	"context"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

type Options struct {
	DBPath   string
	Debounce time.Duration // default 250ms
	Poll     time.Duration // safety tick interval; default 30s
}

// Run sends on ticks whenever the database may have changed, until ctx is
// done. Ticks are coalesced: if the consumer is busy, at most one is pending.
func Run(ctx context.Context, o Options, ticks chan<- struct{}) error {
	if o.Debounce <= 0 {
		o.Debounce = 250 * time.Millisecond
	}
	if o.Poll <= 0 {
		o.Poll = 30 * time.Second
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer w.Close()

	dir := filepath.Dir(o.DBPath)
	base := filepath.Base(o.DBPath)
	files := map[string]bool{base: true, base + "-wal": true, base + "-shm": true}
	if err := w.Add(dir); err != nil {
		return err
	}
	for f := range files {
		// The WAL/SHM may not exist yet; the directory watch catches creation.
		_ = w.Add(filepath.Join(dir, f))
	}

	send := func() {
		select {
		case ticks <- struct{}{}:
		default: // a tick is already pending
		}
	}

	debounce := time.NewTimer(time.Hour)
	debounce.Stop()
	poll := time.NewTicker(o.Poll)
	defer poll.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-w.Events:
			if !ok {
				return nil
			}
			name := filepath.Base(ev.Name)
			if !files[name] {
				continue
			}
			if ev.Has(fsnotify.Create) {
				_ = w.Add(ev.Name) // re-watch a recreated WAL/SHM
			}
			debounce.Reset(o.Debounce)
		case err, ok := <-w.Errors:
			if !ok {
				return nil
			}
			slog.Warn("file watcher error", "err", err)
		case <-debounce.C:
			send()
		case <-poll.C:
			send()
		}
	}
}
