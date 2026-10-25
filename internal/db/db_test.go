package db_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/monk24215/listmonk-analytics-mcp/internal/db"
)

// TestNewAndProbe exercises the pool against a live read-only listmonk
// database. It is skipped when DATABASE_URL is unset so `go test ./...` stays
// green without a database.
func TestNewAndProbe(t *testing.T) {
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

	t.Logf("capabilities: %+v", d.Caps)

	// Sanity read: campaigns must be readable (New already verified it exists).
	var n int64
	if err := d.Pool.QueryRow(ctx, "SELECT count(*) FROM campaigns").Scan(&n); err != nil {
		t.Fatalf("count campaigns: %v", err)
	}
	t.Logf("campaigns count = %d", n)

	// The pool must reject any write — confirms read-only enforcement.
	if _, err := d.Pool.Exec(ctx, "CREATE TEMP TABLE _probe_test (x int)"); err == nil {
		t.Fatal("write was NOT rejected: read-only enforcement is broken")
	} else {
		t.Logf("write correctly rejected: %v", err)
	}
}
