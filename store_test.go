package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore("sqlite", filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestModeration(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	a, _ := s.Create(ctx, "a", "first", "r1", "")
	b, _ := s.Create(ctx, "b", "second", "r2", "")
	c, _ := s.Create(ctx, "c", "third", "r1", "")

	yes := true
	if _, err := s.SetFlags(ctx, uint64(a.ID), &yes, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetFlags(ctx, uint64(b.ID), nil, &yes); err != nil {
		t.Fatal(err)
	}

	wall, err := s.List(ctx, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(wall) != 2 || wall[0].ID != a.ID || wall[1].ID != c.ID {
		t.Fatalf("want pinned a then c (b hidden), got %+v", wall)
	}
	if n, _ := s.CountVisible(ctx); n != 2 {
		t.Fatalf("visible = %d, want 2", n)
	}

	hidden, total, err := s.AdminList(ctx, EntryFilter{Status: "hidden", Limit: 10})
	if err != nil || total != 1 || hidden[0].ID != b.ID {
		t.Fatalf("hidden filter: total=%d err=%v", total, err)
	}
	found, total, _ := s.AdminList(ctx, EntryFilter{Query: "THIRD", Limit: 10})
	if total != 1 || found[0].ID != c.ID {
		t.Fatalf("search should be case-insensitive, got total=%d", total)
	}

	st, err := s.Stats(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	if st.Total != 3 || st.Hidden != 1 || st.Pinned != 1 || st.Today != 3 || len(st.Daily) != 7 {
		t.Fatalf("unexpected stats %+v", st)
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if st, err := s.Settings(ctx); err != nil || st.SigningPaused || st.Banner != "" {
		t.Fatalf("defaults: %+v %v", st, err)
	}
	want := Settings{SigningPaused: true, Banner: "hello"}
	if err := s.SaveSettings(ctx, want); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveSettings(ctx, want); err != nil {
		t.Fatal("saving twice should upsert:", err)
	}
	if got, _ := s.Settings(ctx); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestCleanup(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	old, _ := s.Create(ctx, "old", "old", "r", "")
	pinned, _ := s.Create(ctx, "pin", "pinned", "r", "")
	gone, _ := s.Create(ctx, "gone", "deleted", "r", "")
	past := time.Now().Add(-48 * time.Hour)
	s.db.Model(&Entry{}).Where("id IN ?", []uint{old.ID, pinned.ID}).Update("created_at", past)
	yes := true
	_, _ = s.SetFlags(ctx, uint64(pinned.ID), &yes, nil)
	_, _ = s.Delete(ctx, uint64(gone.ID))

	if n, _ := s.Expire(ctx, time.Now().Add(-24*time.Hour), true); n != 1 {
		t.Fatalf("dry-run expire = %d, want 1 (pinned is kept)", n)
	}
	if n, _ := s.Expire(ctx, time.Now().Add(-24*time.Hour), false); n != 1 {
		t.Fatalf("expire = %d, want 1", n)
	}
	if n, _ := s.Purge(ctx, time.Now().Add(time.Minute), false); n != 2 {
		t.Fatalf("purge = %d, want 2 (expired + deleted)", n)
	}
	if n, _ := s.Count(ctx); n != 1 {
		t.Fatalf("remaining = %d, want 1", n)
	}
}

func TestIPIsAdminOnly(t *testing.T) {
	e := Entry{ID: 1, Name: "a", Message: "m", IP: "203.0.113.7"}
	pub, _ := json.Marshal(e)
	if strings.Contains(string(pub), "203.0.113.7") {
		t.Fatalf("public JSON leaks IP: %s", pub)
	}
	adm, _ := json.Marshal(toAdmin(e))
	if !strings.Contains(string(adm), `"ip":"203.0.113.7"`) || !strings.Contains(string(adm), `"name":"a"`) {
		t.Fatalf("admin JSON missing fields: %s", adm)
	}
}
