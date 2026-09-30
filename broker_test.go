package main

import (
	"testing"
	"time"
)

func TestLocalBroker(t *testing.T) {
	b := NewBroker("host-a", "1.0.0")
	ch := b.Subscribe()
	defer b.Unsubscribe(ch)

	if ev := <-ch; ev.Type != "presence" || ev.Online != 1 {
		t.Fatalf("want presence=1 on subscribe, got %+v", ev)
	}

	b.Publish(Event{Type: "deleted", ID: 7})
	select {
	case ev := <-ch:
		if ev.Type != "deleted" || ev.ID != 7 {
			t.Fatalf("unexpected event %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("published event not delivered")
	}

	if r := b.Replicas(); len(r) != 1 || r[0].Host != "host-a" || r[0].Online != 1 {
		t.Fatalf("unexpected replicas %+v", r)
	}
	if b.Mode() != "local" {
		t.Fatalf("mode = %s", b.Mode())
	}
}
