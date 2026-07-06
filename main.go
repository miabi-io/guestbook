// Command guestbook is a tiny, self-contained web application used to
// demonstrate deploying to Miabi: an Okapi (Go) API + embedded SPA, backed by
// a Postgres (or SQLite) database via GORM.
//
// It is a classic "guestbook" — visitors leave a name and a message, and the
// wall shows every signature. Everything a real Miabi deployment exercises is
// here in miniature: env-based config (DATABASE_URL), an attached managed
// database, a GORM AutoMigrate on startup, a first-run seeder, a /healthz
// readiness probe, and a single static binary that ships the whole frontend
// embedded — no Node build step.
//
// The HTTP server is driven by okapicli (a "server" subcommand with graceful
// lifecycle hooks), and the embedded UI is served with app.WebFS.
package main

import (
	"context"
	"embed"
	"time"

	"github.com/jkaninda/logger"
	"github.com/jkaninda/okapi"
	"github.com/jkaninda/okapi/okapicli"
)

//go:embed all:web
var webFS embed.FS

func main() {
	cfg, err := LoadConfig()
	if err != nil {
		logger.Fatal("invalid configuration", "error", err)
	}

	ctx := context.Background()

	store, err := NewStore(cfg.DBDriver, cfg.DatabaseDSN)
	if err != nil {
		logger.Fatal("failed to open database", "error", err)
	}

	if err := migrateWithRetry(store, 30, 2*time.Second); err != nil {
		logger.Fatal("failed to run migrations", "error", err)
	}
	logger.Info("schema ready", "app", cfg.AppName)

	// Seed sample data on first run (empty table only).
	if cfg.Seed {
		if err := Seed(ctx, store, cfg.DBDriver); err != nil {
			logger.Fatal("failed to seed database", "error", err)
		}
	}

	broker := NewBroker()
	h := &Handler{store: store, broker: broker, appName: cfg.AppName, version: cfg.Version}

	app := okapi.New(okapi.WithPort(cfg.Port))
	app.WithDebug()
	app.Use(okapi.LoggerMiddleware, okapi.RequestID())

	app.Get("/healthz", h.Health)
	api := app.Group("/api")
	api.Get("/info", h.Info)
	api.Get("/entries", h.ListEntries)
	api.Post("/entries", h.CreateEntry)
	api.Delete("/entries/{id:int}", h.DeleteEntry)
	api.Get("/stream", h.Stream) // Server-Sent Events: live entries + presence

	// Serve the embedded single-page UI. Okapi serves real files directly and
	// falls back to index.html for any other path so a client-side router can
	// take over.
	app.WebFS("/", webFS, okapi.WebConfig{Root: "web"})

	cli := okapicli.New(app, "guestbook")
	cli.Command("server", "Start the Guestbook HTTP server", func(cmd *okapicli.Command) error {
		cmd.Okapi().WithPort(cmd.GetInt("port"))
		return cmd.CLI().RunServer(&okapicli.RunOptions{
			ShutdownTimeout: 10 * time.Second,
			OnStarted: func() {
				logger.Info("server started", "app", cfg.AppName, "version", cfg.Version, "port", cmd.GetInt("port"))
			},
			OnShutdown: func() {
				logger.Info("shutting down")
				_ = store.Close()
			},
		})
	}).Int("port", "p", cfg.Port, "HTTP server port")
	cli.DefaultCommand("server")

	if err := cli.Execute(); err != nil {
		logger.Fatal("server error", "error", err)
	}
}

func migrateWithRetry(store *Store, attempts int, wait time.Duration) error {
	var err error
	for i := 0; i < attempts; i++ {
		if err = store.Migrate(); err == nil {
			return nil
		}
		logger.Warn("waiting for database", "attempt", i+1, "of", attempts, "error", err)
		time.Sleep(wait)
	}
	return err
}
