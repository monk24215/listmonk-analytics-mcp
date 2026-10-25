package db_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/monk24215/listmonk-analytics-mcp/internal/db"
)

// TestCampaignEngagementCurve validates metric #4 by internal consistency:
// the sum of opens across all curve buckets must equal the campaign's total
// opens (COUNT(*) of campaign_views), and likewise for clicks. There is no
// listmonk UI equivalent to reconcile against.
func TestCampaignEngagementCurve(t *testing.T) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; skipping live read-only DB test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	d, err := db.New(ctx, url)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer d.Close()

	const id = 17
	m, err := d.CampaignEngagementCurve(ctx, id, false)
	if err != nil {
		t.Fatalf("CampaignEngagementCurve(%d): %v", id, err)
	}

	// Independent totals — the ground truth the curve must sum to.
	var totalOpens, totalClicks int64
	if err := d.Pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM campaign_views WHERE campaign_id=$1`, id).Scan(&totalOpens); err != nil {
		t.Fatalf("direct opens count: %v", err)
	}
	if err := d.Pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM link_clicks WHERE campaign_id=$1`, id).Scan(&totalClicks); err != nil {
		t.Fatalf("direct clicks count: %v", err)
	}

	// Sum the curve.
	var sumOpens, sumClicks int64
	var prev int = -1 << 30
	for _, b := range m.Buckets {
		sumOpens += b.Opens
		sumClicks += b.Clicks
		// Buckets must be ordered ascending and have the right width.
		if b.HoursSinceSend < prev {
			t.Errorf("buckets not ordered: %d after %d", b.HoursSinceSend, prev)
		}
		prev = b.HoursSinceSend
		wantWidth := 1
		if b.HoursSinceSend >= 48 {
			wantWidth = 24
		}
		if b.WidthHours != wantWidth {
			t.Errorf("bucket %dh width=%d want %d", b.HoursSinceSend, b.WidthHours, wantWidth)
		}
	}

	// CONSISTENCY CHECK — the heart of this metric's validation.
	if sumOpens != totalOpens {
		t.Errorf("CONSISTENCY FAIL opens: curve sum=%d total=%d", sumOpens, totalOpens)
	}
	if sumClicks != totalClicks {
		t.Errorf("CONSISTENCY FAIL clicks: curve sum=%d total=%d", sumClicks, totalClicks)
	}
	// The struct's own totals must also match its bucket sums.
	if m.TotalOpens != sumOpens || m.TotalClicks != sumClicks {
		t.Errorf("struct totals (%d/%d) != bucket sums (%d/%d)",
			m.TotalOpens, m.TotalClicks, sumOpens, sumClicks)
	}

	t.Logf("=== ENGAGEMENT CURVE: campaign %d %q ===", m.CampaignID, m.Name)
	if m.StartedAt != nil {
		t.Logf("started_at=%s  buckets=%d", m.StartedAt.Format(time.RFC3339), len(m.Buckets))
	}
	t.Logf("CONSISTENCY: opens curve-sum=%d total=%d match=%t | clicks curve-sum=%d total=%d match=%t",
		sumOpens, totalOpens, sumOpens == totalOpens,
		sumClicks, totalClicks, sumClicks == totalClicks)
	for _, b := range m.Buckets {
		unit := "h"
		if b.WidthHours == 24 {
			unit = "h (daily)"
		}
		t.Logf("  +%d%s opens=%d clicks=%d", b.HoursSinceSend, unit, b.Opens, b.Clicks)
	}
}
