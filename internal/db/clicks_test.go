package db_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/monk24215/listmonk-analytics-mcp/internal/db"
)

// TestCampaignClickMetrics checks click metrics for the three reconciliation
// campaigns against independent single-table counts, verifies the click-rate
// and CTOR math, and reports the numbers.
func TestCampaignClickMetrics(t *testing.T) {
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

	t.Log("=== CLICK METRICS: reconciliation campaigns (ids 17, 11, 14) ===")
	for _, id := range []int{17, 11, 14} {
		m, err := d.CampaignClickMetrics(ctx, id, false)
		if err != nil {
			t.Fatalf("CampaignClickMetrics(%d): %v", id, err)
		}

		// Independent cross-check of the same single scan.
		var total, unique int64
		if err := d.Pool.QueryRow(ctx,
			`SELECT COUNT(*), COUNT(DISTINCT subscriber_id) FROM link_clicks WHERE campaign_id=$1`,
			id).Scan(&total, &unique); err != nil {
			t.Fatalf("direct clicks query (%d): %v", id, err)
		}
		// Independent unique-opens count — the CTOR denominator.
		var uniqueOpens int64
		if err := d.Pool.QueryRow(ctx,
			`SELECT COUNT(DISTINCT subscriber_id) FROM campaign_views WHERE campaign_id=$1`,
			id).Scan(&uniqueOpens); err != nil {
			t.Fatalf("direct opens query (%d): %v", id, err)
		}

		if got := deref(m.TotalClicks); got != total {
			t.Errorf("campaign %d total_clicks: metric=%d direct=%d", id, got, total)
		}

		if m.IndividualTracking {
			if got := deref(m.UniqueClicks); got != unique {
				t.Errorf("campaign %d unique_clicks: metric=%d direct=%d", id, got, unique)
			}
			// Click rate = unique / sent.
			if m.Sent > 0 {
				want := float64(unique) / float64(m.Sent)
				if m.ClickRate == nil || abs(*m.ClickRate-want) > 1e-9 {
					t.Errorf("campaign %d click_rate: got %v want %v", id, m.ClickRate, want)
				}
			}
			// CTOR = unique clicks / unique opens, guarded on unique opens > 0.
			if uniqueOpens > 0 {
				want := float64(unique) / float64(uniqueOpens)
				if m.CTOR == nil || abs(*m.CTOR-want) > 1e-9 {
					t.Errorf("campaign %d ctor: got %v want %v", id, m.CTOR, want)
				}
			} else if m.CTOR != nil {
				t.Errorf("campaign %d ctor: got %v want nil (unique opens = 0)", id, m.CTOR)
			}
		} else {
			// Tracking off: unique/rate/CTOR must not be faked.
			if m.UniqueClicks != nil || m.ClickRate != nil || m.CTOR != nil {
				t.Errorf("campaign %d tracking off but unique/rate/ctor populated", id)
			}
		}

		t.Logf("id=%d %q sent=%d | HEADLINE unique=%d click_rate=%s | DIAG total=%d | CTOR=%s (unique_opens=%d) | tracking=%t",
			m.CampaignID, m.Name, m.Sent,
			deref(m.UniqueClicks), pct(m.ClickRate),
			deref(m.TotalClicks), pct(m.CTOR), uniqueOpens, m.IndividualTracking)
	}
}
