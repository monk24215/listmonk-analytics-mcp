package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrInvalidOption signals a caller-supplied option (sort/order) that failed
// validation. Handlers map it to 400; other errors are 500.
var ErrInvalidOption = errors.New("invalid query option")

// CampaignRow is one row of the campaign comparison table.
//
// Counts are pointers so "feature unavailable" (capability absent → null) is
// distinct from "zero events" (capability present → 0). Rates are null when
// sent = 0 (no denominator) or the underlying capability is absent.
type CampaignRow struct {
	ID       int        `json:"id"`
	Name     string     `json:"name"`
	Status   string     `json:"status"`
	Type     string     `json:"type"`
	Sent     int        `json:"sent"`
	SentDate *time.Time `json:"sent_date"` // campaigns.started_at; null if never sent

	TotalOpens   *int64 `json:"total_opens"`
	UniqueOpens  *int64 `json:"unique_opens"`
	TotalClicks  *int64 `json:"total_clicks"`
	UniqueClicks *int64 `json:"unique_clicks"`
	Bounces      *int64 `json:"bounces"`    // soft + hard only
	Complaints   *int64 `json:"complaints"` // bounce_type = 'complaint', kept separate

	OpenRate      *float64 `json:"open_rate"`      // unique opens / sent
	ClickRate     *float64 `json:"click_rate"`     // unique clicks / sent
	BounceRate    *float64 `json:"bounce_rate"`    // (soft+hard) / sent
	ComplaintRate *float64 `json:"complaint_rate"` // complaints / sent
}

// CampaignComparisonOptions controls filtering and sorting of the table.
type CampaignComparisonOptions struct {
	IncludeOptin bool   // include type='optin' campaigns (excluded by default)
	Status       string // filter by campaign status; "" = all
	SortBy       string // whitelisted sort key; "" = sent_date
	Order        string // "asc" | "desc"; "" = desc
}

// campaignSortColumns whitelists user-supplied sort keys to SQL expressions,
// preventing injection via ORDER BY (which cannot be parameterized).
var campaignSortColumns = map[string]string{
	"id":             "c.id",
	"name":           "c.name",
	"sent":           "c.sent",
	"sent_date":      "c.started_at",
	"open_rate":      "open_rate",
	"click_rate":     "click_rate",
	"bounce_rate":    "bounce_rate",
	"complaint_rate": "complaint_rate",
}

