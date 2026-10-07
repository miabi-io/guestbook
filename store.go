package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/glebarez/sqlite" // pure-Go SQLite (no CGO — keeps the static build)
	"github.com/google/uuid"     // visitor identity ids (v4 UUIDs)
	"github.com/jkaninda/logger"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Entry is a single signature on the guestbook wall.
type Entry struct {
	ID        uint           `gorm:"primaryKey" json:"id"`
	Name      string         `gorm:"type:varchar(60);not null" json:"name"`
	Message   string         `gorm:"type:varchar(500);not null" json:"message"`
	Pinned    bool           `gorm:"not null;default:false;index" json:"pinned"`
	Hidden    bool           `gorm:"not null;default:false;index" json:"hidden"`
	Replica   string         `gorm:"type:varchar(128)" json:"replica,omitempty"`
	IP        string         `gorm:"type:varchar(64)" json:"-"`
	CreatedAt time.Time      `gorm:"index" json:"created_at"`
	UpdatedAt time.Time      `json:"-"`
	DeletedAt gorm.DeletedAt `gorm:"index" json:"-"`

	// Reactions carries the per-emoji tally (e.g. {"heart":3,"tada":1}); it is
	// filled by list endpoints, never stored on the entries table itself.
	Reactions map[string]int `gorm:"-" json:"reactions,omitempty"`
}

// Reaction is one emoji reaction to an entry. Visitors react and un-react
// without an account; the "who" is their visitor id (see Visitor), so the
// same browser cannot inflate the count.
type Reaction struct {
	EntryID   uint           `gorm:"primaryKey;autoIncrement:false" json:"-"`
	Emoji     string         `gorm:"primaryKey;type:varchar(16)" json:"-"`
	Who       string         `gorm:"primaryKey;type:varchar(64)" json:"-"`
	CreatedAt time.Time      `json:"-"`
	DeletedAt gorm.DeletedAt `gorm:"index" json:"-"`
}

// Visitor is a browser the app knows about. The identity cookie carries the
// UUID; the row in this table is what makes it valid — a cookie whose id is
// not in the database is discarded and regenerated. That keeps identities
// honest across database resets and lets stale identities be pruned by the
// cleanup job.
type Visitor struct {
	ID string `gorm:"primaryKey;type:varchar(36)" json:"id"`
	// Name is the last name the visitor signed with; the form pre-fills it.
	Name       string    `gorm:"type:varchar(60)" json:"name,omitempty"`
	FirstSeen  time.Time `json:"first_seen"`
	LastSeen   time.Time `gorm:"index" json:"last_seen"`
	Signatures int64     `gorm:"not null;default:0" json:"signatures"`
}

// IsZero reports whether v is an unidentified visitor (no valid cookie).
func (v *Visitor) IsZero() bool { return v == nil || v.ID == "" }

// Setting is a key/value row holding app-wide state shared by all replicas.
type Setting struct {
	Key       string `gorm:"primaryKey;type:varchar(64)"`
	Value     string `gorm:"type:text;not null"`
	UpdatedAt time.Time
}

// Settings is the moderator-controlled state of the wall.
type Settings struct {
	SigningPaused bool   `json:"signing_paused"`
	Banner        string `json:"banner"`
	// Theme is the wall appearance forced by a moderator: "" (or "system"),
	// "light" or "dark". Visitors can still override it locally.
	Theme string `json:"theme"`
}

// EntryFilter selects entries for the admin console.
type EntryFilter struct {
	Status string // "", "visible", "hidden" or "pinned"
	Query  string
	Limit  int
	Offset int
}

// DayCount is the number of signatures created on a given UTC day.
type DayCount struct {
	Day   string `json:"day"`
	Count int    `json:"count"`
}

// Emojis a visitor can react with; also the whitelist for the API.
var reactionEmojis = []string{"heart", "tada", "thumbsup", "smile"}

// ReactionEmojis returns the whitelist, for the info endpoint.
func ReactionEmojis() []string { return reactionEmojis }

// validReaction reports whether emoji is on the whitelist.
func validReaction(emoji string) bool {
	for _, e := range reactionEmojis {
		if e == emoji {
			return true
		}
	}
	return false
}

// Stats summarises the wall for the admin overview.
type Stats struct {
	Total     int64      `json:"total"`
	Visible   int64      `json:"visible"`
	Hidden    int64      `json:"hidden"`
	Pinned    int64      `json:"pinned"`
	Today     int64      `json:"today"`
	Reactions int64      `json:"reactions"`
	Visitors  int64      `json:"visitors"`
	Daily     []DayCount `json:"daily"`
}

