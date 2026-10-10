package watcher

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTicksOnWritesAndWALRecreate(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "chat.db")
	os.WriteFile(db, []byte("x"), 0o600)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ticks := make(chan struct{}, 1)
	go Run(ctx, Options{DBPath: db, Debounce: 20 * time.Millisecond, Poll: time.Hour}, ticks)
	time.Sleep(100 * time.Millisecond) // let watches register

	expectTick := func(what string) {
		t.Helper()
		select {
		case <-ticks:
		case <-time.After(2 * time.Second):
			t.Fatalf("no tick after %s", what)
		}
	}
	expectNone := func(what string) {
		t.Helper()
		select {
		case <-ticks:
			t.Fatalf("unexpected tick after %s", what)
		case <-time.After(150 * time.Millisecond):
		}
	}

	// A burst of writes coalesces into one tick.
	f, _ := os.OpenFile(db, os.O_WRONLY|os.O_APPEND, 0)
	for i := 0; i < 20; i++ {
		f.Write([]byte("y"))
	}
	f.Close()
	expectTick("db writes")
	expectNone("burst (should be coalesced)")

	// WAL created, written, deleted and recreated (checkpoint).
	wal := db + "-wal"
	os.WriteFile(wal, []byte("1"), 0o600)
	expectTick("wal create")
	os.Remove(wal)
	expectTick("wal remove")
	os.WriteFile(wal, []byte("2"), 0o600)
	expectTick("wal recreate")
	f, _ = os.OpenFile(wal, os.O_WRONLY|os.O_APPEND, 0)
	f.Write([]byte("3"))
	f.Close()
	expectTick("write to recreated wal")

	// Unrelated files are ignored.
	os.WriteFile(filepath.Join(dir, "other.txt"), []byte("z"), 0o600)
	expectNone("unrelated file")
}

func TestSafetyPoll(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "chat.db")
	os.WriteFile(db, []byte("x"), 0o600)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ticks := make(chan struct{}, 1)
	go Run(ctx, Options{DBPath: db, Poll: 50 * time.Millisecond}, ticks)
	select {
	case <-ticks:
	case <-time.After(2 * time.Second):
		t.Fatal("no poll tick")
	}
}
