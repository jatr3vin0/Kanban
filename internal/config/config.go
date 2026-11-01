package config

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/rgracey/kanban-mcp/internal/models"
	"github.com/rgracey/kanban-mcp/internal/store"
)

// Config holds the application configuration.
// Precedence (highest to lowest): CLI flag > environment variable > default.
type Config struct {
	Port         int
	DBPath       string
	MCPTransport string
	LogLevel     string

	// LeaseTTLMS is LEDGERBOARD_LEASE_TTL_MS (C-CONV-10): the spec's fifteen
	// minutes by default, floored at one second. Read at the moment a lease is
	// issued or renewed, so changing it never rewrites an existing lease.
	LeaseTTLMS int64
	// SweepIntervalMS is LEDGERBOARD_SWEEP_INTERVAL_MS (C-CLAIM-20): how often
	// the in-process expiry sweeper ticks. It exists so tests can bound the
	// ticker; 60000 is the contract default.
	SweepIntervalMS int64
	// ClaimableStatuses is LEDGERBOARD_CLAIMABLE_STATUSES (C-CLAIM-3). `done`
	// and `blocked` are never accepted in it.
	ClaimableStatuses []models.Status
}

// Load parses CLI flags and environment variables into a Config.
// It writes a diagnostic message to stderr and calls os.Exit(1) on invalid values.
func Load() Config {
	cfg, err := load(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	return cfg
}

// load is the testable core: it parses args and returns an error on bad input.
func load(args []string) (Config, error) {
	fs := flag.NewFlagSet("kanban-mcp", flag.ContinueOnError)
	port := fs.Int("port", envInt("KANBAN_PORT", 8080), "HTTP server port")
	dbPath := fs.String("db", envStr("KANBAN_DB", "kanban.db"), "path to SQLite database file")
	mcpTransport := fs.String("mcp-transport", envStr("KANBAN_MCP_TRANSPORT", "stdio"), "MCP transport (stdio, http, both)")
	logLevel := fs.String("log-level", envStr("KANBAN_LOG_LEVEL", "info"), "log level (debug, info, warn, error)")
	leaseTTL := fs.Int64("lease-ttl-ms", 0, "lease TTL in milliseconds (default from LEDGERBOARD_LEASE_TTL_MS, else 900000)")

	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}

	cfg := Config{
		Port:         *port,
		DBPath:       *dbPath,
		MCPTransport: *mcpTransport,
		LogLevel:     *logLevel,
	}

	// C-CONV-10: a flag wins over the environment, which wins over the default.
	// An invalid value is a startup error naming the variable — never a silent
	// fallback, because a silently-shortened lease is a correctness bug that
	// only shows up as a stolen ticket.
	ttl := store.DefaultLeaseTTLMS
	if *leaseTTL != 0 {
		ttl = *leaseTTL
		if err := validateKnob("--lease-ttl-ms", ttl, store.MinLeaseTTLMS); err != nil {
			return Config{}, err
		}
	} else {
		var err error
		ttl, err = envInt64Strict("LEDGERBOARD_LEASE_TTL_MS", store.DefaultLeaseTTLMS, store.MinLeaseTTLMS)
		if err != nil {
			return Config{}, err
		}
	}
	cfg.LeaseTTLMS = ttl

	sweep, err := envInt64Strict("LEDGERBOARD_SWEEP_INTERVAL_MS", 60000, 1)
	if err != nil {
		return Config{}, err
	}
	cfg.SweepIntervalMS = sweep

	statuses, err := store.ParseClaimableStatuses(envStr("LEDGERBOARD_CLAIMABLE_STATUSES", "todo"))
	if err != nil {
		return Config{}, fmt.Errorf("invalid LEDGERBOARD_CLAIMABLE_STATUSES: %w", err)
	}
	cfg.ClaimableStatuses = statuses

	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) validate() error {
	switch c.MCPTransport {
	case "stdio", "http", "both":
	default:
		return fmt.Errorf("invalid MCP transport %q: must be stdio, http, or both", c.MCPTransport)
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("invalid log level %q: must be debug, info, warn, or error", c.LogLevel)
	}
	return validateKnob("LEDGERBOARD_LEASE_TTL_MS", c.LeaseTTLMS, store.MinLeaseTTLMS)
}

// validateKnob rejects a non-positive value and a value below the contract floor.
func validateKnob(name string, value, min int64) error {
	if value <= 0 {
		return fmt.Errorf("invalid %s %d: must be a positive integer", name, value)
	}
	if value < min {
		return fmt.Errorf("invalid %s %d: must be at least %d", name, value, min)
	}
	return nil
}

// envStr returns the value of the environment variable key, or fallback if unset/empty.
func envStr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// envInt returns the integer value of the environment variable key, or fallback if unset/empty/invalid.
func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

// envInt64Strict returns the value of key, or fallback when it is unset. An
// unparsable or out-of-range value is an error naming the variable (C-CONV-10),
// not a fallback: a typo in a timing knob must stop the server, not change its
// behaviour quietly.
func envInt64Strict(key string, fallback, min int64) (int64, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: must be an integer number of milliseconds", key, raw)
	}
	if err := validateKnob(key, n, min); err != nil {
		return 0, err
	}
	return n, nil
}
