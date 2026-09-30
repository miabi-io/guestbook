package main

import (
	"net/http"
	"os"
	"runtime"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/jkaninda/logger"
	"github.com/jkaninda/okapi"
)

// Failure injection so restarts, health gating, analytics and alerts have
// something to react to during a demo. See DEMO.md.

const (
	maxSlow      = 30 * time.Second
	maxBurn      = 120 * time.Second
	maxBallastMB = 2048
	maxLogBurst  = 1000
	defaultBurnS = 30
	defaultMemMB = 256
)

type chaosState struct {
	cpuBusy   atomic.Bool
	ballastMB atomic.Int64
}

// RequireDebug rejects requests unless DEBUG_ENDPOINTS=true.
func (h *Handler) RequireDebug(c *okapi.Context) error {
	if !h.debug {
		return c.JSON(http.StatusNotFound, okapi.M{"error": "debug endpoints are disabled (set DEBUG_ENDPOINTS=true)"})
	}
	return c.Next()
}

// DebugCrash exits the process with status 1 after answering.
func (h *Handler) DebugCrash(c *okapi.Context) error {
	logger.Error("crash requested via /api/debug/crash, exiting", "host", h.host)
	go func() {
		time.Sleep(300 * time.Millisecond)
		os.Exit(1)
	}()
	return c.JSON(http.StatusAccepted, okapi.M{"status": "crashing", "host": h.host})
}

// DebugSlow sleeps for ?ms= milliseconds (default 2000) before answering.
func (h *Handler) DebugSlow(c *okapi.Context) error {
	d := min(time.Duration(atoiDefault(c.Query("ms"), 2000))*time.Millisecond, maxSlow)
	select {
	case <-time.After(d):
	case <-c.Request().Context().Done():
		return nil
	}
	return c.OK(okapi.M{"slept_ms": d.Milliseconds(), "host": h.host})
}

// DebugError answers with ?status= (default 500).
func (h *Handler) DebugError(c *okapi.Context) error {
	status := atoiDefault(c.Query("status"), http.StatusInternalServerError)
	if status < 400 || status > 599 {
		status = http.StatusInternalServerError
	}
	logger.Error("injected error via /api/debug/error", "status", status, "host", h.host)
	return c.JSON(status, okapi.M{"error": "injected failure", "status": status, "host": h.host})
}

// DebugHealth forces /healthz on this replica to fail (or recover).
func (h *Handler) DebugHealth(c *okapi.Context) error {
	var req struct {
		Healthy bool `json:"healthy"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, okapi.M{"error": "invalid JSON body"})
	}
	h.unhealthy.Store(!req.Healthy)
	logger.Warn("health overridden via /api/debug/health", "healthy", req.Healthy, "host", h.host)
	return c.OK(okapi.M{"healthy": req.Healthy, "host": h.host})
}

// DebugCPU saturates every core for ?seconds= (default 30).
func (h *Handler) DebugCPU(c *okapi.Context) error {
	d := min(time.Duration(atoiDefault(c.Query("seconds"), defaultBurnS))*time.Second, maxBurn)
	if !h.chaos.cpuBusy.CompareAndSwap(false, true) {
		return c.JSON(http.StatusConflict, okapi.M{"error": "a CPU burn is already running"})
	}
	workers := runtime.GOMAXPROCS(0)
	deadline := time.Now().Add(d)
	done := make(chan struct{}, workers)
	for range workers {
		go func() {
			x := 0
			for time.Now().Before(deadline) {
				for i := range 1_000_000 {
					x += i
				}
			}
			_ = x
			done <- struct{}{}
		}()
	}
	go func() {
		for range workers {
			<-done
		}
		h.chaos.cpuBusy.Store(false)
		logger.Info("cpu burn finished", "host", h.host)
	}()
	logger.Warn("cpu burn started", "seconds", d.Seconds(), "workers", workers, "host", h.host)
	return c.JSON(http.StatusAccepted, okapi.M{"seconds": d.Seconds(), "workers": workers, "host": h.host})
}

// DebugMemory holds ?mb= MiB (default 256) of touched memory for ?seconds=
// (default 30). Useful to show memory limits and OOM restarts.
func (h *Handler) DebugMemory(c *okapi.Context) error {
	mb := min(atoiDefault(c.Query("mb"), defaultMemMB), maxBallastMB)
	d := min(time.Duration(atoiDefault(c.Query("seconds"), defaultBurnS))*time.Second, maxBurn)
	if mb <= 0 {
		return c.JSON(http.StatusBadRequest, okapi.M{"error": "mb must be positive"})
	}
	if !h.chaos.ballastMB.CompareAndSwap(0, int64(mb)) {
		return c.JSON(http.StatusConflict, okapi.M{"error": "memory is already being held"})
	}
	go func() {
		ballast := make([]byte, mb<<20)
		// Touch every page so the memory is actually resident, not just reserved.
		for i := 0; i < len(ballast); i += 4096 {
			ballast[i] = 1
		}
		time.Sleep(d)
		runtime.KeepAlive(ballast)
		h.chaos.ballastMB.Store(0)
		logger.Info("memory released", "mb", mb, "host", h.host)
	}()
	logger.Warn("holding memory", "mb", mb, "seconds", d.Seconds(), "host", h.host)
	return c.JSON(http.StatusAccepted, okapi.M{"mb": mb, "seconds": d.Seconds(), "host": h.host})
}

// DebugLogs writes ?count= (default 20) log lines at ?level= (default error).
func (h *Handler) DebugLogs(c *okapi.Context) error {
	count := min(max(atoiDefault(c.Query("count"), 20), 1), maxLogBurst)
	level := c.Query("level")
	for i := range count {
		n := strconv.Itoa(i + 1)
		switch level {
		case "info":
			logger.Info("demo log line", "n", n, "host", h.host)
		case "warn":
			logger.Warn("demo log line", "n", n, "host", h.host)
		default:
			level = "error"
			logger.Error("demo log line", "n", n, "host", h.host)
		}
	}
	return c.OK(okapi.M{"count": count, "level": level, "host": h.host})
}
