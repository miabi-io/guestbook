package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/jkaninda/logger"
	"github.com/jkaninda/njia"
	"github.com/jkaninda/okapi"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const streamRoute = "/api/stream"

type Metrics struct {
	registry  *prometheus.Registry
	requests  *prometheus.CounterVec
	duration  *prometheus.HistogramVec
	inFlight  prometheus.Gauge
	entries   prometheus.Counter
	deletions prometheus.Counter
	reactions *prometheus.CounterVec
}

func NewMetrics() *Metrics {
	m := &Metrics{
		registry: prometheus.NewRegistry(),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "guestbook_http_requests_total",
			Help: "HTTP requests answered, by method, route pattern and status code.",
		}, []string{"method", "route", "status"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "guestbook_http_request_duration_seconds",
			Help:    "Time to answer an HTTP request, by method and route pattern. The SSE stream is left out.",
			Buckets: prometheus.DefBuckets,
		}, []string{"method", "route"}),
		inFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "guestbook_http_requests_in_flight",
			Help: "HTTP requests being answered right now, open SSE streams included.",
		}),
		entries: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "guestbook_entries_created_total",
			Help: "Signatures written through this replica.",
		}),
		deletions: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "guestbook_entries_deleted_total",
			Help: "Signatures deleted by a moderator through this replica.",
		}),
		reactions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "guestbook_reactions_total",
			Help: "Reactions added or removed through this replica, by emoji.",
		}, []string{"emoji", "action"}),
	}
	m.registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.requests, m.duration, m.inFlight, m.entries, m.deletions, m.reactions,
	)
	return m
}

// Watch registers gauges read at scrape time, so they cost nothing between scrapes.
func (m *Metrics) Watch(h *Handler) {
	m.registry.MustRegister(
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name:        "guestbook_build_info",
			Help:        "Always 1; the labels say which build and replica is answering.",
			ConstLabels: prometheus.Labels{"version": h.version, "host": h.host, "db": h.dbDriver, "broker": h.broker.Mode()},
		}, func() float64 { return 1 }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "guestbook_sse_clients",
			Help: "Browsers connected to this replica's live stream.",
		}, func() float64 { return float64(h.broker.Online()) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "guestbook_entries",
			Help: "Signatures visible on the wall.",
		}, func() float64 {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			n, _ := h.store.CountVisible(ctx)
			return float64(n)
		}),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "guestbook_visitors_active",
			Help: "Browsers seen in the last few minutes, across all replicas.",
		}, func() float64 {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			return float64(h.visitorsActive(ctx))
		}),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "guestbook_database_up",
			Help: "1 when the database answers a ping.",
		}, func() float64 {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if h.store.Ping(ctx) != nil {
				return 0
			}
			return 1
		}),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "guestbook_healthy",
			Help: "1 unless /healthz was forced to fail from the chaos lab.",
		}, func() float64 {
			if h.unhealthy.Load() {
				return 0
			}
			return 1
		}),
	)
}

func (m *Metrics) Middleware(c *okapi.Context) error {
	if m == nil {
		return c.Next()
	}
	start := time.Now()
	m.inFlight.Inc()
	defer m.inFlight.Dec()

	err := c.Next()

	method, route := c.Request().Method, routeOf(c.Request())
	status := c.Response().StatusCode()
	if status == 0 {
		status = http.StatusOK
	}
	m.requests.WithLabelValues(method, route, strconv.Itoa(status)).Inc()
	if route != streamRoute {
		m.duration.WithLabelValues(method, route).Observe(time.Since(start).Seconds())
	}
	return err
}

// routeOf labels by the registered pattern; raw paths would give every entry id its own series.
func routeOf(r *http.Request) string {
	if rt := njia.RouteOf(r); rt != nil && rt.Pattern() != "" {
		return rt.Pattern()
	}
	// Static routes carry no match, and unrouted paths never reach middleware, so the path is the pattern.
	return r.URL.Path
}

func (m *Metrics) EntryCreated() {
	if m != nil {
		m.entries.Inc()
	}
}

func (m *Metrics) EntryDeleted() {
	if m != nil {
		m.deletions.Inc()
	}
}

func (m *Metrics) Reaction(emoji, action string) {
	if m != nil {
		m.reactions.WithLabelValues(emoji, action).Inc()
	}
}

func (m *Metrics) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{Registry: m.registry}))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, "/metrics", http.StatusFound)
	})
	return mux
}

// MetricsServer binds before returning, so a taken port fails the start instead of a log line later.
type MetricsServer struct {
	srv *http.Server
	ln  net.Listener
}

func StartMetricsServer(m *Metrics, port int) (*MetricsServer, error) {
	ln, err := net.Listen("tcp", ":"+strconv.Itoa(port))
	if err != nil {
		return nil, err
	}
	s := &MetricsServer{
		srv: &http.Server{Handler: m.Handler(), ReadHeaderTimeout: 5 * time.Second},
		ln:  ln,
	}
	go func() {
		if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("metrics server stopped", "error", err)
		}
	}()
	return s, nil
}

func (s *MetricsServer) Addr() string { return s.ln.Addr().String() }

func (s *MetricsServer) Shutdown(ctx context.Context) error {
	if s == nil {
		return nil
	}
	return s.srv.Shutdown(ctx)
}
