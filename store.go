package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/glebarez/sqlite" // pure-Go SQLite (no CGO — keeps the static build)
	"github.com/jkaninda/logger"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// Entry is a single signature on the guestbook wall.
type Entry struct {
	ID        uint           `gorm:"primaryKey" json:"id"`
	Name      string         `gorm:"type:varchar(60);not null" json:"name"`
	Message   string         `gorm:"type:varchar(500);not null" json:"message"`
	CreatedAt time.Time      `gorm:"index" json:"created_at"`
	UpdatedAt time.Time      `json:"-"`
	DeletedAt gorm.DeletedAt `gorm:"index" json:"-"`
}

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

func (s *Store) Migrate() error {
	return s.db.AutoMigrate(&Entry{})
}

// List returns entries newest first, with limit/offset pagination.
func (s *Store) List(ctx context.Context, limit, offset int) ([]Entry, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	var entries []Entry
	err := s.db.WithContext(ctx).
		Order("created_at DESC").
		Limit(limit).
		Offset(offset).
		Find(&entries).Error
	return entries, err
}

// Create inserts a new entry and returns it with its generated id/timestamp.
func (s *Store) Create(ctx context.Context, name, message string) (Entry, error) {
	e := Entry{Name: name, Message: message}
	err := s.db.WithContext(ctx).Create(&e).Error
	return e, err
}

// Delete soft-deletes an entry by id. Returns true when a row was affected.
func (s *Store) Delete(ctx context.Context, id uint64) (bool, error) {
	res := s.db.WithContext(ctx).Delete(&Entry{}, id)
	return res.RowsAffected > 0, res.Error
}

// Count returns the total number of (non-deleted) entries.
func (s *Store) Count(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.WithContext(ctx).Model(&Entry{}).Count(&n).Error
	return n, err
}
