package events

import (
	"encoding/json"
	"testing"
	"time"
)

func recv(t *testing.T, s *Subscription) Event {
	t.Helper()
	select {
	case ev := <-s.C():
		return ev
	case <-time.After(time.Second):
		t.Fatal("no event")
	}
	return Event{}
}

func TestPublishSubscribe(t *testing.T) {
	b := NewBus(10)
	all, _, _ := b.Subscribe(Filter{}, "", 10)
	msgs, _, _ := b.Subscribe(Filter{Types: []string{"message.*"}, ChatIDs: []string{"c1"}}, "", 10)

	b.Publish(ChatRead, "c1", map[string]string{"chat_id": "c1"})
	b.Publish(MessageReceived, "c2", map[string]int{"n": 1})
	ev, _ := b.Publish(MessageReceived, "c1", map[string]int{"n": 2})

	if got := recv(t, all); got.Type != ChatRead {
		t.Fatalf("got %s", got.Type)
	}
	recv(t, all)
	recv(t, all)
	got := recv(t, msgs)
	if got.ID != ev.ID || string(got.Data) != `{"n":2}` {
		t.Fatalf("filtered sub got %+v", got)
	}
	select {
	case extra := <-msgs.C():
		t.Fatalf("unexpected %+v", extra)
	default:
	}
	b2, _ := json.Marshal(got)
	if string(b2) == "" || got.CreatedAt.IsZero() {
		t.Fatal("bad envelope")
	}
}

func TestResume(t *testing.T) {
	b := NewBus(3)
	var ids []string
	for i := 0; i < 5; i++ {
		ev, _ := b.Publish(MessageSent, "c", i)
		ids = append(ids, ev.ID)
	}
	// Buffer holds events 3,4,5 (indexes 2..4).
	_, backlog, ok := b.Subscribe(Filter{}, ids[2], 10)
	if !ok || len(backlog) != 2 || backlog[0].ID != ids[3] {
		t.Fatalf("resume from buffered: ok=%v %v", ok, backlog)
	}
	// Last event before the oldest buffered one is still resumable (no gap).
	if _, backlog, ok := b.Subscribe(Filter{}, ids[1], 10); !ok || len(backlog) != 3 {
		t.Fatalf("resume at edge: ok=%v n=%d", ok, len(backlog))
	}
	// Too old: gap.
	if _, _, ok := b.Subscribe(Filter{}, ids[0], 10); ok {
		t.Fatal("expected reset for evicted event")
	}
	// Up to date.
	if _, backlog, ok := b.Subscribe(Filter{}, ids[4], 10); !ok || len(backlog) != 0 {
		t.Fatal("expected clean resume at head")
	}
	// Other process / garbage / future.
	for _, id := range []string{"deadbeef-3", "garbage", b.bootID + "-99"} {
		if _, _, ok := b.Subscribe(Filter{}, id, 10); ok {
			t.Fatalf("%s: expected reset", id)
		}
	}
}

func TestSlowConsumerDropped(t *testing.T) {
	b := NewBus(10)
	s, _, _ := b.Subscribe(Filter{}, "", 1)
	b.Publish(MessageSent, "", 1)
	b.Publish(MessageSent, "", 2) // overflows
	select {
	case <-s.Done():
	case <-time.After(time.Second):
		t.Fatal("slow subscriber not dropped")
	}
	if s.Err() != ErrSlowConsumer {
		t.Fatalf("err = %v", s.Err())
	}
	b.Publish(MessageSent, "", 3) // must not panic or block
	s.Close()                     // idempotent
}

func TestValidateTypes(t *testing.T) {
	if err := ValidateTypes([]string{"message.*", "chat.read", "*"}); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"message", "nope.*", "chat.unread"} {
		if ValidateTypes([]string{bad}) == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}
