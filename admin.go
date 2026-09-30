package main

import (
	"encoding/csv"
	"errors"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/jkaninda/okapi"
	"gorm.io/gorm"
)

const maxBannerLen = 200

type loginRequest struct {
	Token string `json:"token"`
}

// adminEntry exposes the fields public responses omit.
type adminEntry struct {
	Entry
	IP string `json:"ip,omitempty"`
}

func toAdmin(e Entry) adminEntry { return adminEntry{Entry: e, IP: e.IP} }

type flagsRequest struct {
	Pinned *bool `json:"pinned"`
	Hidden *bool `json:"hidden"`
}

// AdminSession reports whether the console is enabled and the caller signed in.
func (h *Handler) AdminSession(c *okapi.Context) error {
	return c.OK(okapi.M{
		"enabled":       h.auth.Enabled(),
		"authenticated": h.auth.Authenticated(c),
	})
}

// AdminLogin exchanges ADMIN_TOKEN for a session cookie.
func (h *Handler) AdminLogin(c *okapi.Context) error {
	if !h.auth.Enabled() {
		return c.JSON(http.StatusNotFound, okapi.M{"error": "admin console is disabled (set ADMIN_TOKEN)"})
	}
	var req loginRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, okapi.M{"error": "invalid JSON body"})
	}
	if !h.auth.CheckToken(strings.TrimSpace(req.Token)) {
		// Slows down guessing without needing shared rate-limit state.
		time.Sleep(700 * time.Millisecond)
		return c.JSON(http.StatusUnauthorized, okapi.M{"error": "invalid admin token"})
	}
	h.auth.SetSession(c, h.auth.Issue(time.Now()))
	return c.OK(okapi.M{"authenticated": true})
}

// AdminLogout clears the session cookie.
func (h *Handler) AdminLogout(c *okapi.Context) error {
	h.auth.SetSession(c, "")
	return c.NoContent()
}

// AdminOverview returns wall statistics plus runtime details of the replica
// that answered, which is what makes scaling visible in the console.
func (h *Handler) AdminOverview(c *okapi.Context) error {
	ctx := c.Request().Context()
	stats, err := h.store.Stats(ctx, 14)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, okapi.M{"error": "could not compute stats"})
	}
	settings, _ := h.store.Settings(ctx)

	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	return c.OK(okapi.M{
		"stats":    stats,
		"settings": settings,
		"online":   h.broker.Online(),
		"replicas": h.broker.Replicas(),
		"debug":    h.debug,
		"runtime": okapi.M{
			"app":        h.appName,
			"version":    h.version,
			"host":       h.host,
			"db":         dbDisplayName(h.dbDriver),
			"broker":     h.broker.Mode(),
			"redis":      h.broker.RedisStatus(ctx),
			"healthy":    !h.unhealthy.Load(),
			"started_at": h.started.UTC().Format(time.RFC3339),
			"uptime_s":   int64(time.Since(h.started).Seconds()),
			"go":         runtime.Version(),
			"cpus":       runtime.NumCPU(),
			"goroutines": runtime.NumGoroutine(),
			"heap_mb":    float64(mem.HeapAlloc) / (1 << 20),
			"sys_mb":     float64(mem.Sys) / (1 << 20),
		},
	})
}

// AdminListEntries lists every entry, hidden ones included, with filters.
func (h *Handler) AdminListEntries(c *okapi.Context) error {
	limit := atoiDefault(c.Query("limit"), 25)
	if limit <= 0 || limit > 200 {
		limit = 25
	}
	offset := max(atoiDefault(c.Query("offset"), 0), 0)

	entries, total, err := h.store.AdminList(c.Request().Context(), EntryFilter{
		Status: c.Query("status"),
		Query:  c.Query("q"),
		Limit:  limit,
		Offset: offset,
	})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, okapi.M{"error": "could not load entries"})
	}
	out := make([]adminEntry, len(entries))
	for i, e := range entries {
		out[i] = toAdmin(e)
	}
	return c.OK(okapi.M{"entries": out, "total": total, "limit": limit, "offset": offset})
}

// AdminUpdateEntry pins/unpins or hides/unhides an entry and broadcasts it.
func (h *Handler) AdminUpdateEntry(c *okapi.Context) error {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		return c.JSON(http.StatusBadRequest, okapi.M{"error": "invalid id"})
	}
	var req flagsRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, okapi.M{"error": "invalid JSON body"})
	}
	entry, err := h.store.SetFlags(c.Request().Context(), id, req.Pinned, req.Hidden)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return c.JSON(http.StatusNotFound, okapi.M{"error": "entry not found"})
	}
	if err != nil {
		return c.JSON(http.StatusInternalServerError, okapi.M{"error": "could not update entry"})
	}
	h.broker.Publish(Event{Type: "updated", Entry: &entry})
	return c.OK(toAdmin(entry))
}

// AdminSettings returns the shared wall settings.
func (h *Handler) AdminSettings(c *okapi.Context) error {
	st, err := h.store.Settings(c.Request().Context())
	if err != nil {
		return c.JSON(http.StatusInternalServerError, okapi.M{"error": "could not load settings"})
	}
	return c.OK(st)
}

// AdminSaveSettings persists the wall settings and broadcasts them so every
// open tab on every replica reacts immediately.
func (h *Handler) AdminSaveSettings(c *okapi.Context) error {
	var st Settings
	if err := c.Bind(&st); err != nil {
		return c.JSON(http.StatusBadRequest, okapi.M{"error": "invalid JSON body"})
	}
	st.Banner = strings.TrimSpace(st.Banner)
	if len(st.Banner) > maxBannerLen {
		return c.JSON(http.StatusBadRequest, okapi.M{"error": "banner is too long (max 200 characters)"})
	}
	if err := h.store.SaveSettings(c.Request().Context(), st); err != nil {
		return c.JSON(http.StatusInternalServerError, okapi.M{"error": "could not save settings"})
	}
	h.broker.Publish(Event{Type: "settings", Settings: &st})
	return c.OK(st)
}

// AdminExport streams every entry as CSV.
func (h *Handler) AdminExport(c *okapi.Context) error {
	entries, err := h.store.All(c.Request().Context())
	if err != nil {
		return c.JSON(http.StatusInternalServerError, okapi.M{"error": "could not export entries"})
	}
	w := c.ResponseWriter()
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="guestbook-`+time.Now().UTC().Format("20060102-150405")+`.csv"`)
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"id", "name", "message", "pinned", "hidden", "replica", "ip", "created_at"})
	for _, e := range entries {
		_ = cw.Write([]string{
			strconv.FormatUint(uint64(e.ID), 10),
			csvSafe(e.Name),
			csvSafe(e.Message),
			strconv.FormatBool(e.Pinned),
			strconv.FormatBool(e.Hidden),
			e.Replica,
			e.IP,
			e.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	cw.Flush()
	return cw.Error()
}

// csvSafe neutralises values a spreadsheet would evaluate as a formula.
func csvSafe(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}
