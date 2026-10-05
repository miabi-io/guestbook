package main

import (
	"context"
	"testing"
)

func TestReactions(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	e, err := s.Create(ctx, "ada", "hello miabi", "r1", "")
	if err != nil {
		t.Fatal(err)
	}

	// Two visitors react, one of them twice (idempotent).
	for _, tc := range []struct {
		who, emoji string
		want       int
	}{
		{"alice", "heart", 1},
		{"bob", "heart", 2},
		{"bob", "heart", 2},
		{"alice", "tada", 1},
	} {
		got, err := s.React(ctx, uint64(e.ID), tc.emoji, tc.who)
		if err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Fatalf("React(%s, %s) = %d, want %d", tc.who, tc.emoji, got, tc.want)
		}
	}

	counts, err := s.ReactionCounts(ctx, uint64(e.ID))
	if err != nil {
		t.Fatal(err)
	}
	if counts["heart"] != 2 || counts["tada"] != 1 {
		t.Fatalf("counts = %+v, want heart=2 tada=1", counts)
	}

	// Unreacting twice is a no-op after the first removal.
	if got, _ := s.Unreact(ctx, uint64(e.ID), "heart", "bob"); got != 1 {
		t.Fatalf("unreact = %d, want 1", got)
	}
	if got, _ := s.Unreact(ctx, uint64(e.ID), "heart", "bob"); got != 1 {
		t.Fatalf("second unreact = %d, want 1", got)
	}

	// List and Get attach the tally; totals count every reaction.
	wall, err := s.List(ctx, 10, 0)
	if err != nil || len(wall) != 1 {
		t.Fatalf("list: %v (%d entries)", err, len(wall))
	}
	if wall[0].Reactions["heart"] != 1 || wall[0].Reactions["tada"] != 1 {
		t.Fatalf("list reactions = %+v", wall[0].Reactions)
	}
	one, err := s.Get(ctx, uint64(e.ID))
	if err != nil || one.Reactions["heart"] != 1 {
		t.Fatalf("get reactions = %+v, err=%v", one.Reactions, err)
	}
	if n, _ := s.ReactionTotals(ctx); n != 2 {
		t.Fatalf("totals = %d, want 2", n)
	}

	// Deleting a reaction must not touch the entry itself.
	if n, _ := s.Count(ctx); n != 1 {
		t.Fatalf("entries = %d, want 1", n)
	}
}

func TestReactionWhitelist(t *testing.T) {
	for _, emoji := range ReactionEmojis() {
		if !validReaction(emoji) {
			t.Fatalf("whitelist rejects its own emoji %q", emoji)
		}
	}
	if validReaction("poop") || validReaction("") {
		t.Fatal("whitelist accepted a non-whitelisted emoji")
	}
}

func TestSettingsTheme(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if st, _ := s.Settings(ctx); st.Theme != "" {
		t.Fatalf("default theme = %q, want empty (system)", st.Theme)
	}
	want := Settings{SigningPaused: false, Banner: "hi", Theme: "light"}
	if err := s.SaveSettings(ctx, want); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Settings(ctx); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}
