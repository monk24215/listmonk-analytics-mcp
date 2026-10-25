package db_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/monk24215/listmonk-analytics-mcp/internal/db"
)

// TestSubscriberEngagement scores subscribers against the live read-only DB,
// verifies ordering/aggregation invariants, and reports the top engaged
// subscribers with masked emails. The IndividualTracking hard gate's data path
// is exercised here (this DB has tracking on); the off path returns a note.
func TestSubscriberEngagement(t *testing.T) {
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

	const window = 90
	res, err := d.SubscriberEngagement(ctx, window, 10, 0)
	if err != nil {
		t.Fatalf("SubscriberEngagement: %v", err)
	}

	t.Logf("IndividualTracking=%t window_days=%d limit=%d", res.IndividualTracking, res.WindowDays, res.Limit)
	if !res.IndividualTracking {
		t.Logf("HARD GATE (tracking): %s", res.Note)
		return
	}

	if res.WindowDays != window {
		t.Errorf("window_days=%d want %d", res.WindowDays, window)
	}
	if len(res.Subscribers) > res.Limit {
		t.Errorf("returned %d > limit %d", len(res.Subscribers), res.Limit)
	}

	var prev = 1e18
	for _, e := range res.Subscribers {
		// Ordering: score descending.
		if e.Score > prev+1e-9 {
			t.Errorf("not ordered by score desc: %v after %v", e.Score, prev)
		}
		prev = e.Score
		// Frequency invariant.
		if e.Frequency != e.Opens+e.Clicks {
			t.Errorf("subscriber %d frequency=%d but opens+clicks=%d", e.SubscriberID, e.Frequency, e.Opens+e.Clicks)
		}
		// Recency must fall within the window.
		if e.DaysSinceLast < 0 || e.DaysSinceLast > float64(window)+1 {
			t.Errorf("subscriber %d days_since_last=%.2f outside window", e.SubscriberID, e.DaysSinceLast)
		}
	}

	t.Logf("=== TOP ENGAGED SUBSCRIBERS (window=%dd, emails masked) ===", window)
	for i, e := range res.Subscribers {
		t.Logf("#%d id=%d %s | opens=%d clicks=%d freq=%d | last_seen=%s (%.1fd ago) | score=%.2f",
			i+1, e.SubscriberID, maskEmail(e.Email),
			e.Opens, e.Clicks, e.Frequency,
			e.LastSeen.Format("2006-01-02"), e.DaysSinceLast, e.Score)
	}
	if len(res.Subscribers) == 0 {
		t.Logf("(no engagement events within the window)")
	}
}

// maskEmail keeps a couple of leading characters and the domain, hiding the rest
// so the report does not print raw PII.
func maskEmail(e string) string {
	at := strings.IndexByte(e, '@')
	if at <= 0 {
		return "***"
	}
	local, domain := e[:at], e[at:]
	keep := 2
	if len(local) < keep {
		keep = len(local)
	}
	return local[:keep] + "***" + domain
}
