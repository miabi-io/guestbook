package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jkaninda/okapi"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestMetricsLabelRequestsByRoutePattern(t *testing.T) {
	m := NewMetrics()
	app := okapi.New()
	app.Use(m.Middleware)
	app.Get("/api/entries/{id:int}", func(c *okapi.Context) error { return c.OK(okapi.M{}) })
	app.Group("/api").Get("/info", func(c *okapi.Context) error { return c.OK(okapi.M{}) })
	srv := httptest.NewServer(app)
	t.Cleanup(srv.Close)

	for _, path := range []string{"/api/entries/1", "/api/entries/2", "/api/info", "/nope"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}

	if got := testutil.ToFloat64(m.requests.WithLabelValues("GET", "/api/entries/{id}", "200")); got != 2 {
		t.Errorf("requests for the entry pattern = %v, want 2", got)
	}
	if got := testutil.ToFloat64(m.requests.WithLabelValues("GET", "/api/info", "200")); got != 1 {
		t.Errorf("requests for the static route = %v, want 1", got)
	}
	if n := testutil.CollectAndCount(m.requests); n != 2 {
		t.Errorf("series = %d, want 2: a path outside every route must not add one", n)
	}
}

func TestNilMetricsAreANoOp(t *testing.T) {
	var m *Metrics
	m.EntryCreated()
	m.EntryDeleted()
	m.Reaction("heart", "add")
	app := okapi.New()
	app.Use(m.Middleware)
	app.Get("/", func(c *okapi.Context) error { return c.OK(okapi.M{}) })
	srv := httptest.NewServer(app)
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestMetricsServerServesPrometheusText(t *testing.T) {
	m := NewMetrics()
	m.EntryCreated()
	m.Reaction("tada", "add")
	s, err := StartMetricsServer(m, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	resp, err := http.Get("http://" + s.Addr() + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	for _, want := range []string{
		"guestbook_entries_created_total 1",
		`guestbook_reactions_total{action="add",emoji="tada"} 1`,
		"go_goroutines",
		"process_resident_memory_bytes",
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("/metrics is missing %q", want)
		}
	}
}

func TestMetricsServerRefusesATakenPort(t *testing.T) {
	first, err := StartMetricsServer(NewMetrics(), 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Shutdown(context.Background()) })
	port := first.Addr()[strings.LastIndex(first.Addr(), ":")+1:]
	if _, err := StartMetricsServer(NewMetrics(), atoiDefault(port, 0)); err == nil {
		t.Fatal("a second server on the same port should fail to start")
	}
}

func TestMetricsConfig(t *testing.T) {
	t.Setenv("PORT", "8080")
	t.Setenv("METRICS_ENABLED", "")
	t.Setenv("METRICS_PORT", "")
	cfg, err := LoadConfig()
	if err != nil || !cfg.MetricsEnabled || cfg.MetricsPort != 9090 {
		t.Fatalf("defaults = %v on %d (%v), want enabled on 9090", cfg.MetricsEnabled, cfg.MetricsPort, err)
	}

	t.Setenv("METRICS_ENABLED", "false")
	if cfg, _ := LoadConfig(); cfg.MetricsEnabled {
		t.Error("METRICS_ENABLED=false should turn metrics off")
	}

	t.Setenv("METRICS_ENABLED", "true")
	t.Setenv("METRICS_PORT", "8080")
	if _, err := LoadConfig(); err == nil {
		t.Error("metrics on the app's own port should be refused")
	}
}
