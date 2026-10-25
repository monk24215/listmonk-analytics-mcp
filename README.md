# listmonk-analytics-mcp

Read-only MCP server exposing the listmonk-analytics queries (campaign opens, clicks, links, curve, bounces, growth, list counts) for a self-hosted Listmonk instance.

It connects only through a SELECT-only Postgres role. No tool in this server writes.

Status: tested against Listmonk v6.1.0 test environment (capabilities, campaign comparison and metrics, bounce trend, list counts, read-only role).

## Requirements

- Go 1.23+
- A Postgres read-only role on the Listmonk database (see the listmonk-analytics repo, setup/readonly-role.sql)

## Build

```bash
go build -o listmonk-analytics-mcp .
```

## Configuration

| Variable | Required | Default | Purpose |
|---|---|---|---|
| DATABASE_URL | yes | | Postgres URL of the read-only role |
| ENGAGED_WINDOW_DAYS | no | 90 | Window for engagement scoring |
| ANALYTICS_ALLOW_PII | no | false | true registers subscriber_engagement (returns emails and names) |
| MCP_TRANSPORT | no | stdio | stdio or http |
| MCP_HTTP_PORT | no | 3200 | HTTP port |
| MCP_HTTP_TOKEN | http only | | At least 24 characters |

## Tools

- analytics_capabilities
- campaigns_compare
- campaign_opens, campaign_clicks, campaign_links, campaign_curve, campaign_bounces
- bounce_trend
- subscriber_growth
- list_active_counts
- subscriber_engagement (only when ANALYTICS_ALLOW_PII=true)

All tools are read-only.

## Security

- The database connection is forced read-only at the session level as well as by the role.
- Subscriber-level data is off unless ANALYTICS_ALLOW_PII=true.
- Over HTTP, the bearer token or /mcp/<token> path is the secret. Treat it like a password.

## Source

The files in internal/db are copied unchanged from wirehouseworkers/listmonk-analytics. The only edit is the import path in the *_test.go files.

## License

MIT