const settingsKey = "settings"

type Store struct {
	db *gorm.DB
}

// NewStore opens a GORM connection using the given driver ("postgres" or
// "sqlite") and DSN. It does not block on the database being reachable — call
// Ping (e.g. from the health check) for that.
func NewStore(driver, dsn string) (*Store, error) {
	var dialector gorm.Dialector
	switch driver {
	case "sqlite", "sqlite3":
		// Create the parent directory (e.g. ./data or /data) so the SQLite
		// file can be written on first run.
		if err := ensureSQLiteDir(dsn); err != nil {
			return nil, fmt.Errorf("create sqlite directory: %w", err)
		}
		dialector = sqlite.Open(dsn)
	case "postgres", "":
		dialector = postgres.Open(dsn)
	default:
		return nil, fmt.Errorf("unsupported DB_DRIVER %q (want postgres or sqlite)", driver)
	}

	db, err := gorm.Open(dialector, &gorm.Config{})
	if err != nil {
		return nil, err
	}
	logger.Info("database connected", "driver", driver)
	return &Store{db: db}, nil
}

// ensureSQLiteDir creates the directory that will hold the SQLite file. It is a
// no-op for in-memory databases and for a bare filename in the current dir.
func ensureSQLiteDir(dsn string) error {
	// Strip a "file:" prefix and any "?query" so we get the plain file path.
	path := strings.TrimPrefix(dsn, "file:")
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	if path == "" || strings.Contains(path, ":memory:") {
		return nil
	}
	dir := filepath.Dir(path)
	if dir == "" || dir == "." {
		return nil
	}
	return os.MkdirAll(dir, 0o755)
}

