package db_test

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/monk24215/listmonk-analytics-mcp/internal/db"
)

// TestCampaignComparison runs the comparison query against a live read-only
// listmonk database and verifies (1) no row multiplication — campaign IDs are
// unique — and (2) the joined aggregates match independent single-table counts,
// proving the multi-source join did not inflate or corrupt counts.
func TestCampaignComparison(t *testing.T) {
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

	rows, err := d.CampaignComparison(ctx, db.CampaignComparisonOptions{})
	if err != nil {
		t.Fatalf("CampaignComparison: %v", err)
	}
	t.Logf("returned %d campaign rows (regular only)", len(rows))

	// (1) No row multiplication: IDs must be unique.
	seen := map[int]bool{}
	for _, r := range rows {
		if seen[r.ID] {
			t.Fatalf("duplicate campaign id %d — join multiplied rows", r.ID)
		}
		seen[r.ID] = true

		// optin must be excluded by default.
		if r.Type == "optin" {
			t.Errorf("campaign %d is optin but was not excluded", r.ID)
		}
		// sent = 0 must yield null rates.
		if r.Sent == 0 && r.OpenRate != nil {
			t.Errorf("campaign %d has sent=0 but non-null open_rate", r.ID)
		}
	}

	// (2) Cross-check joined aggregates vs independent single-table queries for
	// up to 3 campaigns that have sends.
	checked := 0
	for _, r := range rows {
		if r.Sent == 0 || checked >= 3 {
			continue
		}
		checked++

		var uniqOpens, uniqClicks, soft, hard, complaints int64
		if err := d.Pool.QueryRow(ctx,
			`SELECT COUNT(DISTINCT subscriber_id) FROM campaign_views WHERE campaign_id=$1`,
			r.ID).Scan(&uniqOpens); err != nil {
			t.Fatalf("direct opens query: %v", err)
		}
		if err := d.Pool.QueryRow(ctx,
			`SELECT COUNT(DISTINCT subscriber_id) FROM link_clicks WHERE campaign_id=$1`,
			r.ID).Scan(&uniqClicks); err != nil {
			t.Fatalf("direct clicks query: %v", err)
		}
		if err := d.Pool.QueryRow(ctx,
			`SELECT
			   COUNT(*) FILTER (WHERE type='soft'),
			   COUNT(*) FILTER (WHERE type='hard'),
			   COUNT(*) FILTER (WHERE type='complaint')
			 FROM bounces WHERE campaign_id=$1`,
			r.ID).Scan(&soft, &hard, &complaints); err != nil {
			t.Fatalf("direct bounces query: %v", err)
		}

		if got := deref(r.UniqueOpens); got != uniqOpens {
			t.Errorf("campaign %d unique_opens: joined=%d direct=%d", r.ID, got, uniqOpens)
		}
		if got := deref(r.UniqueClicks); got != uniqClicks {
			t.Errorf("campaign %d unique_clicks: joined=%d direct=%d", r.ID, got, uniqClicks)
		}
		if got := deref(r.Bounces); got != soft+hard {
			t.Errorf("campaign %d bounces: joined=%d direct(soft+hard)=%d", r.ID, got, soft+hard)
		}
		if got := deref(r.Complaints); got != complaints {
			t.Errorf("campaign %d complaints: joined=%d direct=%d", r.ID, got, complaints)
		}
	}

	// Reconciliation dump: 3 finished campaigns, sorted by sent desc.
	fin, err := d.CampaignComparison(ctx, db.CampaignComparisonOptions{
		Status: "finished", SortBy: "sent", Order: "desc",
	})
	if err != nil {
		t.Fatalf("finished query: %v", err)
	}
	t.Log("=== RECONCILIATION: finished regular campaigns (top by sent) ===")
	n := 0
	for _, r := range fin {
		if n >= 3 {
			break
		}
		n++
		t.Logf("id=%d %q sent=%d | unique_opens(Reach)=%d unique_clicks=%d bounces=%d complaints=%d | open=%s click=%s bounce=%s complaint=%s | sent_date=%s",
			r.ID, r.Name, r.Sent,
			deref(r.UniqueOpens), deref(r.UniqueClicks), deref(r.Bounces), deref(r.Complaints),
			pct(r.OpenRate), pct(r.ClickRate), pct(r.BounceRate), pct(r.ComplaintRate),
			fmtTime(r.SentDate),
		)
	}
}

func deref(p *int64) int64 {
	if p == nil {
		return -1
	}
	return *p
}

func pct(p *float64) string {
	if p == nil {
		return "—"
	}
	return strconv.FormatFloat(*p*100, 'f', 2, 64) + "%"
}

func fmtTime(t *time.Time) string {
	if t == nil {
		return "(never)"
	}
	return t.Format("2006-01-02")
}
