package db_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/monk24215/listmonk-analytics-mcp/internal/db"
)

// TestCampaignBounceMetrics checks per-campaign bounce/complaint figures for the
// three reconciliation campaigns against independent FILTER counts, verifies the
// bounce rate excludes complaints, and reports the numbers. Zero bounces on a
// new account is a valid result, not an error.
func TestCampaignBounceMetrics(t *testing.T) {
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

	t.Logf("HasBounces=%t", d.Caps.HasBounces)
	t.Log("=== BOUNCE/COMPLAINT METRICS: campaigns (ids 17, 11, 14) ===")
	for _, id := range []int{17, 11, 14} {
		m, err := d.CampaignBounceMetrics(ctx, id, false)
		if err != nil {
			t.Fatalf("CampaignBounceMetrics(%d): %v", id, err)
		}

		if !d.Caps.HasBounces {
			t.Logf("id=%d bounces unavailable: %s", id, m.Note)
			continue
		}

		// Independent FILTER counts for the same campaign.
		var soft, hard, complaints int64
		if err := d.Pool.QueryRow(ctx, `
			SELECT COUNT(*) FILTER (WHERE type='soft'),
			       COUNT(*) FILTER (WHERE type='hard'),
			       COUNT(*) FILTER (WHERE type='complaint')
			FROM bounces WHERE campaign_id=$1`, id).Scan(&soft, &hard, &complaints); err != nil {
			t.Fatalf("direct bounce query (%d): %v", id, err)
		}

		if deref(m.SoftBounces) != soft {
			t.Errorf("campaign %d soft: metric=%d direct=%d", id, deref(m.SoftBounces), soft)
		}
		if deref(m.HardBounces) != hard {
			t.Errorf("campaign %d hard: metric=%d direct=%d", id, deref(m.HardBounces), hard)
		}
		if deref(m.Complaints) != complaints {
			t.Errorf("campaign %d complaints: metric=%d direct=%d", id, deref(m.Complaints), complaints)
		}
		// SEPARATION: bounces must be soft+hard only, never including complaints.
		if deref(m.Bounces) != soft+hard {
			t.Errorf("campaign %d bounces=%d but soft+hard=%d (complaints must NOT be included)",
				id, deref(m.Bounces), soft+hard)
		}
		// Rate math, zero-guarded.
		if m.Sent > 0 {
			wantBR := float64(soft+hard) / float64(m.Sent)
			if m.BounceRate == nil || abs(*m.BounceRate-wantBR) > 1e-9 {
				t.Errorf("campaign %d bounce_rate: got %v want %v", id, m.BounceRate, wantBR)
			}
			wantCR := float64(complaints) / float64(m.Sent)
			if m.ComplaintRate == nil || abs(*m.ComplaintRate-wantCR) > 1e-9 {
				t.Errorf("campaign %d complaint_rate: got %v want %v", id, m.ComplaintRate, wantCR)
			}
		}

		t.Logf("id=%d %q sent=%d | soft=%d hard=%d BOUNCES(soft+hard)=%d rate=%s | COMPLAINTS=%d rate=%s",
			m.CampaignID, m.Name, m.Sent,
			deref(m.SoftBounces), deref(m.HardBounces), deref(m.Bounces), pct(m.BounceRate),
			deref(m.Complaints), pct(m.ComplaintRate))
	}
}

// TestBounceTrend checks the global daily trend sums against independent totals
// over the whole bounces table (including campaign-untied rows).
func TestBounceTrend(t *testing.T) {
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

	tr, err := d.BounceTrend(ctx)
	if err != nil {
		t.Fatalf("BounceTrend: %v", err)
	}

	if !d.Caps.HasBounces {
		t.Logf("global trend unavailable: %s", tr.Note)
		return
	}

	// Independent grand totals across ALL rows (campaign-tied and untied).
	var soft, hard, complaints, total int64
	if err := d.Pool.QueryRow(ctx, `
		SELECT COUNT(*) FILTER (WHERE type='soft'),
		       COUNT(*) FILTER (WHERE type='hard'),
		       COUNT(*) FILTER (WHERE type='complaint'),
		       COUNT(*)
		FROM bounces`).Scan(&soft, &hard, &complaints, &total); err != nil {
		t.Fatalf("direct grand totals: %v", err)
	}

	if tr.TotalSoft != soft || tr.TotalHard != hard || tr.TotalCompl != complaints {
		t.Errorf("trend totals soft/hard/compl = %d/%d/%d, direct = %d/%d/%d",
			tr.TotalSoft, tr.TotalHard, tr.TotalCompl, soft, hard, complaints)
	}
	// soft+hard+complaints should account for the whole table (only these 3 types exist).
	if soft+hard+complaints != total {
		t.Logf("note: %d rows of other/unknown bounce_type beyond soft/hard/complaint", total-(soft+hard+complaints))
	}

	t.Logf("=== GLOBAL BOUNCE/COMPLAINT TREND ===")
	t.Logf("days=%d | TOTAL soft=%d hard=%d complaints=%d (kept separate)",
		len(tr.Buckets), tr.TotalSoft, tr.TotalHard, tr.TotalCompl)
	for _, b := range tr.Buckets {
		t.Logf("  %s soft=%d hard=%d complaints=%d", b.Day.Format("2006-01-02"), b.Soft, b.Hard, b.Complaints)
	}
	if len(tr.Buckets) == 0 {
		t.Logf("  (no bounces recorded — clean empty trend)")
	}
}
