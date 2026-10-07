package main

import (
	"net/http"
	"strings"
	"time"

	"github.com/jkaninda/okapi"
)

// Visitor identity. Until now the only cookie was the reaction de-dupe token:
// accepted blindly, never checked against anything. The wall now issues a
// first-party identity cookie and keeps the matching row in the database, so
// the backend can tell a browser it has seen before from one it has not.
//
// The flow, on every API request (see identifyVisitor):
//
//  1. no cookie / malformed id     → mint a new UUID, persist it, set the cookie
//  2. cookie with a known id       → the visitor is recognised (last_seen stamped)
//  3. cookie with an unknown id    → the database was reset, the identity was
//     pruned by the cleanup job, or the value was forged: discard it and
//     generate a fresh identity as in (1)
//
// Identities are anonymous (a random UUID), recognised — never authenticated.

const (
	visitorCookie    = "gb_visitor"
	visitorCookieTTL = 365 * 24 * time.Hour
	// visitorFreshWindow is how long an identity counts as "visiting now".
	visitorFreshWindow = 5 * time.Minute
)

// visitorCtxKey is where identifyVisitor stashes the resolved *Visitor.
const visitorCtxKey = "gb.visitor"

// identifyVisitor is middleware that resolves (or creates) the caller's
// visitor identity for every API request and stores it on the context; see
// the package comment above for the flow.
func (h *Handler) identifyVisitor(c *okapi.Context) error {
	visitor, fresh, err := h.resolveVisitor(c)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, okapi.M{"error": "could not resolve visitor identity"})
	}
	if fresh {
		h.setVisitorCookie(c, visitor.ID)
	}
	c.Set(visitorCtxKey, &visitor)
	return c.Next()
}

// resolveVisitor returns the caller's identity; fresh reports whether a new
// visitor row was created and the cookie must be (re)set.
func (h *Handler) resolveVisitor(c *okapi.Context) (visitor Visitor, fresh bool, err error) {
	ctx := c.Request().Context()
	if id, cerr := c.Cookie(visitorCookie); cerr == nil && ValidVisitorID(id) {
		visitor, found, err := h.store.FindVisitor(ctx, id)
		if err != nil {
			return Visitor{}, false, err
		}
		if found {
			// Refresh the cookie's lifetime while the visitor stays active, and
			// stamp the visit so presence and pruning have something to work with.
			h.setVisitorCookie(c, id)
			h.store.TouchVisitor(ctx, id, time.Now().UTC())
			return visitor, false, nil
		}
		// Known shape, unknown id: fall through and mint a new identity.
	}
	visitor, err = h.store.CreateVisitor(ctx, NewVisitorID())
	if err != nil {
		return Visitor{}, false, err
	}
	return visitor, true, nil
}

// setVisitorCookie writes (or refreshes) the identity cookie.
func (h *Handler) setVisitorCookie(c *okapi.Context, id string) {
	r := c.Request()
	http.SetCookie(c.ResponseWriter(), &http.Cookie{
		Name:     visitorCookie,
		Value:    id,
		Path:     "/",
		MaxAge:   int(visitorCookieTTL.Seconds()),
		HttpOnly: true,
		Secure:   r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
		SameSite: http.SameSiteLaxMode,
	})
}

// visitorFromContext returns the identity resolved by the middleware; the
// zero value means "unidentified" (e.g. a request that bypassed the group).
func visitorFromContext(c *okapi.Context) *Visitor {
	if v, ok := c.Get(visitorCtxKey); ok {
		if visitor, ok := v.(*Visitor); ok {
			return visitor
		}
	}
	return &Visitor{}
}

// VisitorMe lets the UI learn its own identity: whether the cookie was just
// (re)generated and the remembered name to pre-fill the form with.
func (h *Handler) VisitorMe(c *okapi.Context) error {
	v := visitorFromContext(c)
	return c.OK(okapi.M{
		"id":         v.ID,
		"name":       v.Name,
		"first_seen": v.FirstSeen.UTC().Format(time.RFC3339),
		"signatures": v.Signatures,
	})
}

// visitorName returns the name under which the visitor last signed, so the
// server can attach it to an unsigned signature.
func visitorName(v *Visitor) string {
	if v == nil {
		return ""
	}
	return strings.TrimSpace(v.Name)
}
