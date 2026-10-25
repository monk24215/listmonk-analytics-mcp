package db_test

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/monk24215/listmonk-analytics-mcp/internal/db"
)

// TestCampaignOpenMetrics checks open metrics for the three reconciliation
// campaigns against independent single-table counts, and reports the numbers.
func TestCampaignOpenMetrics(t *testing.T) {
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

	t.Log("=== OPEN METRICS: reconciliation campaigns (ids 17, 11, 14) ===")
	for _, id := range []int{17, 11, 14} {
		m, err := d.CampaignOpenMetrics(ctx, id, false)
		if err != nil {
			t.Fatalf("CampaignOpenMetrics(%d): %v", id, err)
		}

		// Independent cross-check of the same single scan.
		var total, unique int64
		if err := d.Pool.QueryRow(ctx,
			`SELECT COUNT(*), COUNT(DISTINCT subscriber_id) FROM campaign_views WHERE campaign_id=$1`,
			id).Scan(&total, &unique); err != nil {
			t.Fatalf("direct opens query (%d): %v", id, err)
		}
		if got := deref(m.TotalOpens); got != total {
			t.Errorf("campaign %d total_opens: metric=%d direct=%d", id, got, total)
		}
		if got := deref(m.UniqueOpens); got != unique {
			t.Errorf("campaign %d unique_opens: metric=%d direct=%d", id, got, unique)
		}

		// Verify rate and ratio math.
		if m.Sent > 0 && deref(m.UniqueOpens) >= 0 {
			want := float64(unique) / float64(m.Sent)
			if m.OpenRate == nil || abs(*m.OpenRate-want) > 1e-9 {
				t.Errorf("campaign %d open_rate: got %v want %v", id, m.OpenRate, want)
			}
		}
		if unique > 0 {
			want := float64(total) / float64(unique)
			if m.OpenRatio == nil || abs(*m.OpenRatio-want) > 1e-9 {
				t.Errorf("campaign %d open_ratio: got %v want %v", id, m.OpenRatio, want)
			}
		}

		t.Logf("id=%d %q sent=%d | HEADLINE unique=%d open_rate=%s | DIAG total=%d ratio=%s | tracking=%t",
			m.CampaignID, m.Name, m.Sent,
			deref(m.UniqueOpens), pct(m.OpenRate),
			deref(m.TotalOpens), ratioStr(m.OpenRatio), m.IndividualTracking)
	}
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

func ratioStr(p *float64) string {
	if p == nil {
		return "—"
	}
	return strconv.FormatFloat(*p, 'f', 3, 64)
}