// Close releases the underlying connection pool.
func (s *Store) Close() error {
	sqlDB, err := s.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

// Ping verifies the database is reachable. Used by the health endpoint so
// Miabi can gate traffic until the app is actually ready.
func (s *Store) Ping(ctx context.Context) error {
	sqlDB, err := s.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.PingContext(ctx)
}

// Migrate applies the schema under the same lock as seeding, since
// concurrent AutoMigrate calls race on Postgres catalog inserts.
func (s *Store) Migrate() error {
	return s.Exclusive(context.Background(), func(tx *Store) error {
		return tx.db.AutoMigrate(&Entry{}, &Setting{}, &Reaction{}, &Visitor{})
	})
}

// List returns the public wall: visible entries, pinned first, then newest.
func (s *Store) List(ctx context.Context, limit, offset int) ([]Entry, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	var entries []Entry
	err := s.db.WithContext(ctx).
		Where("hidden = ?", false).
		Order("pinned DESC").
		Order("created_at DESC").
		Limit(limit).
		Offset(offset).
		Find(&entries).Error
	if err != nil {
		return nil, err
	}
	return entries, s.loadReactions(ctx, entries)
}

// Create inserts a new entry and returns it with its generated id/timestamp.
func (s *Store) Create(ctx context.Context, name, message, replica, ip string) (Entry, error) {
	e := Entry{Name: name, Message: message, Replica: replica, IP: ip}
	err := s.db.WithContext(ctx).Create(&e).Error
	return e, err
}

// loadReactions fills the Reactions map of every entry in one grouped query.
func (s *Store) loadReactions(ctx context.Context, entries []Entry) error {
	ids := make([]uint, len(entries))
	for i, e := range entries {
		ids[i] = e.ID
		entries[i].Reactions = map[string]int{}
	}
	if len(ids) == 0 {
		return nil
	}
	var rows []struct {
		EntryID uint
		Emoji   string
		N       int
	}
	if err := s.db.WithContext(ctx).Model(&Reaction{}).
		Select("entry_id, emoji, count(*) AS n").
		Where("entry_id IN ?", ids).
		Group("entry_id, emoji").
		Scan(&rows).Error; err != nil {
		return err
	}
	byID := make(map[uint]int, len(entries))
	for i, e := range entries {
		byID[e.ID] = i
	}
	for _, r := range rows {
		if i, ok := byID[r.EntryID]; ok {
			entries[i].Reactions[r.Emoji] = r.N
		}
	}
	return nil
}

// React registers a reaction and returns the emoji's new total for the entry.
// It is idempotent for a given (entry, emoji, who): reacting twice is a no-op.
func (s *Store) React(ctx context.Context, entryID uint64, emoji, who string) (int, error) {
	err := s.db.WithContext(ctx).
		Clauses(clause.OnConflict{DoNothing: true}).
		Create(&Reaction{EntryID: uint(entryID), Emoji: emoji, Who: who}).Error
	if err != nil {
		return 0, err
	}
	return s.reactionTotal(ctx, entryID, emoji)
}

// Unreact removes a reaction (idempotent) and returns the emoji's new total.
func (s *Store) Unreact(ctx context.Context, entryID uint64, emoji, who string) (int, error) {
	err := s.db.WithContext(ctx).
		Where("entry_id = ? AND emoji = ? AND who = ?", entryID, emoji, who).
		Delete(&Reaction{}).Error
	if err != nil {
		return 0, err
	}
	return s.reactionTotal(ctx, entryID, emoji)
}

func (s *Store) reactionTotal(ctx context.Context, entryID uint64, emoji string) (int, error) {
	var n int64
	err := s.db.WithContext(ctx).Model(&Reaction{}).
		Where("entry_id = ? AND emoji = ?", entryID, emoji).
		Count(&n).Error
	return int(n), err
}

// ReactionCounts returns the per-emoji tally for one entry.
func (s *Store) ReactionCounts(ctx context.Context, entryID uint64) (map[string]int, error) {
	out := map[string]int{}
	var rows []struct {
		Emoji string
		N     int
	}
	err := s.db.WithContext(ctx).Model(&Reaction{}).
		Select("emoji, count(*) AS n").
		Where("entry_id = ?", entryID).
		Group("emoji").
		Scan(&rows).Error
	for _, r := range rows {
		out[r.Emoji] = r.N
	}
	return out, err
}

// ReactionTotals returns the total number of reactions across the wall.
func (s *Store) ReactionTotals(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.WithContext(ctx).Model(&Reaction{}).Count(&n).Error
	return n, err
}

// FindVisitor loads a visitor by id. Returns false when the id is unknown —
// the caller then discards the cookie and issues a fresh identity.
func (s *Store) FindVisitor(ctx context.Context, id string) (Visitor, bool, error) {
	var v Visitor
	err := s.db.WithContext(ctx).First(&v, "id = ?", id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return v, false, nil
	}
	return v, err == nil, err
}

// CreateVisitor inserts a new identity with the given UUID.
func (s *Store) CreateVisitor(ctx context.Context, id string) (Visitor, error) {
	now := time.Now().UTC()
	v := Visitor{ID: id, FirstSeen: now, LastSeen: now}
	err := s.db.WithContext(ctx).Create(&v).Error
	return v, err
}

// TouchVisitor stamps the visitor's last-seen time.
func (s *Store) TouchVisitor(ctx context.Context, id string, at time.Time) {
	s.db.WithContext(ctx).Model(&Visitor{}).Where("id = ?", id).Update("last_seen", at)
}

// VisitorSigned records a new signature: it bumps the counter and remembers
// the name, so the form can be pre-filled on the next visit.
func (s *Store) VisitorSigned(ctx context.Context, id, name string) {
	s.db.WithContext(ctx).Model(&Visitor{}).
		Where("id = ?", id).
		Updates(map[string]any{"name": name, "signatures": gorm.Expr("signatures + 1")})
}

// VisitorCount returns the number of identities the app has issued.
func (s *Store) VisitorCount(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.WithContext(ctx).Model(&Visitor{}).Count(&n).Error
	return n, err
}

// VisitorsSeenAfter counts visitors active since the given time — the
// "currently visiting" number, distinct from connected SSE clients.
func (s *Store) VisitorsSeenAfter(ctx context.Context, since time.Time) (int64, error) {
	var n int64
	err := s.db.WithContext(ctx).Model(&Visitor{}).Where("last_seen >= ?", since).Count(&n).Error
	return n, err
}

// AdminVisitors lists the most recently active visitors.
func (s *Store) AdminVisitors(ctx context.Context, limit int) ([]Visitor, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var visitors []Visitor
	err := s.db.WithContext(ctx).Order("last_seen DESC").Limit(limit).Find(&visitors).Error
	return visitors, err
}

// PurgeVisitors permanently removes visitors idle since cutoff, together with
// their reactions, so a later signup with a fresh cookie starts clean. It
// returns the number of visitors removed.
func (s *Store) PurgeVisitors(ctx context.Context, cutoff time.Time, dryRun bool) (int64, error) {
	scope := s.db.WithContext(ctx).Where("last_seen < ?", cutoff)
	if dryRun {
		var n int64
		err := scope.Model(&Visitor{}).Count(&n).Error
		return n, err
	}
	var ids []string
	if err := scope.Model(&Visitor{}).Pluck("id", &ids).Error; err != nil || len(ids) == 0 {
		return 0, err
	}
	if err := s.db.WithContext(ctx).Where("who IN ?", ids).Delete(&Reaction{}).Error; err != nil {
		return 0, err
	}
	res := s.db.WithContext(ctx).Where("id IN ?", ids).Delete(&Visitor{})
	return res.RowsAffected, res.Error
}

// ResetStats reports what Reset removed (or would remove, when DryRun).
type ResetStats struct {
	Entries   int64 `json:"entries"`
	Reactions int64 `json:"reactions"`
	Visitors  int64 `json:"visitors"`
	Settings  int64 `json:"settings"`
	DryRun    bool  `json:"dry_run"`
}

// Reset wipes the wall so a demo can start from scratch (typically run as a
// one-off Job, followed by a restart to re-seed). Entries and reactions
// always go; settings are kept unless hard is set. With dryRun it only
// reports what would be removed.
func (s *Store) Reset(ctx context.Context, hard, dryRun bool) (ResetStats, error) {
	var out ResetStats
	out.DryRun = dryRun
	err := s.Exclusive(ctx, func(tx *Store) error {
		count := func(model any, dst *int64, unscoped bool) error {
			q := tx.db.WithContext(ctx).Model(model)
			if unscoped {
				q = q.Unscoped()
			}
			return q.Count(dst).Error
		}
		// Soft-deleted entries are gone for good too: after a reset there is
		// nothing left to purge.
		if err := count(&Entry{}, &out.Entries, true); err != nil {
			return err
		}
		if err := count(&Reaction{}, &out.Reactions, false); err != nil {
			return err
		}
		if err := count(&Visitor{}, &out.Visitors, false); err != nil {
			return err
		}
		if hard {
			if err := count(&Setting{}, &out.Settings, false); err != nil {
				return err
			}
		}
		if dryRun {
			return nil
		}
		wipe := func(model any) error {
			// "WHERE 1=1" satisfies GORM's global-delete guard.
			return tx.db.WithContext(ctx).Unscoped().Where("1 = 1").Delete(model).Error
		}
		for _, model := range []any{&Entry{}, &Reaction{}, &Visitor{}} {
			if err := wipe(model); err != nil {
				return err
			}
		}
		if hard {
			return wipe(&Setting{})
		}
		return nil
	})
	return out, err
}

// NewVisitorID returns a random UUID for a fresh identity.
func NewVisitorID() string { return uuid.NewString() }

// ValidVisitorID reports whether id is a syntactically valid visitor id.
// A malformed cookie is discarded without hitting the database.
func ValidVisitorID(id string) bool {
	return len(id) == 36 && uuid.Validate(id) == nil
}

// Delete soft-deletes an entry by id. Returns true when a row was affected.
func (s *Store) Delete(ctx context.Context, id uint64) (bool, error) {
	res := s.db.WithContext(ctx).Delete(&Entry{}, id)
	return res.RowsAffected > 0, res.Error
}

// schemaLockID is an arbitrary Postgres advisory-lock key.
const schemaLockID = 7_406_381

// Exclusive runs fn in a transaction holding a Postgres advisory lock, so
// replicas booting together migrate and seed one at a time.
func (s *Store) Exclusive(ctx context.Context, fn func(tx *Store) error) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if tx.Name() == "postgres" {
			if err := tx.Exec("SELECT pg_advisory_xact_lock(?)", schemaLockID).Error; err != nil {
				return err
			}
		}
		return fn(&Store{db: tx})
	})
}

