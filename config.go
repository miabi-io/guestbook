package main

import (
	"fmt"
	"strings"

	goutils "github.com/jkaninda/go-utils"
)

// Config is the runtime configuration, read entirely from environment
// variables — the twelve-factor way, and exactly how Miabi injects settings
// into a deployed application (Postgres connection details land here when you
// attach a managed database to the app).
type Config struct {
	Port        int
	DBDriver    string // "postgres" or "sqlite"
	DatabaseDSN string
	AppName     string
	Version     string
	Seed        bool
}

// LoadConfig builds the Config from the environment.
func LoadConfig() (Config, error) {
	driver, dsn := resolveDatabase()
	return Config{
		Port:        goutils.EnvInt("PORT", 8080),
		AppName:     goutils.Env("APP_NAME", "Miabi Guestbook"),
		Version:     resolveVersion(),
		Seed:        goutils.Env("SEED", "true") == "true",
		DBDriver:    driver,
		DatabaseDSN: dsn,
	}, nil
}

// resolveDatabase picks the database driver and DSN.
//
// The driver comes from DB_DRIVER (postgres|sqlite). When DB_DRIVER is unset it
// is inferred: if Postgres is configured (DATABASE_URL or DB_HOST is present)
// Postgres is used; otherwise the app falls back to a zero-config SQLite file —
// handy for local dev and demos with nothing else to set up. On Miabi, attach a
// managed Postgres and it is selected automatically.
func resolveDatabase() (driver, dsn string) {
	driver = strings.ToLower(goutils.Env("DB_DRIVER", ""))
	if driver == "" {
		if goutils.Env("DATABASE_URL", "") != "" || goutils.Env("DB_HOST", "") != "" {
			driver = "postgres"
		} else {
			driver = "sqlite"
		}
	}

	switch driver {
	case "sqlite", "sqlite3":
		// A file path; use ":memory:" for an ephemeral in-memory database. The
		// parent directory is created on startup. In the container this defaults
		// to /data (a volume); see DB_PATH in the Dockerfile.
		return "sqlite", goutils.Env("DB_PATH", "data/guestbook.db")
	default:
		return "postgres", postgresDSN()
	}
}

// postgresDSN resolves the Postgres connection, following Miabi's convention:
//
//   - DATABASE_URL — a full DSN (URL or key=value). Preferred, and what Miabi's
//     managed databases inject. When set, it is used verbatim.
//   - Discrete DB_* variables — assembled into a key=value DSN otherwise.
func postgresDSN() string {
	if url := goutils.Env("DATABASE_URL", ""); url != "" {
		return url
	}
	return fmt.Sprintf(
		"host=%s user=%s password=%s dbname=%s port=%d sslmode=%s",
		goutils.Env("DB_HOST", "localhost"),
		goutils.Env("DB_USER", "postgres"),
		goutils.Env("DB_PASSWORD", "postgres"),
		goutils.Env("DB_NAME", "guestbook"),
		goutils.EnvInt("DB_PORT", 5432),
		goutils.Env("DB_SSL_MODE", "disable"),
	)
}
