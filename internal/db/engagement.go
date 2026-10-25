package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// CurveBucket is one interval of the engagement curve, anchored at the campaign's
// started_at. HoursSinceSend is the bucket's start offset in whole hours since
// send; WidthHours is 1 for the hourly buckets (first 48h) or 24 for the daily
// buckets thereafter. Opens and Clicks are total event counts (COUNT(*)) in the
// interval — so summing them across all buckets reproduces the campaign totals.
type CurveBucket struct {
	HoursSinceSend int   `json:"hours_since_send"`
	WidthHours     int   `json:"width_hours"`
	Opens          int64 `json:"opens"`
	Clicks         int64 `json:"clicks"`
}

// CampaignEngagementCurve holds metric #4 for a single campaign: opens and
// clicks bucketed by elapsed time since started_at.
//
// Buckets with no events do not appear (sparse series) — acceptable for a curve
// the frontend plots against an explicit hours-since-send axis. TotalOpens and
// TotalClicks are the curve's own sums and exist for a self-consistency check.
type CampaignEngagementCurve struct {
	CampaignID int        `json:"campaign_id"`
	Name       string     `json:"name"`
	StartedAt  *time.Time `json:"started_at"` // zero anchor; null = never sent

	Buckets     []CurveBucket `json:"buckets"`
	TotalOpens  int64         `json:"total_opens"`
	TotalClicks int64         `json:"total_clicks"`

	// Note explains a degraded or empty result (never sent, or tables absent).
	Note string `json:"note,omitempty"`
}

// CampaignEngagementCurve returns the opens/clicks time series for one campaign,
// bucketed hourly for the first 48h since started_at and daily afterwards.
//
// Edge cases & gating:
//   - campaign unknown or excluded optin → ErrCampaignNotFound.
//   - started_at null (never sent / draft) → empty curve + note (no anchor).
//   - campaign_views absent (HasCampaignViews false) → opens omitted from the
//     union; clicks absent (HasLinks false) → clicks omitted. Both absent →
//     empty curve + note.
//
// Counts are COUNT(*) (total events), so individual tracking is not required:
// the curve plots event volume over time, and its bucket sums equal the
// campaign's total opens/clicks.
func (db *DB) CampaignEngagementCurve(ctx context.Context, id int, includeOptin bool) (*CampaignEngagementCurve, error) {
	// Validate existence / optin exclusion and fetch the zero anchor.
	var name string
	var startedAt *time.Time
	err := db.Pool.QueryRow(ctx,
		`SELECT name, started_at FROM campaigns WHERE id = $1 AND ($2 OR type <> 'optin')`,
		id, includeOptin).Scan(&name, &startedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrCampaignNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("campaign lookup: %w", err)
	}

	m := &CampaignEngagementCurve{
		CampaignID: id,
		Name:       name,
		StartedAt:  startedAt,
		Buckets:    []CurveBucket{},
	}

	if startedAt == nil {
		m.Note = "campaign not yet sent (no started_at) — no engagement curve"
		return m, nil
	}

	// Build the event union from whichever event tables are present.
	var sources []string
	if db.Caps.HasCampaignViews {
		sources = append(sources,
			`SELECT created_at, 1 AS is_open, 0 AS is_click FROM campaign_views WHERE campaign_id = $1`)
	}
	if db.Caps.HasLinks {
		sources = append(sources,
			`SELECT created_at, 0 AS is_open, 1 AS is_click FROM link_clicks WHERE campaign_id = $1`)
	}
	if len(sources) == 0 {
		m.Note = "engagement curve unavailable (campaign_views and links tables absent)"
		return m, nil
	}

	// Bucket each event by whole hours since started_at: hourly (<48h), then
	// daily. The same expression is applied to opens and clicks so they align on
	// identical bucket boundaries. UNION ALL preserves one row per event, so the
	// SUMs are COUNT(*) totals.
	query := fmt.Sprintf(`
SELECT
    CASE WHEN h < 48 THEN floor(h)::int
         ELSE (floor(h / 24.0) * 24)::int END AS hours_since_send,
    SUM(is_open)::bigint  AS opens,
    SUM(is_click)::bigint AS clicks
FROM (
    SELECT EXTRACT(EPOCH FROM (created_at - $2::timestamptz)) / 3600.0 AS h,
           is_open, is_click
    FROM ( %s ) e
) x
GROUP BY hours_since_send
ORDER BY hours_since_send`, strings.Join(sources, "\n    UNION ALL\n    "))

	rows, err := db.Pool.Query(ctx, query, id, *startedAt)
	if err != nil {
		return nil, fmt.Errorf("engagement curve query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var b CurveBucket
		if err := rows.Scan(&b.HoursSinceSend, &b.Opens, &b.Clicks); err != nil {
			return nil, fmt.Errorf("scan curve bucket: %w", err)
		}
		b.WidthHours = 1
		if b.HoursSinceSend >= 48 {
			b.WidthHours = 24
		}
		m.Buckets = append(m.Buckets, b)
		m.TotalOpens += b.Opens
		m.TotalClicks += b.Clicks
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate curve buckets: %w", err)
	}

	return m, nil
}