// Count returns the total number of (non-deleted) entries, hidden included.
func (s *Store) Count(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.WithContext(ctx).Model(&Entry{}).Count(&n).Error
	return n, err
}

// CountVisible returns the number of entries shown on the public wall.
func (s *Store) CountVisible(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.WithContext(ctx).Model(&Entry{}).Where("hidden = ?", false).Count(&n).Error
	return n, err
}

// Get returns a single entry by id, with its reaction counts.
func (s *Store) Get(ctx context.Context, id uint64) (Entry, error) {
	var e Entry
	err := s.db.WithContext(ctx).First(&e, id).Error
	if err != nil {
		return e, err
	}
	e.Reactions, err = s.ReactionCounts(ctx, id)
	return e, err
}

// AdminList returns entries matching f, newest first, and the match count.
func (s *Store) AdminList(ctx context.Context, f EntryFilter) ([]Entry, int64, error) {
	q := s.db.WithContext(ctx).Model(&Entry{})
	switch f.Status {
	case "visible":
		q = q.Where("hidden = ?", false)
	case "hidden":
		q = q.Where("hidden = ?", true)
	case "pinned":
		q = q.Where("pinned = ?", true)
	}
	if term := strings.TrimSpace(f.Query); term != "" {
		like := "%" + strings.ToLower(term) + "%"
		q = q.Where("LOWER(name) LIKE ? OR LOWER(message) LIKE ? OR ip LIKE ?", like, like, like)
	}

	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var entries []Entry
	err := q.Order("created_at DESC").Limit(f.Limit).Offset(f.Offset).Find(&entries).Error
	if err != nil {
		return nil, 0, err
	}
	return entries, total, s.loadReactions(ctx, entries)
}

