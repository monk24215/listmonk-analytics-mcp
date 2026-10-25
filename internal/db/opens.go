package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ErrCampaignNotFound is returned when no campaign matches the requested id
// (including the case where it is an optin campaign and optin is excluded).
var ErrCampaignNotFound = errors.New("campaign not found")

// CampaignOpenMetrics holds metric #1 for a single campaign.
//
// Tiering: UniqueOpens and OpenRate are the headline figures. TotalOpens and
// OpenRatio are diagnostics intended for the campaign drill-down, not the main
// dashboard — they are returned in the payload but must not be promoted to a
// headline by the frontend.
type CampaignOpenMetrics struct {
	CampaignID int    `json:"campaign_id"`
	Name       string `json:"name"`
	Sent       int    `json:"sent"`

	IndividualTracking bool     `json:"individual_tracking"`
	UniqueOpens        *int64   `json:"unique_opens"` // headline numerator; null when unavailable
	OpenRate           *float64 `json:"open_rate"`    // headline: unique/sent; null if sent=0 or unavailable

	// Diagnostics — campaign detail/drill-down only.
	TotalOpens *int64   `json:"total_opens"`
	OpenRatio  *float64 `json:"open_ratio"` // total/unique; re-open / anomaly signal

	// Note explains a degraded result (tracking off, or table absent).
	Note string `json:"note,omitempty"`
}

// CampaignOpenMetrics computes open metrics for one campaign in a single scan
// of campaign_views (COUNT(*) and COUNT(DISTINCT subscriber_id) together).
//
// Capability gating:
//   - campaign_views absent  → opens unavailable (all null + note).
//   - IndividualTracking off → unique is meaningless; total is reported and
//     unique is marked unavailable (never faked).
func (db *DB) CampaignOpenMetrics(ctx context.Context, id int, includeOptin bool) (*CampaignOpenMetrics, error) {
	totalSel, uniqueSel, viewsJoin := "NULL::bigint", "NULL::bigint", ""
	if db.Caps.HasCampaignViews {
		totalSel = "COALESCE(v.total_opens, 0)"
		uniqueSel = "COALESCE(v.unique_opens, 0)"
		viewsJoin = `LEFT JOIN (
			SELECT campaign_id,
			       COUNT(*)                      AS total_opens,
			       COUNT(DISTINCT subscriber_id) AS unique_opens
			FROM campaign_views
			WHERE campaign_id = $1
			GROUP BY campaign_id
		) v ON v.campaign_id = c.id`
	}

	query := fmt.Sprintf(`
SELECT c.id, c.name, c.sent,
       %s AS total_opens,
       %s AS unique_opens
FROM campaigns c
%s
WHERE c.id = $1 AND ($2 OR c.type <> 'optin')`,
		totalSel, uniqueSel, viewsJoin)

	var m CampaignOpenMetrics
	var total, unique *int64
	err := db.Pool.QueryRow(ctx, query, id, includeOptin).
		Scan(&m.CampaignID, &m.Name, &m.Sent, &total, &unique)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrCampaignNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("open metrics query: %w", err)
	}

	m.IndividualTracking = db.Caps.IndividualTracking

	if !db.Caps.HasCampaignViews {
		m.Note = "open tracking unavailable (campaign_views table absent)"
		return &m, nil
	}

	// campaign_views present → total opens is a real count.
	m.TotalOpens = total

	if !db.Caps.IndividualTracking {
		// Unique is meaningless without per-subscriber tracking. Report total
		// only; do not fabricate a unique figure or rate.
		m.Note = "opens (unique unavailable — individual tracking off)"
		return &m, nil
	}

	m.UniqueOpens = unique
	if m.Sent > 0 && unique != nil {
		rate := float64(*unique) / float64(m.Sent)
		m.OpenRate = &rate
	}
	if unique != nil && *unique > 0 && total != nil {
		ratio := float64(*total) / float64(*unique)
		m.OpenRatio = &ratio
	}

	return &m, nil
}
