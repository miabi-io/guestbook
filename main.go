// Command guestbook is a tiny, self-contained web application used to
// demonstrate deploying to Miabi: an Okapi (Go) API + embedded SPA, backed by
// a Postgres (or SQLite) database via GORM.
//
// It is a classic "guestbook" — visitors leave a name and a message, and the
// wall shows every signature. Everything a real Miabi deployment exercises is
// here in miniature: env-based config (DATABASE_URL), an attached managed
// database, an optional Redis for multi-replica live updates, a secret-guarded
// admin console, failure-injection endpoints, a scheduled cleanup job, a
// /healthz readiness probe, and a single static binary that ships the whole
// frontend embedded — no Node build step.
//
// Subcommands (okapicli): server (default), migrate, cleanup, reset.
package main

import (
	"context"
	"embed"
	"io/fs"
	"net/http"
	"os"
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

	opts := []okapi.OptionFunc{okapi.WithPort(cfg.Port)}
	if len(cfg.TrustedProxies) > 0 {
		opts = append(opts, okapi.WithTrustedProxies(cfg.TrustedProxies...))
	}
	app := okapi.New(opts...)
	cli := okapicli.New(app, "guestbook")

	cli.Command("server", "Start the Guestbook HTTP server", func(cmd *okapicli.Command) error {
		return runServer(cmd, cfg)
	}).Int("port", "p", cfg.Port, "HTTP server port")

	cli.Command("migrate", "Apply database migrations and exit (e.g. as a pre-deploy step)", func(cmd *okapicli.Command) error {
		store, err := openStore(cfg)
		if err != nil {
			return err
		}
		defer func() { _ = store.Close() }()
		logger.Info("migrations applied", "db", dbDisplayName(cfg.DBDriver))
		return nil
	})

	cli.Command("cleanup", "Purge deleted entries and optionally expire old ones (run as a scheduled job)", func(cmd *okapicli.Command) error {
		return runCleanup(cmd, cfg)
	}).
		Duration("purge-after", "", 7*24*time.Hour, "Permanently remove entries deleted longer ago than this").
		Duration("max-age", "", 0, "Also delete unpinned entries older than this (0 = keep forever)").
		Duration("prune-visitors", "", 30*24*time.Hour, "Forget visitor identities idle longer than this (0 = keep forever)").
		Bool("dry-run", "", false, "Report what would be removed without changing anything")

	cli.Command("reset", "Wipe the database: entries, reactions and visitor identities (run as a one-off job)", func(cmd *okapicli.Command) error {
		return runReset(cmd, cfg)
	}).
		Bool("hard", "", false, "Also reset the wall settings (pause, banner, theme)").
		Bool("reseed", "", false, "Insert the sample signatures after the wipe").
		Bool("dry-run", "", false, "Report what would be removed without changing anything")

	cli.DefaultCommand("server")

	if err := cli.Execute(); err != nil {
		logger.Fatal("command failed", "error", err)
	}
}

func runServer(cmd *okapicli.Command, cfg Config) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store, err := openStore(cfg)
	if err != nil {
		return err
	}
	if cfg.Seed {
		if err := Seed(ctx, store, cfg.DBDriver); err != nil {
			return err
		}
	}

	host, _ := os.Hostname()
	broker := NewBroker(host, cfg.Version, nil)
	if cfg.RedisURL != "" {
		if err := broker.UseRedis(cfg.RedisURL); err != nil {
			return err
		}
	}
	go broker.Run(ctx)
	go publishTicks(ctx, broker, cfg.Version, host)
	logger.Info("live updates ready", "broker", broker.Mode())

	h := &Handler{
		store:    store,
		broker:   broker,
		auth:     NewAdminAuth(cfg.AdminToken),
		appName:  cfg.AppName,
		version:  cfg.Version,
		host:     host,
		dbDriver: cfg.DBDriver,
		debug:    cfg.DebugEndpoints,
		started:  time.Now(),
	}
	broker.requests = h.requests.Load
	broker.visitors = func() int64 { return h.visitorsActive(context.Background()) }
	if !h.auth.Enabled() {
		logger.Warn("admin console disabled: set ADMIN_TOKEN to enable /admin")
	}
	if h.debug {
		logger.Warn("debug endpoints enabled under /api/debug (admin auth required)")
	}

	app := cmd.Okapi()
	app.WithPort(cmd.GetInt("port"))
	app.WithDebug()
	app.Use(okapi.LoggerMiddleware, okapi.RequestID(), h.countRequests)
	registerRoutes(app, h)

	return cmd.CLI().RunServer(&okapicli.RunOptions{
		ShutdownTimeout: 10 * time.Second,
		OnStarted: func() {
			logger.Info("server started", "app", cfg.AppName, "version", cfg.Version, "port", cmd.GetInt("port"))
		},
		OnShutdown: func() {
			logger.Info("shutting down")
			closeCtx, done := context.WithTimeout(context.Background(), 2*time.Second)
			defer done()
			cancel()
			broker.Close(closeCtx)
			_ = store.Close()
		},
	})
}

