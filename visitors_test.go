package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jkaninda/okapi"
)

func TestVisitorLifecycle(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if !ValidVisitorID(NewVisitorID()) {
		t.Fatal("a freshly minted id should validate")
	}
	for _, bad := range []string{"", "not-a-uuid", "550e8400-e29b-41d4-a716-44665544000z"} {
		if ValidVisitorID(bad) {
			t.Fatalf("invalid id accepted: %q", bad)
		}
	}

	// A cookie the database does not know about must be regenerated.
	if _, found, err := s.FindVisitor(ctx, NewVisitorID()); err != nil || found {
		t.Fatalf("unknown visitor: found=%v err=%v", found, err)
	}

	id := NewVisitorID()
	v, err := s.CreateVisitor(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if v.ID != id || v.Name != "" || v.Signatures != 0 {
		t.Fatalf("new visitor = %+v", v)
	}
	if _, found, err := s.FindVisitor(ctx, id); err != nil || !found {
		t.Fatalf("known visitor: found=%v err=%v", found, err)
	}

	s.VisitorSigned(ctx, id, "Ada")
	got, found, _ := s.FindVisitor(ctx, id)
	if !found || got.Name != "Ada" || got.Signatures != 1 {
		t.Fatalf("after signing: %+v", got)
	}

	// Freshness drives the "visiting now" number (2 minutes ago is still fresh).
	s.db.Model(&Visitor{}).Where("id = ?", id).Update("last_seen", time.Now().Add(-2*time.Minute))
	if n, _ := s.VisitorsSeenAfter(ctx, time.Now().Add(-visitorFreshWindow)); n != 1 {
		t.Fatalf("active visitors = %d, want 1", n)
	}
	stale := time.Now().Add(-48 * time.Hour)
	s.db.Model(&Visitor{}).Where("id = ?", id).Update("last_seen", stale)
	if n, _ := s.VisitorsSeenAfter(ctx, time.Now().Add(-visitorFreshWindow)); n != 0 {
		t.Fatalf("active visitors after going stale = %d, want 0", n)
	}

	// Pruning removes the identity and its reactions.
	e, _ := s.Create(ctx, "ada", "hello", "r1", "")
	if _, err := s.React(ctx, uint64(e.ID), "heart", id); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.PurgeVisitors(ctx, time.Now().Add(-24*time.Hour), true); n != 1 {
		t.Fatalf("dry-run prune = %d, want 1", n)
	}
	if n, _ := s.PurgeVisitors(ctx, time.Now().Add(-24*time.Hour), false); n != 1 {
		t.Fatalf("prune = %d, want 1", n)
	}
	if _, found, _ := s.FindVisitor(ctx, id); found {
		t.Fatal("pruned visitor still found")
	}
	if n, _ := s.ReactionTotals(ctx); n != 0 {
		t.Fatalf("reactions of the pruned visitor = %d, want 0", n)
	}
}

// TestVisitorCookieFlow exercises the middleware over HTTP: no cookie → one is
// issued and persisted; a known cookie is kept; a cookie the database no
// longer knows about is regenerated.
func TestVisitorCookieFlow(t *testing.T) {
	s := newTestStore(t)
	h := &Handler{store: s, broker: NewBroker("test", "dev", nil), auth: NewAdminAuth("")}
	app := okapi.New()
	api := app.Group("/api", h.identifyVisitor)
	api.Get("/me", h.VisitorMe)

	call := func(cookie string) (*httptest.ResponseRecorder, map[string]any) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
		if cookie != "" {
			req.Header.Set("Cookie", visitorCookie+"="+cookie)
		}
		app.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return rec, body
	}

	// First visit: the backend mints an identity, persists it and sets the cookie.
	rec, first := call("")
	issued := ""
	for _, c := range rec.Result().Cookies() {
		if c.Name == visitorCookie {
			issued = c.Value
		}
	}
	if issued == "" || first["id"] != issued {
		t.Fatalf("first visit: cookie %q, body %v", issued, first)
	}
	if _, found, _ := s.FindVisitor(context.Background(), issued); !found {
		t.Fatal("issued identity was not persisted")
	}

	// Second visit with the cookie: recognised, identity unchanged.
	_, second := call(issued)
	if second["id"] != issued {
		t.Fatalf("known cookie should keep its identity: %v vs %v", second["id"], issued)
	}

	// A cookie the database does not know (e.g. after a reset) is regenerated.
	rec, third := call(NewVisitorID())
	regenerated := third["id"].(string)
	if !ValidVisitorID(regenerated) || regenerated == issued {
		t.Fatalf("unknown cookie should be regenerated, got %v", third)
	}
	if !cookieSetTo(rec, visitorCookie, regenerated) {
		t.Fatalf("regenerated identity was not written back to the cookie: %v", rec.Header())
	}

	// A malformed cookie never reaches the database and gets a fresh identity.
	_, fourth := call("not-a-uuid")
	if !ValidVisitorID(fourth["id"].(string)) {
		t.Fatalf("malformed cookie should yield a fresh identity, got %v", fourth)
	}
}

func cookieSetTo(rec *httptest.ResponseRecorder, name, value string) bool {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c.Value == value
		}
	}
	return false
}

func TestReset(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	e, _ := s.Create(ctx, "ada", "hello", "r1", "")
	gone, _ := s.Create(ctx, "gone", "deleted", "r1", "")
	_, _ = s.Delete(ctx, uint64(gone.ID))
	who, _ := s.CreateVisitor(ctx, NewVisitorID())
	if _, err := s.React(ctx, uint64(e.ID), "tada", who.ID); err != nil {
		t.Fatal(err)
	}
	_ = s.SaveSettings(ctx, Settings{SigningPaused: true, Banner: "hi"})

	// Dry run reports but changes nothing.
	dry, err := s.Reset(ctx, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if dry.Entries != 2 || dry.Reactions != 1 || dry.Visitors != 1 || dry.Settings != 0 {
		t.Fatalf("dry run = %+v", dry)
	}
	if n, _ := s.Count(ctx); n != 1 {
		t.Fatalf("dry run removed entries: %d left", n)
	}

	// A soft reset keeps the settings, and soft-deleted rows are gone for good.
	stats, err := s.Reset(ctx, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Entries != 2 || stats.Reactions != 1 || stats.Visitors != 1 {
		t.Fatalf("reset = %+v", stats)
	}
	if n, _ := s.Count(ctx); n != 0 {
		t.Fatalf("entries after reset = %d, want 0", n)
	}
	if n, _ := s.ReactionTotals(ctx); n != 0 {
		t.Fatalf("reactions after reset = %d, want 0", n)
	}
	if n, _ := s.VisitorCount(ctx); n != 0 {
		t.Fatalf("visitors after reset = %d, want 0", n)
	}
	if n, _ := s.Purge(ctx, time.Now().Add(time.Hour), false); n != 0 {
		t.Fatalf("purged after reset = %d, want 0 (soft-deleted rows wiped too)", n)
	}
	if st, _ := s.Settings(ctx); !st.SigningPaused || st.Banner != "hi" {
		t.Fatalf("settings should survive a soft reset, got %+v", st)
	}

	// Hard reset drops the settings as well.
	if _, err := s.Reset(ctx, true, false); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.Settings(ctx); st.SigningPaused || st.Banner != "" {
		t.Fatalf("settings should be cleared by a hard reset, got %+v", st)
	}
}