// SetFlags updates the moderation flags that are non-nil and returns the entry.
func (s *Store) SetFlags(ctx context.Context, id uint64, pinned, hidden *bool) (Entry, error) {
	updates := map[string]any{}
	if pinned != nil {
		updates["pinned"] = *pinned
	}
	if hidden != nil {
		updates["hidden"] = *hidden
	}
	e, err := s.Get(ctx, id)
	if err != nil || len(updates) == 0 {
		return e, err
	}
	if err := s.db.WithContext(ctx).Model(&e).Updates(updates).Error; err != nil {
		return e, err
	}
	return s.Get(ctx, id)
}

// All returns every entry, oldest first, for export.
func (s *Store) All(ctx context.Context) ([]Entry, error) {
	var entries []Entry
	err := s.db.WithContext(ctx).Order("created_at ASC").Find(&entries).Error
	return entries, err
}

// Stats computes the admin overview, including per-day counts for the last
// `days` days. Bucketing happens in Go so it works the same on both drivers.
func (s *Store) Stats(ctx context.Context, days int) (Stats, error) {
	var st Stats
	db := s.db.WithContext(ctx).Model(&Entry{})
	if err := db.Count(&st.Total).Error; err != nil {
		return st, err
	}
	s.db.WithContext(ctx).Model(&Entry{}).Where("hidden = ?", true).Count(&st.Hidden)
	s.db.WithContext(ctx).Model(&Entry{}).Where("pinned = ?", true).Count(&st.Pinned)
	s.db.WithContext(ctx).Model(&Reaction{}).Count(&st.Reactions)
	s.db.WithContext(ctx).Model(&Visitor{}).Count(&st.Visitors)
	st.Visible = st.Total - st.Hidden

	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	since := today.AddDate(0, 0, -(days - 1))

	var stamps []time.Time
	if err := s.db.WithContext(ctx).Model(&Entry{}).
		Where("created_at >= ?", since).
		Pluck("created_at", &stamps).Error; err != nil {
		return st, err
	}
	buckets := make(map[string]int, days)
	for _, t := range stamps {
		buckets[t.UTC().Format("2006-01-02")]++
	}
	for d := since; !d.After(today); d = d.AddDate(0, 0, 1) {
		key := d.Format("2006-01-02")
		st.Daily = append(st.Daily, DayCount{Day: key, Count: buckets[key]})
	}
	st.Today = int64(buckets[today.Format("2006-01-02")])
	return st, nil
}

// Settings loads the shared wall settings, returning defaults when unset.
func (s *Store) Settings(ctx context.Context) (Settings, error) {
	var row Setting
	var st Settings
	err := s.db.WithContext(ctx).Where(&Setting{Key: settingsKey}).Limit(1).Find(&row).Error
	if err != nil || row.Value == "" {
		return st, err
	}
	err = json.Unmarshal([]byte(row.Value), &st)
	return st, err
}

// SaveSettings persists the shared wall settings.
func (s *Store) SaveSettings(ctx context.Context, st Settings) error {
	payload, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return s.db.WithContext(ctx).Save(&Setting{Key: settingsKey, Value: string(payload)}).Error
}

// Purge permanently removes entries soft-deleted before cutoff.
func (s *Store) Purge(ctx context.Context, cutoff time.Time, dryRun bool) (int64, error) {
	q := s.db.WithContext(ctx).Unscoped().Where("deleted_at IS NOT NULL AND deleted_at < ?", cutoff)
	if dryRun {
		var n int64
		err := q.Model(&Entry{}).Count(&n).Error
		return n, err
	}
	res := q.Delete(&Entry{})
	return res.RowsAffected, res.Error
}

// Expire soft-deletes unpinned entries created before cutoff.
func (s *Store) Expire(ctx context.Context, cutoff time.Time, dryRun bool) (int64, error) {
	q := s.db.WithContext(ctx).Where("pinned = ? AND created_at < ?", false, cutoff)
	if dryRun {
		var n int64
		err := q.Model(&Entry{}).Count(&n).Error
		return n, err
	}
	res := q.Delete(&Entry{})
	return res.RowsAffected, res.Error
}
