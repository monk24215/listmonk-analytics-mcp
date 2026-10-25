package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// CampaignClickMetrics holds metric #2 for a single campaign.
//
// Tiering: UniqueClicks and ClickRate are the headline figures. TotalClicks is
// a diagnostic intended for the campaign drill-down, not the main dashboard —
// it is returned in the payload but must not be promoted to a headline by the
// frontend.
type CampaignClickMetrics struct {
	CampaignID int    `json:"campaign_id"`
	Name       string `json:"name"`
	Sent       int    `json:"sent"`

	IndividualTracking bool     `json:"individual_tracking"`
	UniqueClicks       *int64   `json:"unique_clicks"` // headline numerator; null when unavailable
	ClickRate          *float64 `json:"click_rate"`    // headline: unique/sent; null if sent=0 or unavailable

	// Diagnostic — campaign detail/drill-down only.
	TotalClicks *int64 `json:"total_clicks"`

	// CTOR (click-to-open rate) = unique clicks / unique opens. Computed ONLY
	// when individual tracking is on and unique opens > 0; otherwise omitted
	// entirely (never faked to 0).
	CTOR *float64 `json:"ctor,omitempty"`

	// Note explains a degraded result (tracking off, or table absent).
	Note string `json:"note,omitempty"`
}

// CampaignClickMetrics computes click metrics for one campaign in a single scan
// of link_clicks (COUNT(*) and COUNT(DISTINCT subscriber_id) together), plus a
// unique-opens count from campaign_views used solely as the CTOR denominator.
//
// Capability gating:
//   - links/link_clicks absent (HasLinks false) → clicks unavailable
//     (all null + note). listmonk creates links and link_clicks together, so
//     HasLinks implies link_clicks is present.
//   - IndividualTracking off → unique is meaningless; total is reported and
//     unique/rate/CTOR are marked unavailable (never faked).
//   - CTOR additionally requires campaign_views and unique opens > 0.
func (db *DB) CampaignClickMetrics(ctx context.Context, id int, includeOptin bool) (*CampaignClickMetrics, error) {
	totalSel, uniqueSel, clicksJoin := "NULL::bigint", "NULL::bigint", ""
	if db.Caps.HasLinks {
		totalSel = "COALESCE(cl.total_clicks, 0)"
		uniqueSel = "COALESCE(cl.unique_clicks, 0)"
		clicksJoin = `LEFT JOIN (
			SELECT campaign_id,
			       COUNT(*)                      AS total_clicks,
			       COUNT(DISTINCT subscriber_id) AS unique_clicks
			FROM link_clicks
			WHERE campaign_id = $1
			GROUP BY campaign_id
		) cl ON cl.campaign_id = c.id`
	}

	// Unique opens is fetched only as the CTOR denominator.
	uniqueOpensSel, opensJoin := "NULL::bigint", ""
	if db.Caps.HasCampaignViews {
		uniqueOpensSel = "COALESCE(v.unique_opens, 0)"
		opensJoin = `LEFT JOIN (
			SELECT campaign_id,
			       COUNT(DISTINCT subscriber_id) AS unique_opens
			FROM campaign_views
			WHERE campaign_id = $1
			GROUP BY campaign_id
		) v ON v.campaign_id = c.id`
	}

	query := fmt.Sprintf(`
SELECT c.id, c.name, c.sent,
       %s AS total_clicks,
       %s AS unique_clicks,
       %s AS unique_opens
FROM campaigns c
%s
%s
WHERE c.id = $1 AND ($2 OR c.type <> 'optin')`,
		totalSel, uniqueSel, uniqueOpensSel, clicksJoin, opensJoin)

	var m CampaignClickMetrics
	var total, unique, uniqueOpens *int64
	err := db.Pool.QueryRow(ctx, query, id, includeOptin).
		Scan(&m.CampaignID, &m.Name, &m.Sent, &total, &unique, &uniqueOpens)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrCampaignNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("click metrics query: %w", err)
	}

	m.IndividualTracking = db.Caps.IndividualTracking

	if !db.Caps.HasLinks {
		m.Note = "click tracking unavailable (links/link_clicks tables absent)"
		return &m, nil
	}

	// link_clicks present → total clicks is a real count.
	m.TotalClicks = total

	if !db.Caps.IndividualTracking {
		// Unique is meaningless without per-subscriber tracking. Report total
		// only; do not fabricate a unique figure, click rate, or CTOR.
		m.Note = "clicks (unique unavailable — individual tracking off)"
		return &m, nil
	}

	m.UniqueClicks = unique
	if m.Sent > 0 && unique != nil {
		rate := float64(*unique) / float64(m.Sent)
		m.ClickRate = &rate
	}
	// CTOR only when unique opens are available and non-zero (zero-guard).
	if unique != nil && uniqueOpens != nil && *uniqueOpens > 0 {
		ctor := float64(*unique) / float64(*uniqueOpens)
		m.CTOR = &ctor
	}

	return &m, nil
}
