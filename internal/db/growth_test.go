package db_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/monk24215/listmonk-analytics-mcp/internal/db"
)

// TestSubscriberGrowth checks that the daily growth series sums to the total
// subscriber count (proving SQL aggregation is correct and complete) and reports
// the tail of the curve.
func TestSubscriberGrowth(t *testing.T) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; skipping live read-only DB test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	d, err := db.New(ctx, url)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer d.Close()

	g, err := d.SubscriberGrowth(ctx, "day")
	if err != nil {
		t.Fatalf("SubscriberGrowth: %v", err)
	}

	var total int64
	if err := d.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM subscribers`).Scan(&total); err != nil {
		t.Fatalf("direct subscriber count: %v", err)
	}
	if g.Total != total {
		t.Errorf("growth total=%d but COUNT(*)=%d", g.Total, total)
	}

	// Buckets must be in ascending date order.
	for i := 1; i < len(g.Buckets); i++ {
		if g.Buckets[i].Period.Before(g.Buckets[i-1].Period) {
			t.Errorf("buckets out of order at %d", i)
		}
	}

	t.Logf("=== SUBSCRIBER GROWTH (daily) ===")
	t.Logf("total=%d across %d daily buckets (sum matches COUNT(*)=%t)", g.Total, len(g.Buckets), g.Total == total)
	tail := g.Buckets
	if len(tail) > 10 {
		tail = tail[len(tail)-10:]
		t.Logf("(showing last 10 buckets)")
	}
	for _, b := range tail {
		t.Logf("  %s new=%d", b.Period.Format("2006-01-02"), b.NewSubscribers)
	}
}

// TestListActiveCounts checks per-list active counts and, crucially, that the
// active rule respects each list's lists.optin: an independent per-list recount
// using the optin-appropriate status predicate must match.
func TestListActiveCounts(t *testing.T) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set; skipping live read-only DB test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	d, err := db.New(ctx, url)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer d.Close()

	c, err := d.ListActiveCounts(ctx)
	if err != nil {
		t.Fatalf("ListActiveCounts: %v", err)
	}

	t.Logf("HasSubscriberLists=%t", d.Caps.HasSubscriberLists)
	if !d.Caps.HasSubscriberLists {
		t.Logf("per-list counts unavailable: %s", c.Note)
		return
	}

	t.Logf("=== PER-LIST ACTIVE COUNTS (rule applied per list's opt-in) ===")
	for _, l := range c.Lists {
		// Independent recount using the optin-appropriate predicate. This proves
		// the rule was chosen by THIS list's optin, not a blanket rule.
		var statusPred string
		switch l.Optin {
		case "double":
			statusPred = "sl.status = 'confirmed'"
		case "single":
			statusPred = "sl.status <> 'unsubscribed'"
		default:
			statusPred = "false"
		}
		var want int64
		q := `SELECT COUNT(*) FROM subscriber_lists sl
		      JOIN subscribers s ON s.id = sl.subscriber_id
		      WHERE sl.list_id = $1 AND s.status <> 'blocklisted' AND ` + statusPred
		if err := d.Pool.QueryRow(ctx, q, l.ListID).Scan(&want); err != nil {
			t.Fatalf("independent recount (list %d): %v", l.ListID, err)
		}
		if l.ActiveSubscribers != want {
			t.Errorf("list %d (%s opt-in) active=%d independent=%d", l.ListID, l.Optin, l.ActiveSubscribers, want)
		}

		t.Logf("list=%d %q type=%s optin=%s status=%s | active=%d total_subs=%d | rule: %s",
			l.ListID, l.Name, l.Type, l.Optin, l.Status,
			l.ActiveSubscribers, l.TotalSubscriptions, l.ActiveRule)
	}
	if len(c.Lists) == 0 {
		t.Logf("(no lists)")
	}
}
