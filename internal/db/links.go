package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// LinkClickRow is one destination URL's click counts within a campaign.
//
// UniqueClicks is a pointer so "unavailable" (individual tracking off → null)
// is distinct from "zero" — though zero per-link rows do not appear at all
// (see CampaignLinkBreakdown).
type LinkClickRow struct {
	URL          string `json:"url"`
	TotalClicks  int64  `json:"total_clicks"`
	UniqueClicks *int64 `json:"unique_clicks"` // null when individual tracking off
}

// CampaignLinkBreakdown holds metric #3 for a single campaign: per-URL click
// counts, ordered by total clicks descending.
//
// Links with zero clicks do not appear (no link_clicks rows) — acceptable for
// a "what got clicked" view. An empty Links slice with no Note therefore means
// the campaign simply had no recorded clicks.
type CampaignLinkBreakdown struct {
	CampaignID int    `json:"campaign_id"`
	Name       string `json:"name"`

	IndividualTracking bool           `json:"individual_tracking"`
	Links              []LinkClickRow `json:"links"`

	// Note explains a degraded result (tracking off, or table absent).
	Note string `json:"note,omitempty"`
}

// CampaignLinkBreakdown returns per-URL click counts for one campaign by joining
// link_clicks to links and grouping by URL. campaign_id is nullable in
// link_clicks, so the filter is always pinned to the specific campaign.
//
// Capability gating:
//   - links/link_clicks absent (HasLinks false) → breakdown unavailable
//     (empty + note). listmonk creates links and link_clicks together, so
//     HasLinks implies link_clicks is present.
//   - IndividualTracking off → unique per link is meaningless; total per link
//     is reported and unique is left null (never faked).
func (db *DB) CampaignLinkBreakdown(ctx context.Context, id int, includeOptin bool) (*CampaignLinkBreakdown, error) {
	// Validate the campaign exists and is not an excluded optin campaign. The
	// per-link aggregation alone cannot distinguish "unknown campaign" from
	// "campaign with zero clicks", so existence is checked against campaigns.
	var name string
	err := db.Pool.QueryRow(ctx,
		`SELECT name FROM campaigns WHERE id = $1 AND ($2 OR type <> 'optin')`,
		id, includeOptin).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrCampaignNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("campaign lookup: %w", err)
	}

	m := &CampaignLinkBreakdown{
		CampaignID:         id,
		Name:               name,
		IndividualTracking: db.Caps.IndividualTracking,
		Links:              []LinkClickRow{},
	}

	if !db.Caps.HasLinks {
		m.Note = "link breakdown unavailable (links/link_clicks tables absent)"
		return m, nil
	}

	// Unique per link is meaningful only with per-subscriber tracking; otherwise
	// select NULL rather than a misleading distinct-of-nulls count.
	uniqueSel := "NULL::bigint"
	if db.Caps.IndividualTracking {
		uniqueSel = "COUNT(DISTINCT lc.subscriber_id)"
	} else {
		m.Note = "unique per link unavailable — individual tracking off"
	}

	query := fmt.Sprintf(`
SELECT l.url,
       COUNT(*)  AS total_clicks,
       %s        AS unique_clicks
FROM link_clicks lc
JOIN links l ON l.id = lc.link_id
WHERE lc.campaign_id = $1
GROUP BY l.url
ORDER BY total_clicks DESC, l.url ASC`, uniqueSel)

	rows, err := db.Pool.Query(ctx, query, id)
	if err != nil {
		return nil, fmt.Errorf("link breakdown query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var r LinkClickRow
		if err := rows.Scan(&r.URL, &r.TotalClicks, &r.UniqueClicks); err != nil {
			return nil, fmt.Errorf("scan link row: %w", err)
		}
		m.Links = append(m.Links, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate link rows: %w", err)
	}

	return m, nil
}
