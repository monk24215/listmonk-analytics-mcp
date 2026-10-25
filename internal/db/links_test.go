package db_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/monk24215/listmonk-analytics-mcp/internal/db"
)

// TestCampaignLinkBreakdown checks the per-link click breakdown for campaign 17
// against an independent JOIN aggregation, verifies the rows are ordered by
// total clicks descending, and reports the URLs and counts.
func TestCampaignLinkBreakdown(t *testing.T) {
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
	m, err := d.CampaignLinkBreakdown(ctx, id, false)
	if err != nil {
		t.Fatalf("CampaignLinkBreakdown(%d): %v", id, err)
	}

	// Independent cross-check: per-URL totals from the same JOIN.
	direct := map[string][2]int64{} // url -> {total, unique}
	rows, err := d.Pool.Query(ctx, `
		SELECT l.url, COUNT(*), COUNT(DISTINCT lc.subscriber_id)
		FROM link_clicks lc
		JOIN links l ON l.id = lc.link_id
		WHERE lc.campaign_id = $1
		GROUP BY l.url`, id)
	if err != nil {
		t.Fatalf("direct link query: %v", err)
	}
	for rows.Next() {
		var u string
		var tot, uniq int64
		if err := rows.Scan(&u, &tot, &uniq); err != nil {
			t.Fatalf("scan direct: %v", err)
		}
		direct[u] = [2]int64{tot, uniq}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate direct: %v", err)
	}

	if len(m.Links) != len(direct) {
		t.Errorf("link count: metric=%d direct=%d", len(m.Links), len(direct))
	}

	var prev int64 = 1<<62 - 1
	for _, l := range m.Links {
		// Ordering: total clicks descending.
		if l.TotalClicks > prev {
			t.Errorf("rows not ordered by total_clicks desc: %d after %d", l.TotalClicks, prev)
		}
		prev = l.TotalClicks

		want, ok := direct[l.URL]
		if !ok {
			t.Errorf("url %q not in direct results", l.URL)
			continue
		}
		if l.TotalClicks != want[0] {
			t.Errorf("url %q total: metric=%d direct=%d", l.URL, l.TotalClicks, want[0])
		}
		if m.IndividualTracking {
			if deref(l.UniqueClicks) != want[1] {
				t.Errorf("url %q unique: metric=%d direct=%d", l.URL, deref(l.UniqueClicks), want[1])
			}
		} else if l.UniqueClicks != nil {
			t.Errorf("url %q unique populated but tracking off", l.URL)
		}
	}

	t.Logf("=== PER-LINK BREAKDOWN: campaign %d %q (tracking=%t) ===", m.CampaignID, m.Name, m.IndividualTracking)
	if len(m.Links) == 0 {
		t.Logf("(no clicks recorded)")
	}
	for i, l := range m.Links {
		t.Logf("#%d total=%d unique=%d  %s", i+1, l.TotalClicks, deref(l.UniqueClicks), l.URL)
	}
}
