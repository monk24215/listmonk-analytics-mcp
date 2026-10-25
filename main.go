// listmonk-analytics-mcp: read-only MCP server exposing the listmonk-analytics
// queries. Connects only through a SELECT-only Postgres role.
package main

import (
	"context"
	"crypto/subtle"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/monk24215/listmonk-analytics-mcp/internal/db"
)

const (
	serverName    = "listmonk-analytics-mcp"
	serverVersion = "0.1.0"
)

type config struct {
	databaseURL   string
	engagedWindow int
	allowPII      bool
	transport     string
	httpPort      string
	httpToken     string
}

func loadConfig() (config, error) {
	c := config{
		databaseURL:   strings.TrimSpace(os.Getenv("DATABASE_URL")),
		engagedWindow: 90,
		allowPII:      isTrue(os.Getenv("ANALYTICS_ALLOW_PII")),
		transport:     strings.ToLower(strings.TrimSpace(envOr("MCP_TRANSPORT", "stdio"))),
		httpPort:      envOr("MCP_HTTP_PORT", "3200"),
		httpToken:     strings.TrimSpace(os.Getenv("MCP_HTTP_TOKEN")),
	}
	if c.databaseURL == "" {
		return c, fmt.Errorf("DATABASE_URL is required (use the read-only role)")
	}
	if v := os.Getenv("ENGAGED_WINDOW_DAYS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return c, fmt.Errorf("ENGAGED_WINDOW_DAYS must be a positive integer")
		}
		c.engagedWindow = n
	}
	if c.transport != "stdio" && c.transport != "http" {
		return c, fmt.Errorf("MCP_TRANSPORT must be stdio or http")
	}
	if c.transport == "http" && len(c.httpToken) < 24 {
		return c, fmt.Errorf("MCP_HTTP_TOKEN must be at least 24 characters for http transport")
	}
	return c, nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func isTrue(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// Output wraps every result so the SDK can infer an output schema.
type Output struct {
	Result any `json:"result"`
}

func readOnly() *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true}
}

type campaignIDInput struct {
	ID           int  `json:"id" jsonschema:"Listmonk campaign id"`
	IncludeOptin bool `json:"include_optin,omitempty" jsonschema:"include double opt-in campaigns (default false)"`
}

type compareInput struct {
	IncludeOptin bool   `json:"include_optin,omitempty" jsonschema:"include opt-in campaigns (default false)"`
	Status       string `json:"status,omitempty" jsonschema:"filter by campaign status; empty = all"`
	SortBy       string `json:"sort_by,omitempty" jsonschema:"sort key (whitelisted server-side); empty = sent date"`
	Order        string `json:"order,omitempty" jsonschema:"asc or desc; empty = desc"`
}

type growthInput struct {
	Interval string `json:"interval,omitempty" jsonschema:"day or week; empty = day"`
}

type engagementInput struct {
	Limit  int `json:"limit,omitempty" jsonschema:"max rows (default 50)"`
	Offset int `json:"offset,omitempty" jsonschema:"rows to skip"`
}

type noInput struct{}

