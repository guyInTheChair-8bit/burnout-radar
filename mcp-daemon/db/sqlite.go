// Package db provides the SQLite persistence layer for BurnoutRadar MCP Daemon.
//
// ZKA GUARANTEE: This package stores ONLY final mathematical scalars. No user
// identifiers, raw message counts, or any form of PII is ever written here.
// The pipeline layer has already destroyed all identity information before
// calling into this package.
package db

import (
	"database/sql"
	_ "embed" // required for //go:embed directive
	"fmt"

	_ "github.com/mattn/go-sqlite3" // registers the "sqlite3" driver with database/sql
)

// schema holds the CREATE TABLE statement read at compile time from schema.sql.
// Using go:embed avoids reading a file at runtime and ensures the schema is
// always bundled with the binary.
//
//go:embed schema.sql
var schema string

// ChannelMetrics mirrors the channel_metrics table row.
// ZKA: All fields are mathematical scalars or non-identifying metadata.
// channel_id identifies a *team channel*, not an individual.
type ChannelMetrics struct {
	ID            int64   // Auto-incremented row ID
	Date          string  // YYYY-MM-DD (UTC)
	ChannelID     string  // Slack channel ID (not a person)
	GiniCoeff     float64 // Gini coefficient [0,1]
	ParetoShare   float64 // Top-20% message share [0,100]
	ZScore        float64 // Standard deviations from historical mean
	DMSharePct    float64 // Percentage of DM messages
	AvgWordCount  float64 // Average words per message
}

// DB wraps a *sql.DB connection with domain-specific helper methods.
type DB struct {
	conn *sql.DB
}

// NewDB opens (or creates) a SQLite database at the given path, applies the
// embedded schema, and returns a ready-to-use DB handle.
//
// The ":memory:" path is supported for testing.
func NewDB(path string) (*DB, error) {
	conn, err := sql.Open("sqlite3", path)
	if err != nil {
		return nil, fmt.Errorf("db: failed to open sqlite at %q: %w", path, err)
	}

	// Verify the connection is actually usable (sql.Open is lazy).
	if err := conn.Ping(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("db: ping failed for %q: %w", path, err)
	}

	// Apply the embedded schema. CREATE TABLE IF NOT EXISTS makes this idempotent.
	if _, err := conn.Exec(schema); err != nil {
		conn.Close()
		return nil, fmt.Errorf("db: failed to apply schema: %w", err)
	}

	return &DB{conn: conn}, nil
}

// Close releases the underlying SQLite connection.
func (d *DB) Close() error {
	if d.conn != nil {
		return d.conn.Close()
	}
	return nil
}

// InsertMetrics writes (or replaces) a set of channel metrics for a given date.
//
// Uses INSERT OR REPLACE to handle the UNIQUE(date, channel_id) constraint,
// implementing an idempotent upsert — safe to call multiple times per window.
//
// ZKA: Only mathematical scalars and the non-PII channel_id are written.
func (d *DB) InsertMetrics(
	date, channelID string,
	gini, pareto, zScore, dmShare, avgWord float64,
) error {
	const query = `
		INSERT OR REPLACE INTO channel_metrics
			(date, channel_id, gini_coeff, pareto_share, z_score, dm_share_pct, avg_word_count)
		VALUES (?, ?, ?, ?, ?, ?, ?)`

	_, err := d.conn.Exec(query, date, channelID, gini, pareto, zScore, dmShare, avgWord)
	if err != nil {
		return fmt.Errorf("db: InsertMetrics(%s, %s): %w", date, channelID, err)
	}
	return nil
}

// GetMetrics retrieves the stored channel metrics for a specific date and channel.
// Returns (nil, nil) if no row is found (caller should check for nil pointer).
func (d *DB) GetMetrics(date, channelID string) (*ChannelMetrics, error) {
	const query = `
		SELECT id, date, channel_id, gini_coeff, pareto_share, z_score, dm_share_pct, avg_word_count
		FROM channel_metrics
		WHERE date = ? AND channel_id = ?`

	row := d.conn.QueryRow(query, date, channelID)

	m := &ChannelMetrics{}
	err := row.Scan(
		&m.ID,
		&m.Date,
		&m.ChannelID,
		&m.GiniCoeff,
		&m.ParetoShare,
		&m.ZScore,
		&m.DMSharePct,
		&m.AvgWordCount,
	)
	if err == sql.ErrNoRows {
		return nil, nil // caller should check for nil to mean "not found"
	}
	if err != nil {
		return nil, fmt.Errorf("db: GetMetrics(%s, %s): %w", date, channelID, err)
	}
	return m, nil
}
