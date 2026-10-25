package db

import (
	"context"
	"fmt"
	"time"
)

// GrowthBucket is one time bucket of new-subscriber signups.
type GrowthBucket struct {
	Period         time.Time `json:"period"` // bucket start (date): the day, or the week's Monday
	NewSubscribers int64     `json:"new_subscribers"`
}

// SubscriberGrowth is the new-subscribers-over-time series. Aggregated entirely
// in SQL (GROUP BY date bucket) so a large subscriber base never streams into
// Go — only one row per non-empty bucket is returned.
type SubscriberGrowth struct {
	Interval string         `json:"interval"` // "day" | "week"
	Buckets  []GrowthBucket `json:"buckets"`
	Total    int64          `json:"total"`
}

// growthIntervals whitelists the interval query param to a date_trunc unit,
// preventing injection (date_trunc's unit cannot be parameterized).
var growthIntervals = map[string]string{
	"day":  "day",
	"week": "week",
}

// SubscriberGrowth returns new subscribers per time bucket from
// subscribers.created_at. interval is "day" (default) or "week".
//
// The COUNT/GROUP BY runs in Postgres; Go only accumulates the per-bucket rows.
func (db *DB) SubscriberGrowth(ctx context.Context, interval string) (*SubscriberGrowth, error) {
	if interval == "" {
		interval = "day"
	}
	unit, ok := growthIntervals[interval]
	if !ok {
		return nil, fmt.Errorf("%w: interval %q", ErrInvalidOption, interval)
	}

	query := fmt.Sprintf(`
SELECT date_trunc('%s', created_at)::date AS period,
       COUNT(*) AS new_subscribers
FROM subscribers
GROUP BY period
ORDER BY period`, unit)

	rows, err := db.Pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("subscriber growth query: %w", err)
	}
	defer rows.Close()

	g := &SubscriberGrowth{Interval: interval, Buckets: []GrowthBucket{}}
	for rows.Next() {
		var b GrowthBucket
		if err := rows.Scan(&b.Period, &b.NewSubscribers); err != nil {
			return nil, fmt.Errorf("scan growth bucket: %w", err)
		}
		g.Buckets = append(g.Buckets, b)
		g.Total += b.NewSubscribers
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate growth buckets: %w", err)
	}

	return g, nil
}

// ListActiveCount is one list's active-subscriber count alongside the opt-in
// type that determined the active rule. ActiveRule states, in plain words, which
// rule was applied so the figure is interpretable.
type ListActiveCount struct {
	ListID int    `json:"list_id"`
	Name   string `json:"name"`
	Type   string `json:"type"`   // public | private | temporary
	Optin  string `json:"optin"`  // single | double — selects the active rule
	Status string `json:"status"` // active | archived

	ActiveSubscribers  int64  `json:"active_subscribers"`
	TotalSubscriptions int64  `json:"total_subscriptions"` // all non-orphan rows on the list
	ActiveRule         string `json:"active_rule"`
}

// ListActiveCounts is the per-list active-subscriber breakdown.
type ListActiveCounts struct {
	HasSubscriberLists bool              `json:"has_subscriber_lists"`
	Lists              []ListActiveCount `json:"lists"`
	Note               string            `json:"note,omitempty"`
}

// ListActiveCounts returns active-subscriber counts per list, applying the
// correct active rule PER LIST based on that list's lists.optin:
//
//   - double opt-in → active = subscriber_lists.status = 'confirmed'
//   - single opt-in → active = status <> 'unsubscribed' (unconfirmed counts as
//     active, since single opt-in needs no confirmation)
//
// Blocklisted subscribers are excluded from active counts everywhere. Orphaned
// subscriber_lists rows (list_id NULL) never match a list and are naturally
// excluded. The opt-in branch lives inside the aggregate FILTER, so each list
// is evaluated by its own rule — no single rule is assumed across lists.
//
// Gating: HasSubscriberLists false → empty + note.
func (db *DB) ListActiveCounts(ctx context.Context) (*ListActiveCounts, error) {
	out := &ListActiveCounts{HasSubscriberLists: db.Caps.HasSubscriberLists, Lists: []ListActiveCount{}}
	if !db.Caps.HasSubscriberLists {
		out.Note = "per-list counts unavailable (subscriber_lists table absent)"
		return out, nil
	}

	rows, err := db.Pool.Query(ctx, `
SELECT l.id, l.name, l.type::text, l.optin::text, l.status::text,
       COUNT(*) FILTER (
           WHERE sl.subscriber_id IS NOT NULL
             AND s.status <> 'blocklisted'
             AND (
                   (l.optin = 'double' AND sl.status = 'confirmed')
                OR (l.optin = 'single' AND sl.status <> 'unsubscribed')
             )
       ) AS active_subscribers,
       COUNT(sl.subscriber_id) AS total_subscriptions
FROM lists l
LEFT JOIN subscriber_lists sl ON sl.list_id = l.id
LEFT JOIN subscribers s ON s.id = sl.subscriber_id
GROUP BY l.id, l.name, l.type, l.optin, l.status
ORDER BY l.id`)
	if err != nil {
		return nil, fmt.Errorf("list active counts query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var c ListActiveCount
		if err := rows.Scan(&c.ListID, &c.Name, &c.Type, &c.Optin, &c.Status,
			&c.ActiveSubscribers, &c.TotalSubscriptions); err != nil {
			return nil, fmt.Errorf("scan list row: %w", err)
		}
		c.ActiveRule = activeRule(c.Optin)
		out.Lists = append(out.Lists, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate list rows: %w", err)
	}

	return out, nil
}

// activeRule describes, for display, the active-status rule applied to a list of
// the given opt-in type. Mirrors the SQL FILTER branch above.
func activeRule(optin string) string {
	switch optin {
	case "double":
		return "status = 'confirmed' (double opt-in)"
	case "single":
		return "status <> 'unsubscribed' (single opt-in: unconfirmed counts as active)"
	default:
		return "unknown opt-in type"
	}
}