func register(s *mcp.Server, d *db.DB, c config) {
	ctx := context.Background()
	_ = ctx

	mcp.AddTool(s, &mcp.Tool{Name: "analytics_capabilities", Description: "Which listmonk tables are present and whether individual subscriber tracking is on. Call first.", Annotations: readOnly()},
		func(_ context.Context, _ *mcp.CallToolRequest, _ noInput) (*mcp.CallToolResult, Output, error) {
			return nil, Output{Result: d.Caps}, nil
		})

	mcp.AddTool(s, &mcp.Tool{Name: "campaigns_compare", Description: "Compare campaigns by open, click and bounce metrics.", Annotations: readOnly()},
		func(ctx context.Context, _ *mcp.CallToolRequest, in compareInput) (*mcp.CallToolResult, Output, error) {
			rows, err := d.CampaignComparison(ctx, db.CampaignComparisonOptions{IncludeOptin: in.IncludeOptin, Status: in.Status, SortBy: in.SortBy, Order: in.Order})
			return nil, Output{Result: rows}, err
		})

	mcp.AddTool(s, &mcp.Tool{Name: "campaign_opens", Description: "Open rate and open diagnostics for one campaign.", Annotations: readOnly()},
		func(ctx context.Context, _ *mcp.CallToolRequest, in campaignIDInput) (*mcp.CallToolResult, Output, error) {
			r, err := d.CampaignOpenMetrics(ctx, in.ID, in.IncludeOptin)
			return nil, Output{Result: r}, err
		})

	mcp.AddTool(s, &mcp.Tool{Name: "campaign_clicks", Description: "Click rate and click-to-open ratio for one campaign.", Annotations: readOnly()},
		func(ctx context.Context, _ *mcp.CallToolRequest, in campaignIDInput) (*mcp.CallToolResult, Output, error) {
			r, err := d.CampaignClickMetrics(ctx, in.ID, in.IncludeOptin)
			return nil, Output{Result: r}, err
		})

	mcp.AddTool(s, &mcp.Tool{Name: "campaign_links", Description: "Per-URL click breakdown for one campaign.", Annotations: readOnly()},
		func(ctx context.Context, _ *mcp.CallToolRequest, in campaignIDInput) (*mcp.CallToolResult, Output, error) {
			r, err := d.CampaignLinkBreakdown(ctx, in.ID, in.IncludeOptin)
			return nil, Output{Result: r}, err
		})

	mcp.AddTool(s, &mcp.Tool{Name: "campaign_curve", Description: "Views and clicks over time since send, for one campaign.", Annotations: readOnly()},
		func(ctx context.Context, _ *mcp.CallToolRequest, in campaignIDInput) (*mcp.CallToolResult, Output, error) {
			r, err := d.CampaignEngagementCurve(ctx, in.ID, in.IncludeOptin)
			return nil, Output{Result: r}, err
		})

	mcp.AddTool(s, &mcp.Tool{Name: "campaign_bounces", Description: "Soft, hard and complaint bounces with rates for one campaign.", Annotations: readOnly()},
		func(ctx context.Context, _ *mcp.CallToolRequest, in campaignIDInput) (*mcp.CallToolResult, Output, error) {
			r, err := d.CampaignBounceMetrics(ctx, in.ID, in.IncludeOptin)
			return nil, Output{Result: r}, err
		})

	mcp.AddTool(s, &mcp.Tool{Name: "bounce_trend", Description: "Daily soft, hard and complaint bounce counts.", Annotations: readOnly()},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ noInput) (*mcp.CallToolResult, Output, error) {
			r, err := d.BounceTrend(ctx)
			return nil, Output{Result: r}, err
		})

	mcp.AddTool(s, &mcp.Tool{Name: "subscriber_growth", Description: "New subscribers per day or week.", Annotations: readOnly()},
		func(ctx context.Context, _ *mcp.CallToolRequest, in growthInput) (*mcp.CallToolResult, Output, error) {
			r, err := d.SubscriberGrowth(ctx, in.Interval)
			return nil, Output{Result: r}, err
		})

	mcp.AddTool(s, &mcp.Tool{Name: "list_active_counts", Description: "Active subscriber counts per list, respecting opt-in rules.", Annotations: readOnly()},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ noInput) (*mcp.CallToolResult, Output, error) {
			r, err := d.ListActiveCounts(ctx)
			return nil, Output{Result: r}, err
		})

	if c.allowPII {
		mcp.AddTool(s, &mcp.Tool{Name: "subscriber_engagement", Description: "Per-subscriber engagement scores. Returns emails and names (PII). Enabled only when ANALYTICS_ALLOW_PII=true.", Annotations: readOnly()},
			func(ctx context.Context, _ *mcp.CallToolRequest, in engagementInput) (*mcp.CallToolResult, Output, error) {
				limit := in.Limit
				if limit <= 0 {
					limit = 50
				}
				r, err := d.SubscriberEngagement(ctx, c.engagedWindow, limit, in.Offset)
				return nil, Output{Result: r}, err
			})
	}
}

func main() {
	c, err := loadConfig()
	if err != nil {
		log.Fatalf("%s: %v", serverName, err)
	}
	ctx := context.Background()
	d, err := db.New(ctx, c.databaseURL)
	if err != nil {
		log.Fatalf("%s: %v", serverName, err)
	}
	defer d.Close()

	impl := &mcp.Implementation{Name: serverName, Version: serverVersion}
	build := func() *mcp.Server {
		s := mcp.NewServer(impl, nil)
		register(s, d, c)
		return s
	}

	if c.transport == "stdio" {
		log.Printf("%s v%s ready on stdio · PII=%v", serverName, serverVersion, c.allowPII)
		if err := build().Run(ctx, &mcp.StdioTransport{}); err != nil {
			log.Fatal(err)
		}
		return
	}

	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return build() }, nil)
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"ok":true,"name":%q,"version":%q}`, serverName, serverVersion)
	})
	mux.Handle("/mcp", authed(c.httpToken, h))
	mux.Handle("/mcp/", authed(c.httpToken, h))
	log.Printf("%s v%s listening on http :%s", serverName, serverVersion, c.httpPort)
	log.Fatal(http.ListenAndServe(":"+c.httpPort, mux))
}

// authed accepts "Authorization: Bearer <token>" or the token as the path segment.
func authed(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		path := strings.TrimPrefix(r.URL.Path, "/mcp/")
		ok := (bearer != "" && subtle.ConstantTimeCompare([]byte(bearer), []byte(token)) == 1) ||
			(path != "" && r.URL.Path != "/mcp" && subtle.ConstantTimeCompare([]byte(path), []byte(token)) == 1)
		if !ok {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		r.URL.Path = "/mcp"
		next.ServeHTTP(w, r)
	})
}
