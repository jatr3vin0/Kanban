package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"time"

	"github.com/rgracey/kanban-mcp/internal/clocksource"
	"github.com/rgracey/kanban-mcp/internal/models"
)

// SQLiteStore implements the Store interface using SQLite.
//
// The db handle is the single writer pool (C-CONV-3, C-CONV-9): every mutation
// runs inside BEGIN IMMEDIATE on it. readDB is the separate read pool; it is
// never used for a mutation, and it falls back to db when a caller wires only
// one handle (as every upstream call site does).
type SQLiteStore struct {
	db     *sql.DB
	readDB *sql.DB

	// clock is the only source of now_ms (C-CONV-2). RealClock in production;
	// the fake-clock seam substitutes this and nothing else (C-CONV-11).
	clock clocksource.Clock
	// ttlMS is LEDGERBOARD_LEASE_TTL_MS, read at the moment a lease is issued or
	// renewed (C-CONV-10). Changing it never rewrites an existing lease.
	ttlMS int64
	// claimableStatuses is LEDGERBOARD_CLAIMABLE_STATUSES (C-CLAIM-3).
	claimableStatuses []models.Status
}

// Option configures a SQLiteStore.
type Option func(*SQLiteStore)

// WithClock injects the now_ms source. Tests use it directly; the server binary
// builds its clock from the environment (clocksource.FromEnv).
func WithClock(c clocksource.Clock) Option {
	return func(s *SQLiteStore) {
		if c != nil {
			s.clock = c
		}
	}
}

// WithLeaseTTLMS sets the lease TTL in milliseconds (C-CONV-10).
func WithLeaseTTLMS(ms int64) Option {
	return func(s *SQLiteStore) {
		if ms >= MinLeaseTTLMS {
			s.ttlMS = ms
		}
	}
}

// WithClaimableStatuses sets the claimable status set (C-CLAIM-3).
func WithClaimableStatuses(statuses []models.Status) Option {
	return func(s *SQLiteStore) {
		if len(statuses) > 0 {
			s.claimableStatuses = statuses
		}
	}
}

// WithReadPool wires the contract's separate read pool (C-CONV-9).
func WithReadPool(pool *sql.DB) Option {
	return func(s *SQLiteStore) {
		if pool != nil {
			s.readDB = pool
		}
	}
}

// NewSQLiteStore creates a new SQLiteStore with the given database connection.
//
// The defaults are the contract's defaults: the real clock, a 900000 ms (15
// minute) lease TTL and `todo` as the only claimable status. They are options
// rather than required arguments so that every existing call site keeps
// compiling unchanged.
func NewSQLiteStore(db *sql.DB, opts ...Option) *SQLiteStore {
	s := &SQLiteStore{
		db:                db,
		readDB:            db,
		clock:             clocksource.RealClock{},
		ttlMS:             DefaultLeaseTTLMS,
		claimableStatuses: []models.Status{models.StatusTodo},
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// LeaseTTLMS reports the configured lease TTL, for tests and diagnostics.
func (s *SQLiteStore) LeaseTTLMS() int64 { return s.ttlMS }

// newUUID generates a new UUID v4 string using crypto/rand.
func newUUID() string {
	uuid := make([]byte, 16)
	_, err := rand.Read(uuid)
	if err != nil {
		panic(err)
	}
	// Set version 4 (0100xxxx)
	uuid[6] = (uuid[6] & 0x0F) | 0x40
	// Set variant (10xxxxxx)
	uuid[8] = (uuid[8] & 0x3F) | 0x80
	return hex.EncodeToString(uuid)
}

// timeToRFC3339 converts time.Time to RFC3339 string.
func timeToRFC3339(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// rfc3339ToTime parses RFC3339 string to time.Time.
func rfc3339ToTime(s string) (time.Time, error) {
	return time.Parse(time.RFC3339, s)
}

// assert at compile-time that SQLiteStore implements Store
var _ Store = (*SQLiteStore)(nil)
