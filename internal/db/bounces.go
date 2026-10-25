package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// CampaignBounceMetrics holds metric #5 for a single campaign.
//
// CRITICAL: complaints (bounce_type='complaint') are kept strictly separate from
// soft/hard bounces — Complaints and ComplaintRate are never folded into Bounces
// or BounceRate. A complaint rate is a deliverability red flag in its own right.
type CampaignBounceMetrics struct {
	CampaignID int    `json:"campaign_id"`
	Name       string `json:"name"`
	Sent       int    `json:"sent"`

	HasBounces bool `json:"has_bounces"` // false → counts/rates below are null

	// Soft + hard only. Complaints are deliberately excluded from these.
	SoftBounces *int64   `json:"soft_bounces"`
	HardBounces *int64   `json:"hard_bounces"`
	Bounces     *int64   `json:"bounces"`     // soft + hard
	BounceRate  *float64 `json:"bounce_rate"` // (soft+hard)/sent; null if sent=0

	// Complaints (spam reports) — reported as their own separate figure.
	Complaints    *int64   `json:"complaints"`
	ComplaintRate *float64 `json:"complaint_rate"` // complaints/sent; null if sent=0

	// Note explains a degraded result (bounces table absent).
	Note string `json:"note,omitempty"`
}

// CampaignBounceMetrics computes per-campaign bounce and complaint figures in a
// single scan of bounces. campaign_id is nullable in bounces; this per-campaign
// view filters to the specific campaign (untied bounces belong only in the
// global trend, not in a per-campaign rate).
//
// Gating: HasBounces false → counts/rates null + note. Zero bounces → clean
// zeros (the FILTER aggregates yield 0, never an error).
func (db *DB) CampaignBounceMetrics(ctx context.Context, id int, includeOptin bool) (*CampaignBounceMetrics, error) {
	softSel, hardSel, complaintSel, bouncesJoin := "NULL::bigint", "NULL::bigint", "NULL::bigint", ""
	if db.Caps.HasBounces {
		softSel = "COALESCE(b.soft, 0)"
		hardSel = "COALESCE(b.hard, 0)"
		complaintSel = "COALESCE(b.complaints, 0)"
		bouncesJoin = `LEFT JOIN (
			SELECT campaign_id,
			       COUNT(*) FILTER (WHERE type = 'soft')      AS soft,
			       COUNT(*) FILTER (WHERE type = 'hard')      AS hard,
			       COUNT(*) FILTER (WHERE type = 'complaint') AS complaints
			FROM bounces
			WHERE campaign_id = $1
			GROUP BY campaign_id
		) b ON b.campaign_id = c.id`
	}

	query := fmt.Sprintf(`
SELECT c.id, c.name, c.sent,
       %s AS soft,
       %s AS hard,
       %s AS complaints
FROM campaigns c
%s
WHERE c.id = $1 AND ($2 OR c.type <> 'optin')`,
		softSel, hardSel, complaintSel, bouncesJoin)

	var m CampaignBounceMetrics
	var soft, hard, complaints *int64
	err := db.Pool.QueryRow(ctx, query, id, includeOptin).
		Scan(&m.CampaignID, &m.Name, &m.Sent, &soft, &hard, &complaints)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrCampaignNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("bounce metrics query: %w", err)
	}

	m.HasBounces = db.Caps.HasBounces
	if !db.Caps.HasBounces {
		m.Note = "bounce tracking unavailable (bounces table absent)"
		return &m, nil
	}

	m.SoftBounces = soft
	m.HardBounces = hard
	m.Complaints = complaints

	// soft + hard, kept apart from complaints.
	if soft != nil && hard != nil {
		sh := *soft + *hard
		m.Bounces = &sh
		if m.Sent > 0 {
			br := float64(sh) / float64(m.Sent)
			m.BounceRate = &br
		}
	}
	// complaint rate as its own figure.
	if complaints != nil && m.Sent > 0 {
		cr := float64(*complaints) / float64(m.Sent)
		m.ComplaintRate = &cr
	}

	return &m, nil
}

// BounceTrendBucket is one daily bucket of the global bounce/complaint trend.
// Counts are kept separate by type so complaints are never merged with bounces.
type BounceTrendBucket struct {
	Day        time.Time `json:"day"` // UTC date (midnight) of the bucket
	Soft       int64     `json:"soft"`
	Hard       int64     `json:"hard"`
	Complaints int64     `json:"complaints"`
}

// BounceTrend holds the global, daily-bucketed bounce/complaint trend across all
// campaigns. Unlike the per-campaign view, this INCLUDES bounces whose
// campaign_id is null (not tied to a campaign), since they are still part of the
// account's overall deliverability picture.
type BounceTrend struct {
	HasBounces bool                `json:"has_bounces"`
	Buckets    []BounceTrendBucket `json:"buckets"`
	TotalSoft  int64               `json:"total_soft"`
	TotalHard  int64               `json:"total_hard"`
	TotalCompl int64               `json:"total_complaints"`
	Note       string              `json:"note,omitempty"`
}

// BounceTrend returns counts of soft, hard, and complaint events bucketed by day
// (created_at), globally across all campaigns. Null-campaign bounces are
// included here (they are excluded only from per-campaign rates).
//
// Gating: HasBounces false → empty + note. Zero rows → clean empty trend.
func (db *DB) BounceTrend(ctx context.Context) (*BounceTrend, error) {
	t := &BounceTrend{HasBounces: db.Caps.HasBounces, Buckets: []BounceTrendBucket{}}
	if !db.Caps.HasBounces {
		t.Note = "bounce tracking unavailable (bounces table absent)"
		return t, nil
	}

	rows, err := db.Pool.Query(ctx, `
SELECT date_trunc('day', created_at)::date AS day,
       COUNT(*) FILTER (WHERE type = 'soft')      AS soft,
       COUNT(*) FILTER (WHERE type = 'hard')      AS hard,
       COUNT(*) FILTER (WHERE type = 'complaint') AS complaints
FROM bounces
GROUP BY day
ORDER BY day`)
	if err != nil {
		return nil, fmt.Errorf("bounce trend query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var b BounceTrendBucket
		if err := rows.Scan(&b.Day, &b.Soft, &b.Hard, &b.Complaints); err != nil {
			return nil, fmt.Errorf("scan trend bucket: %w", err)
		}
		t.Buckets = append(t.Buckets, b)
		t.TotalSoft += b.Soft
		t.TotalHard += b.Hard
		t.TotalCompl += b.Complaints
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate trend buckets: %w", err)
	}

	return t, nil
}