func registerRoutes(app *okapi.Okapi, h *Handler) {
	app.Get("/healthz", h.Health)

	// Every API request resolves the visitor identity: the cookie is checked
	// against the database and regenerated when its id is unknown (fresh
	// browser, pruned identity, or a reset database).
	api := app.Group("/api", h.identifyVisitor)
	api.Get("/info", h.Info)
	api.Get("/time", h.Time)
	api.Get("/settings", h.PublicSettings)
	api.Get("/me", h.VisitorMe)
	api.Get("/entries", h.ListEntries)
	api.Post("/entries", h.CreateEntry)
	api.Post("/entries/{id:int}/reactions", h.React)
	api.Delete("/entries/{id:int}/reactions", h.Unreact)
	api.Get("/stream", h.Stream)

	api.Get("/admin/session", h.AdminSession)
	api.Post("/admin/login", h.AdminLogin)
	api.Post("/admin/logout", h.AdminLogout)

	admin := app.Group("/api/admin", h.identifyVisitor, h.auth.Require)
	admin.Get("/overview", h.AdminOverview)
	admin.Get("/entries", h.AdminListEntries)
	admin.Patch("/entries/{id:int}", h.AdminUpdateEntry)
	admin.Delete("/entries/{id:int}", h.DeleteEntry)
	admin.Get("/settings", h.AdminSettings)
	admin.Put("/settings", h.AdminSaveSettings)
	admin.Get("/visitors", h.AdminVisitors)
	admin.Get("/export.csv", h.AdminExport)

	debug := app.Group("/api/debug", h.auth.Require, h.RequireDebug)
	debug.Post("/crash", h.DebugCrash)
	debug.Get("/slow", h.DebugSlow)
	debug.Get("/error", h.DebugError)
	debug.Post("/health", h.DebugHealth)
	debug.Post("/cpu", h.DebugCPU)
	debug.Post("/memory", h.DebugMemory)
	debug.Post("/logs", h.DebugLogs)

	adminPage, err := fs.ReadFile(webFS, "web/admin.html")
	if err != nil {
		logger.Fatal("admin page missing from embedded assets", "error", err)
	}
	app.Get("/admin", func(c *okapi.Context) error {
		return c.Data(http.StatusOK, "text/html; charset=utf-8", adminPage)
	})

	// Browsers ask for /favicon.ico on their own; the logo serves as the icon.
	app.Get("/favicon.ico", func(c *okapi.Context) error {
		c.Redirect(http.StatusFound, "/badges/icon.svg")
		return nil
	})

	// Okapi serves real files directly and falls back to index.html for any
	// other path so the client-side router can take over.
	app.WebFS("/", webFS, okapi.WebConfig{Root: "web"})
}

func runCleanup(cmd *okapicli.Command, cfg Config) error {
	ctx := context.Background()
	store, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	dryRun := cmd.GetBool("dry-run")
	now := time.Now()

	purged, err := store.Purge(ctx, now.Add(-cmd.GetDuration("purge-after")), dryRun)
	if err != nil {
		return err
	}
	var expired int64
	if maxAge := cmd.GetDuration("max-age"); maxAge > 0 {
		if expired, err = store.Expire(ctx, now.Add(-maxAge), dryRun); err != nil {
			return err
		}
	}
	var pruned int64
	if prune := cmd.GetDuration("prune-visitors"); prune > 0 {
		if pruned, err = store.PurgeVisitors(ctx, now.Add(-prune), dryRun); err != nil {
			return err
		}
	}
	remaining, _ := store.Count(ctx)
	logger.Info("cleanup finished",
		"purged", purged, "expired", expired, "visitors_pruned", pruned,
		"remaining", remaining, "dry_run", dryRun)
	return nil
}

// runReset wipes the database (the "clean slate" job): entries, reactions and
// visitor identities go, settings stay unless --hard. With --reseed the sample
// signatures are inserted right away; otherwise restart the app so its first
// boot seeds the empty wall. The wall's cookies stop matching any visitor row
// after a reset, so browsers get a fresh identity on their next request.
func runReset(cmd *okapicli.Command, cfg Config) error {
	ctx := context.Background()
	store, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	dryRun := cmd.GetBool("dry-run")
	hard := cmd.GetBool("hard")

	stats, err := store.Reset(ctx, hard, dryRun)
	if err != nil {
		return err
	}
	if dryRun {
		logger.Info("reset dry run, nothing removed",
			"entries", stats.Entries, "reactions", stats.Reactions,
			"visitors", stats.Visitors, "settings", stats.Settings)
		return nil
	}
	logger.Warn("database reset",
		"entries", stats.Entries, "reactions", stats.Reactions,
		"visitors", stats.Visitors, "settings", stats.Settings, "hard", hard)

	if cmd.GetBool("reseed") {
		if err := Seed(ctx, store, cfg.DBDriver); err != nil {
			return err
		}
	} else {
		logger.Info("wall is empty; restart the app to re-seed it")
	}
	return nil
}

// openStore connects to the database and applies migrations, retrying while
// the database comes up.
func openStore(cfg Config) (*Store, error) {
	store, err := NewStore(cfg.DBDriver, cfg.DatabaseDSN)
	if err != nil {
		return nil, err
	}
	if err := migrateWithRetry(store, 30, 2*time.Second); err != nil {
		_ = store.Close()
		return nil, err
	}
	return store, nil
}

// publishTicks sends each replica's own clock to its local clients once a
// second, so under a canary rollout the clock card reveals which build served
// you. Ticks stay local on purpose, even with Redis.
func publishTicks(ctx context.Context, b *Broker, version, host string) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			b.PublishLocal(Event{
				Type:     "tick",
				Time:     now.Format(time.RFC3339),
				Version:  version,
				Host:     host,
				Visitors: b.visitorsActive(),
			})
		}
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