// CampaignComparison returns one row per campaign with engagement rates.
//
// Row multiplication is avoided by aggregating each event source
// (campaign_views, link_clicks, bounces) in its own GROUP BY subquery before
// joining: every subquery yields at most one row per campaign_id, so a
// campaign with many opens AND many clicks does not produce a cartesian blowup.
// Event sources are included only when their capability is present.
func (db *DB) CampaignComparison(ctx context.Context, opts CampaignComparisonOptions) ([]CampaignRow, error) {
	sortCol := "c.started_at"
	if opts.SortBy != "" {
		col, ok := campaignSortColumns[opts.SortBy]
		if !ok {
			return nil, fmt.Errorf("%w: sort %q", ErrInvalidOption, opts.SortBy)
		}
		sortCol = col
	}

	order := "DESC"
	switch strings.ToLower(opts.Order) {
	case "", "desc":
		order = "DESC"
	case "asc":
		order = "ASC"
	default:
		return nil, fmt.Errorf("%w: order %q", ErrInvalidOption, opts.Order)
	}

	// Opens.
	totalOpens, uniqueOpens, openRate, opensJoin := "NULL::bigint", "NULL::bigint", "NULL::float8", ""
	if db.Caps.HasCampaignViews {
		totalOpens = "COALESCE(v.total_opens, 0)"
		uniqueOpens = "COALESCE(v.unique_opens, 0)"
		openRate = "CASE WHEN c.sent > 0 THEN COALESCE(v.unique_opens, 0)::float8 / c.sent END"
		opensJoin = `LEFT JOIN (
			SELECT campaign_id,
			       COUNT(*)                       AS total_opens,
			       COUNT(DISTINCT subscriber_id)  AS unique_opens
			FROM campaign_views
			GROUP BY campaign_id
		) v ON v.campaign_id = c.id`
	}

	// Clicks. listmonk creates links and link_clicks together, so HasLinks
	// implies link_clicks is present.
	totalClicks, uniqueClicks, clickRate, clicksJoin := "NULL::bigint", "NULL::bigint", "NULL::float8", ""
	if db.Caps.HasLinks {
		totalClicks = "COALESCE(cl.total_clicks, 0)"
		uniqueClicks = "COALESCE(cl.unique_clicks, 0)"
		clickRate = "CASE WHEN c.sent > 0 THEN COALESCE(cl.unique_clicks, 0)::float8 / c.sent END"
		clicksJoin = `LEFT JOIN (
			SELECT campaign_id,
			       COUNT(*)                       AS total_clicks,
			       COUNT(DISTINCT subscriber_id)  AS unique_clicks
			FROM link_clicks
			WHERE campaign_id IS NOT NULL
			GROUP BY campaign_id
		) cl ON cl.campaign_id = c.id`
	}

	// Bounces & complaints, kept separate. campaign_id is nullable in bounces;
	// untied bounces are excluded from per-campaign rates.
	bounces, complaints, bounceRate, complaintRate, bouncesJoin :=
		"NULL::bigint", "NULL::bigint", "NULL::float8", "NULL::float8", ""
	if db.Caps.HasBounces {
		bounces = "COALESCE(b.soft_hard, 0)"
		complaints = "COALESCE(b.complaints, 0)"
		bounceRate = "CASE WHEN c.sent > 0 THEN COALESCE(b.soft_hard, 0)::float8 / c.sent END"
		complaintRate = "CASE WHEN c.sent > 0 THEN COALESCE(b.complaints, 0)::float8 / c.sent END"
		bouncesJoin = `LEFT JOIN (
			SELECT campaign_id,
			       COUNT(*) FILTER (WHERE type IN ('soft','hard')) AS soft_hard,
			       COUNT(*) FILTER (WHERE type = 'complaint')      AS complaints
			FROM bounces
			WHERE campaign_id IS NOT NULL
			GROUP BY campaign_id
		) b ON b.campaign_id = c.id`
	}

	query := fmt.Sprintf(`
SELECT
    c.id,
    c.name,
    c.status::text,
    c.type::text,
    c.sent,
    c.started_at,
    %s AS total_opens,
    %s AS unique_opens,
    %s AS total_clicks,
    %s AS unique_clicks,
    %s AS bounces,
    %s AS complaints,
    %s AS open_rate,
    %s AS click_rate,
    %s AS bounce_rate,
    %s AS complaint_rate
FROM campaigns c
%s
%s
%s
WHERE ($1 OR c.type <> 'optin')
  AND ($2 = '' OR c.status::text = $2)
ORDER BY %s %s NULLS LAST, c.id ASC`,
		totalOpens, uniqueOpens, totalClicks, uniqueClicks, bounces, complaints,
		openRate, clickRate, bounceRate, complaintRate,
		opensJoin, clicksJoin, bouncesJoin,
		sortCol, order,
	)

	rows, err := db.Pool.Query(ctx, query, opts.IncludeOptin, opts.Status)
	if err != nil {
		return nil, fmt.Errorf("campaign comparison query: %w", err)
	}
	defer rows.Close()

	var out []CampaignRow
	for rows.Next() {
		var r CampaignRow
		if err := rows.Scan(
			&r.ID, &r.Name, &r.Status, &r.Type, &r.Sent, &r.SentDate,
			&r.TotalOpens, &r.UniqueOpens, &r.TotalClicks, &r.UniqueClicks,
			&r.Bounces, &r.Complaints,
			&r.OpenRate, &r.ClickRate, &r.BounceRate, &r.ComplaintRate,
		); err != nil {
			return nil, fmt.Errorf("scan campaign row: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate campaign rows: %w", err)
	}

	return out, nil
}
