package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rgracey/kanban-mcp/internal/api"
	"github.com/rgracey/kanban-mcp/internal/clocksource"
	"github.com/rgracey/kanban-mcp/internal/config"
	"github.com/rgracey/kanban-mcp/internal/crashpoint"
	"github.com/rgracey/kanban-mcp/internal/db"
	internalmcp "github.com/rgracey/kanban-mcp/internal/mcp"
	"github.com/rgracey/kanban-mcp/internal/store"
)

// version is set at build time via -ldflags="-X main.version=<tag>".
var version = "dev"

func main() {
	cfg := config.Load()

	// Set up structured logging with JSON output
	var level slog.Level
	switch cfg.LogLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	slog.Info("starting kanban-mcp", "version", version)
	slog.Debug("config loaded", "config", cfg)

	// Open database with migrations
	// Arm the inert crash-injection seam only if the environment says so (C-CONV-11).
	// This is the only call site: nothing on the REST or MCP surface can arm a point.
	crashpoint.Init()
	if names := crashpoint.ArmedNames(); len(names) > 0 {
		slog.Warn("crash points armed: this process will SIGKILL itself at the named points", "points", names)
	}

	// now_ms comes from exactly one place (C-CONV-2).
	clock, err := clocksource.FromEnv()
	if err != nil {
		slog.Error("invalid clock configuration", "error", err)
		log.Fatal(err)
	}

	dbConn, err := db.Open(cfg.DBPath)
	if err != nil {
		slog.Error("failed to open database", "error", err)
		log.Fatal(err)
	}

	slog.Info("database opened", "path", cfg.DBPath)

	// Create store, MCP server, and API router
	// The contract's separate read pool (C-CONV-9): four connections, never used for
	// a mutation. The single writer handle above serialises every write.
	readPool, err := db.OpenReadPool(cfg.DBPath)
	if err != nil {
		slog.Error("failed to open read pool", "error", err)
		log.Fatal(err)
	}
	defer readPool.Close()

	store := store.NewSQLiteStore(dbConn,
		store.WithClock(clock),
		store.WithLeaseTTLMS(cfg.LeaseTTLMS),
		store.WithClaimableStatuses(cfg.ClaimableStatuses),
		store.WithReadPool(readPool),
	)
	// C-IDEM-16(a): purge expired idempotency keys once, at startup. Housekeeping
	// only — no correctness depends on it (C-IDEM-19) — so a failure is logged and
	// the server still starts. The opportunistic in-transaction purge rides along
	// with non-deduplicated write transactions (C-IDEM-16(b)).
	if n, err := store.PurgeIdempotencyKeysOnStartup(context.Background()); err != nil {
		slog.Error("idempotency key purge failed", "error", err)
	} else if n > 0 {
		slog.Info("purged expired idempotency keys", "count", n)
	}

	// Memory delivery stays off in this slice (C-OUTBOX-14's default, spec §10):
	// handoff transactions create and expose outbox rows, and the delivery worker
	// arrives with the outbox slice (C-OUTBOX-15).

	mcpSrv := internalmcp.NewServer(store)
	hub := api.NewHub()

	// When using HTTP transport, mount the MCP handler on /mcp within the main
	// router so both the SPA/API and MCP share a single port.
	var mcpHandler http.Handler
	if cfg.MCPTransport == "http" || cfg.MCPTransport == "both" {
		mcpHandler = internalmcp.NewHTTPHandler(mcpSrv)
		slog.Info("MCP HTTP handler mounted", "path", "/mcp", "port", cfg.Port)
	}

	router := api.NewRouter(store, hub, mcpHandler)

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.Port),
		Handler: router,
	}

	go func() {
		slog.Info("HTTP server listening", "port", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server error", "err", err)
			os.Exit(1)
		}
	}()

	// For stdio and both transports, start the stdio MCP listener.
	if cfg.MCPTransport == "stdio" || cfg.MCPTransport == "both" {
		go func() {
			slog.Info("MCP stdio transport starting")
			if err := internalmcp.StartStdio(mcpSrv); err != nil {
				slog.Error("MCP stdio error", "err", err)
			}
		}()
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	slog.Info("shutting down")

	// Close the SSE hub first so all streaming connections exit cleanly
	// before http.Server.Shutdown tries to drain active connections.
	hub.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		slog.Error("shutdown error", "err", err)
	}
	slog.Info("shutdown complete")

	dbConn.Close()
}
