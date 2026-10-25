package db

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// EngagementScore is one subscriber's engagement profile over the window.
// Email and Name are PII and are only ever reached through the auth-gated,
// IndividualTracking-gated endpoint.
type EngagementScore struct {
	SubscriberID int       `json:"subscriber_id"`
	Email        string    `json:"email"`
	Name         string    `json:"name"`
	Opens        int64     `json:"opens"`
	Clicks       int64     `json:"clicks"`
	Frequency    int64     `json:"frequency"`        // opens + clicks within window
	LastSeen     time.Time `json:"last_seen"`        // recency: most recent open/click
	DaysSinceLast float64  `json:"days_since_last"`
	Score        float64   `json:"score"`
}

// EngagementScoring is the paginated subscriber-engagement leaderboard.
type EngagementScoring struct {
	IndividualTracking bool              `json:"individual_tracking"`
	WindowDays         int               `json:"window_days"`
	Limit              int               `json:"limit"`
	Offset             int               `json:"offset"`
	Subscribers        []EngagementScore `json:"subscribers"`

	// Note explains a degraded result (tracking off — the hard gate).
	Note string `json:"note,omitempty"`
}

const (
	scoringDefaultLimit = 50
	scoringMaxLimit     = 500
)

// SubscriberEngagement scores subscribers by recency and frequency of opens and
// clicks within the last windowDays, returning the top page ordered by score.
//
// Scoring (simple, explainable — not ML):
//   - frequency = opens + clicks (clicks weighted ×3 in the score, as a click is
//     a stronger engagement signal than an open)
//   - recency factor = (windowDays − days_since_last + 1) / windowDays, so the
//     same activity scores higher the more recently it happened
//   - score = (opens + clicks×3) × recency_factor
//
// HARD GATE (IndividualTracking): without per-subscriber tracking there is no
// subscriber_id to score by, so the feature returns a note and no data. (The
// auth/PII hard gate is enforced in the API handler, which refuses to serve
// when no dashboard credentials are configured.)
//
// Performance: all aggregation runs in SQL and only one page (LIMIT/OFFSET) is
// returned — subscriber rows never stream into Go. Null subscriber_id rows
// (individual tracking off, or subscriber deleted) are excluded; blocklisted
// subscribers are excluded.
func (db *DB) SubscriberEngagement(ctx context.Context, windowDays, limit, offset int) (*EngagementScoring, error) {
	if windowDays <= 0 {
		windowDays = 90
	}
	if limit <= 0 {
		limit = scoringDefaultLimit
	}
	if limit > scoringMaxLimit {
		limit = scoringMaxLimit
	}
	if offset < 0 {
		offset = 0
	}

	res := &EngagementScoring{
		IndividualTracking: db.Caps.IndividualTracking,
		WindowDays:         windowDays,
		Limit:              limit,
		Offset:             offset,
		Subscribers:        []EngagementScore{},
	}

	if !db.Caps.IndividualTracking {
		res.Note = "subscriber engagement unavailable — individual tracking is off (no subscriber_id to score by)"
		return res, nil
	}

	// Event sources within the window, from whichever tracking tables exist.
	var sources []string
	if db.Caps.HasCampaignViews {
		sources = append(sources, `SELECT subscriber_id, created_at, 1 AS is_open, 0 AS is_click
		FROM campaign_views
		WHERE subscriber_id IS NOT NULL AND created_at >= now() - make_interval(days => $1)`)
	}
	if db.Caps.HasLinks {
		sources = append(sources, `SELECT subscriber_id, created_at, 0 AS is_open, 1 AS is_click
		FROM link_clicks
		WHERE subscriber_id IS NOT NULL AND created_at >= now() - make_interval(days => $1)`)
	}
	if len(sources) == 0 {
		res.Note = "subscriber engagement unavailable (no tracking tables present)"
		return res, nil
	}

	query := fmt.Sprintf(`
WITH events AS (
	%s
),
agg AS (
	SELECT subscriber_id,
	       SUM(is_open)::bigint  AS opens,
	       SUM(is_click)::bigint AS clicks,
	       MAX(created_at)       AS last_seen
	FROM events
	GROUP BY subscriber_id
)
SELECT a.subscriber_id, s.email, s.name,
       a.opens, a.clicks, (a.opens + a.clicks) AS frequency,
       a.last_seen,
       EXTRACT(EPOCH FROM (now() - a.last_seen)) / 86400.0 AS days_since_last,
       (a.opens + a.clicks * 3)::float8
         * (($1::float8 - EXTRACT(EPOCH FROM (now() - a.last_seen)) / 86400.0 + 1) / $1::float8)
         AS score
FROM agg a
JOIN subscribers s ON s.id = a.subscriber_id
WHERE s.status <> 'blocklisted'
ORDER BY score DESC, a.last_seen DESC
LIMIT $2 OFFSET $3`, strings.Join(sources, "\n\tUNION ALL\n\t"))

	rows, err := db.Pool.Query(ctx, query, windowDays, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("subscriber engagement query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var e EngagementScore
		if err := rows.Scan(&e.SubscriberID, &e.Email, &e.Name,
			&e.Opens, &e.Clicks, &e.Frequency, &e.LastSeen, &e.DaysSinceLast, &e.Score); err != nil {
			return nil, fmt.Errorf("scan engagement row: %w", err)
		}
		res.Subscribers = append(res.Subscribers, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate engagement rows: %w", err)
	}

	return res, nil
}
