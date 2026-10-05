package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/jkaninda/okapi"
	"gorm.io/gorm"
)

// Reactions let visitors cheer a signature without an account: one tap adds,
// tapping again removes, and the tally streams to every open tab over SSE.
// A first-party cookie identifies the browser so a count can't be inflated by
// hammering the endpoint.

const (
	reactionCookie    = "gb_reactor"
	reactionCookieTTL = 365 * 24 * time.Hour
)

// reactorID returns the stable, anonymous id of this browser, setting the
// cookie on first sight.
func reactorID(c *okapi.Context) string {
	if id, err := c.Cookie(reactionCookie); err == nil && len(id) >= 8 && len(id) <= 64 {
		return id
	}
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return ""
	}
	id := hex.EncodeToString(buf)
	r := c.Request()
	http.SetCookie(c.ResponseWriter(), &http.Cookie{
		Name:     reactionCookie,
		Value:    id,
		Path:     "/",
		MaxAge:   int(reactionCookieTTL.Seconds()),
		HttpOnly: true,
		Secure:   r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
		SameSite: http.SameSiteLaxMode,
	})
	return id
}

// parseReactionRequest validates the {id} path param and the ?emoji= query.
func parseReactionRequest(c *okapi.Context) (uint64, string, error) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		return 0, "", errors.New("invalid id")
	}
	emoji := c.Query("emoji")
	if !validReaction(emoji) {
		return 0, "", errors.New("unknown emoji (want one of: heart, tada, thumbsup, smile)")
	}
	return id, emoji, nil
}

// React adds a reaction to an entry and broadcasts the new tally live.
func (h *Handler) React(c *okapi.Context) error {
	ctx := c.Request().Context()
	id, emoji, err := parseReactionRequest(c)
	if err != nil {
		return c.JSON(http.StatusBadRequest, okapi.M{"error": err.Error()})
	}
	if _, err := h.store.Get(ctx, id); errors.Is(err, gorm.ErrRecordNotFound) {
		return c.JSON(http.StatusNotFound, okapi.M{"error": "entry not found"})
	}
	who := reactorID(c)
	if who == "" {
		return c.JSON(http.StatusInternalServerError, okapi.M{"error": "could not identify the browser"})
	}
	count, err := h.store.React(ctx, id, emoji, who)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, okapi.M{"error": "could not save the reaction"})
	}
	h.broker.Publish(Event{Type: "reaction", ID: uint(id), Emoji: emoji, Count: count})
	return c.OK(okapi.M{"entry_id": id, "emoji": emoji, "count": count})
}

// Unreact removes the browser's reaction and broadcasts the new tally live.
func (h *Handler) Unreact(c *okapi.Context) error {
	ctx := c.Request().Context()
	id, emoji, err := parseReactionRequest(c)
	if err != nil {
		return c.JSON(http.StatusBadRequest, okapi.M{"error": err.Error()})
	}
	who := reactorID(c)
	if who == "" {
		return c.JSON(http.StatusInternalServerError, okapi.M{"error": "could not identify the browser"})
	}
	count, err := h.store.Unreact(ctx, id, emoji, who)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, okapi.M{"error": "could not remove the reaction"})
	}
	h.broker.Publish(Event{Type: "reaction", ID: uint(id), Emoji: emoji, Count: count})
	return c.OK(okapi.M{"entry_id": id, "emoji": emoji, "count": count})
}

// countRequests is middleware that tallies the HTTP requests this replica has
// answered since it started. The number lands in /healthz, /api/info and the
// admin replica table: it grows per replica under load-balancing and drops
// back to ~0 when a rollout replaces a container, which makes recreate and
// rolling updates visible from the outside.
func (h *Handler) countRequests(c *okapi.Context) error {
	h.requests.Add(1)
	return c.Next()
}
