package main

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jkaninda/okapi"
)

// Handler holds the dependencies for the HTTP layer.
type Handler struct {
	store   *Store
	broker  *Broker
	appName string
	version string
	host    string
}

// serverTime is the shape of the live clock payload (v2 feature). It carries
// the version and host so a canary viewer can see exactly which build/replica
// answered.
func (h *Handler) serverTime() okapi.M {
	return okapi.M{
		"time":    time.Now().Format(time.RFC3339),
		"version": h.version,
		"host":    h.host,
	}
}

// createEntryRequest is the POST /api/entries body.
type createEntryRequest struct {
	Name    string `json:"name"`
	Message string `json:"message"`
}

const (
	maxNameLen    = 60
	maxMessageLen = 500
)

// Health reports liveness/readiness. Returns 503 when the database is
// unreachable so Miabi's health checks hold traffic until the DB is up.
func (h *Handler) Health(c *okapi.Context) error {
	if err := h.store.Ping(c.Request().Context()); err != nil {
		return c.JSON(http.StatusServiceUnavailable, okapi.M{
			"status":  "unavailable",
			"db":      "down",
			"version": h.version,
		})
	}
	return c.OK(okapi.M{"status": "ok", "db": "up", "app": h.appName, "version": h.version})
}

// Info returns app metadata for the UI: name, the serving version (useful for
// spotting which build answered during a canary rollout), and how many clients
// are currently connected.
func (h *Handler) Info(c *okapi.Context) error {
	return c.OK(okapi.M{
		"app":     h.appName,
		"version": h.version,
		"online":  h.broker.Online(),
		"host":    h.host,
	})
}

// Time returns the current server time (plus version + host). Handy for probing
// a canary rollout from the shell: `watch curl -s .../api/time`.
func (h *Handler) Time(c *okapi.Context) error {
	return c.OK(h.serverTime())
}

// ListEntries returns guestbook entries, newest first, with simple pagination.
// Query params: limit (default 100, max 200) and offset (default 0). The home
// page requests a small limit; the "All signatures" page pages through them.
func (h *Handler) ListEntries(c *okapi.Context) error {
	ctx := c.Request().Context()

	limit := atoiDefault(c.Query("limit"), 100)
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	offset := atoiDefault(c.Query("offset"), 0)
	if offset < 0 {
		offset = 0
	}

	entries, err := h.store.List(ctx, limit, offset)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, okapi.M{"error": "could not load entries"})
	}
	count, _ := h.store.Count(ctx)
	return c.OK(okapi.M{
		"entries": entries,
		"total":   count,
		"limit":   limit,
		"offset":  offset,
	})
}

// atoiDefault parses s as an int, returning def when empty or invalid.
func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

// CreateEntry adds a new signature to the wall and broadcasts it live.
func (h *Handler) CreateEntry(c *okapi.Context) error {
	var req createEntryRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, okapi.M{"error": "invalid JSON body"})
	}

	name := strings.TrimSpace(req.Name)
	message := strings.TrimSpace(req.Message)

	if name == "" {
		name = "Anonymous"
	}
	if len(name) > maxNameLen {
		name = name[:maxNameLen]
	}
	if message == "" {
		return c.JSON(http.StatusBadRequest, okapi.M{"error": "message is required"})
	}
	if len(message) > maxMessageLen {
		return c.JSON(http.StatusBadRequest, okapi.M{
			"error": "message is too long (max 500 characters)",
		})
	}

	entry, err := h.store.Create(c.Request().Context(), name, message)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, okapi.M{"error": "could not save entry"})
	}

	h.broker.Publish(Event{Type: "created", Entry: &entry})
	return c.Created(entry)
}

// DeleteEntry removes an entry by id and broadcasts the removal live.
func (h *Handler) DeleteEntry(c *okapi.Context) error {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		return c.JSON(http.StatusBadRequest, okapi.M{"error": "invalid id"})
	}
	ok, err := h.store.Delete(c.Request().Context(), id)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, okapi.M{"error": "could not delete entry"})
	}
	if !ok {
		return c.JSON(http.StatusNotFound, okapi.M{"error": "entry not found"})
	}

	h.broker.Publish(Event{Type: "deleted", ID: uint(id)})
	return c.NoContent()
}

// Stream is the SSE endpoint. Each client subscribes to the broker and receives
// live "created" / "deleted" / "presence" events. A "welcome" event is sent
// first so the client immediately knows the serving version and online count.
func (h *Handler) Stream(c *okapi.Context) error {
	ctx, cancel := context.WithCancel(c.Request().Context())
	defer cancel()

	subscription := h.broker.Subscribe()
	defer h.broker.Unsubscribe(subscription)

	msgs := make(chan okapi.Message, 32)
	go func() {
		defer close(msgs)

		welcome := okapi.Message{Event: "welcome", Data: okapi.M{
			"version": h.version,
			"online":  h.broker.Online(),
			"host":    h.host,
		}}
		select {
		case msgs <- welcome:
		case <-ctx.Done():
			return
		}

		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-subscription:
				if !ok {
					return
				}
				select {
				case msgs <- okapi.Message{Event: ev.Type, Data: ev}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	return c.SSEStreamWithOptions(ctx, msgs, &okapi.StreamOptions{
		Serializer:   &okapi.JSONSerializer{},
		PingInterval: 25 * time.Second,
	})
}
