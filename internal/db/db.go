// Package db provides a read-only connection pool to listmonk's Postgres
// database and a startup capability probe. It issues SELECT only; the pool is
// configured with default_transaction_read_only = on as defense in depth on
// top of the read-only role the operator provisions.
package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Capabilities records which optional listmonk features are present in the
// target database. Metric queries gate themselves on these flags rather than
// assuming a table or column exists.
type Capabilities struct {
	HasBounces         bool // bounces table present
	HasLinks           bool // links table present
	HasCampaignViews   bool // campaign_views table present
	HasSubscriberLists bool // subscriber_lists table present
	IndividualTracking bool // campaign_views/link_clicks carry non-null subscriber_id
}

// DB is a read-only handle to listmonk's database plus the probed capabilities.
type DB struct {
	Pool *pgxpool.Pool
	Caps Capabilities
}

// New builds the read-only pool, verifies the target looks like a listmonk
// database, and probes its capabilities. It fails clearly if the campaigns
// table is absent. The caller owns the returned DB and must Close it.
func New(ctx context.Context, databaseURL string) (*DB, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}

	// A read-only analytics dashboard needs only a handful of connections.
	cfg.MaxConns = 4

	// Defense in depth: force every session read-only at the protocol level,
	// independent of the role's own privileges. Sent in the startup packet so
	// it is in effect before the first query.
	cfg.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create connection pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect to database: %w", err)
	}

	if err := ensureListmonk(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}

	caps, err := probe(ctx, pool)
	if err != nil {
		pool.Close()
		return nil, err
	}

	return &DB{Pool: pool, Caps: caps}, nil
}

// Close releases the pool's connections.
func (db *DB) Close() {
	db.Pool.Close()
}

// tableExists reports whether public.<name> exists, using to_regclass (returns
// NULL for an absent relation) — a single, cheap, read-only lookup.
func tableExists(ctx context.Context, pool *pgxpool.Pool, name string) (bool, error) {
	var reg *string
	if err := pool.QueryRow(ctx, "SELECT to_regclass($1)::text", "public."+name).Scan(&reg); err != nil {
		return false, fmt.Errorf("check for %s table: %w", name, err)
	}
	return reg != nil, nil
}

// ensureListmonk fails clearly if the core campaigns table is missing, which
// means DATABASE_URL does not point at a listmonk database.
func ensureListmonk(ctx context.Context, pool *pgxpool.Pool) error {
	ok, err := tableExists(ctx, pool, "campaigns")
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("campaigns table not found: DATABASE_URL does not appear to point at a listmonk database")
	}
	return nil
}

// probe inspects the database for optional tables and infers individual
// tracking. campaigns is assumed present (ensureListmonk ran first).
func probe(ctx context.Context, pool *pgxpool.Pool) (Capabilities, error) {
	var caps Capabilities
	var err error

	if caps.HasBounces, err = tableExists(ctx, pool, "bounces"); err != nil {
		return caps, err
	}
	if caps.HasLinks, err = tableExists(ctx, pool, "links"); err != nil {
		return caps, err
	}
	if caps.HasCampaignViews, err = tableExists(ctx, pool, "campaign_views"); err != nil {
		return caps, err
	}
	if caps.HasSubscriberLists, err = tableExists(ctx, pool, "subscriber_lists"); err != nil {
		return caps, err
	}

	if caps.IndividualTracking, err = probeIndividualTracking(ctx, pool, caps); err != nil {
		return caps, err
	}

	return caps, nil
}

// probeIndividualTracking reports whether per-subscriber tracking is on, i.e.
// whether any open or click event carries a non-null subscriber_id. EXISTS
// short-circuits on the first matching row. Only existing tables are queried;
// the table names are fixed literals, so the format string is injection-safe.
func probeIndividualTracking(ctx context.Context, pool *pgxpool.Pool, caps Capabilities) (bool, error) {
	hasLinkClicks, err := tableExists(ctx, pool, "link_clicks")
	if err != nil {
		return false, err
	}

	sources := []struct {
		table   string
		present bool
	}{
		{"campaign_views", caps.HasCampaignViews},
		{"link_clicks", hasLinkClicks},
	}

	for _, s := range sources {
		if !s.present {
			continue
		}
		var found bool
		q := fmt.Sprintf("SELECT EXISTS (SELECT 1 FROM %s WHERE subscriber_id IS NOT NULL)", s.table)
		if err := pool.QueryRow(ctx, q).Scan(&found); err != nil {
			return false, fmt.Errorf("probe individual tracking on %s: %w", s.table, err)
		}
		if found {
			return true, nil
		}
	}

	return false, nil
}
