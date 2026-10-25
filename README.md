# DeFi activity dashboard

A read-only liquidation activity explorer built with Go, SQLite and a browser UI. Adapted from the history view of my personal Morpho Blue operations dashboard on Base.

![Dashboard with market filters, liquidation records and block details](docs/dashboard-desktop.png)

## What it does

- Browse liquidation records by market, newest block or largest debt repaid.
- Inspect positions and transaction ordering within a block.
- Page through records without skipping markets sharing the same block or tied amounts.
- Keep USDC amounts as integer base units in storage and decimal strings in JSON.

The bundled data is **synthetic** and labelled in the interface. It demonstrates the product, not actual liquidation performance or trading profit.

## Run

Requires Go 1.26 or later. No Node tooling, API keys, wallets or RPC endpoints are needed.

```bash
go run .
```

Open <http://127.0.0.1:8088>. Optional settings:

```bash
go run . -listen 127.0.0.1:8090
go run . -data examples/activity.json
```

The server imports the JSON dataset into an in-memory SQLite database at startup. The API and interface only read it. Restarting reconstructs the database from the dataset; it is not a live indexer.

## Architecture

```text
JSON dataset -> validated import -> SQLite -> Go HTTP API -> browser
```

| File | Responsibility |
|---|---|
| `store.go` | Dataset validation, schema, indexed queries, details and pagination |
| `main.go` | Startup, read-only API and embedded static assets |
| `web/` | Market filters, activity table and block-detail panel |
| `examples/activity.json` | 24 synthetic market/block records |
| `store_test.go` | Pagination completeness, ordering, query plans, amounts and API boundaries |

`GET /api/summary` describes the dataset. `GET /api/activity` accepts `market`, `sort`, `limit` and an opaque `cursor`. `GET /api/block` accepts `market` and `block`.

```bash
curl 'http://127.0.0.1:8088/api/activity?market=ETH&sort=largest&limit=2'
curl 'http://127.0.0.1:8088/api/block?market=ETH&block=44000000'
```

Supported markets are `ETH`, `BTC` and `CBETH`; omitted market means all markets. Sort is `newest` or `largest`. Page size is 1–100. Continue using the returned `next` cursor with the same market and sort.

## Design decisions

**Composite cursors.** A block can contain liquidations in more than one market. Newest-first pagination uses `(block DESC, market ASC)`. Largest-first pagination includes the repayment amount before those tie-breakers. A block-only cursor is insufficient when the primary sort is amount. Cursors are bound to their market filter and order.

**Ordered indexes.** SQLite indexes match each supported order. Tests inspect query plans to ensure these views do not require a temporary sort.

**Exact amounts.** Repaid USDC is stored as an integer with six decimal places. JSON strings preserve integer precision; the browser uses `BigInt` for display. The UI shows two decimal places, truncating sub-cent fractions. Debt repaid is not profit.

**A narrow publication boundary.** The production console also controls services and displays strategy state. This standalone adaptation contains only the activity explorer: no live connections, signing, service restarts, configuration changes or strategy code. It retains the indexed history-query design and adapts the pagination logic; the UI and dataset were prepared for this standalone version.

## Checks

```bash
go test ./...
go vet ./...
```

The browser flow was also checked at desktop and mobile sizes: market filtering, both orders, pagination, block selection and the empty state.

## Author and license

Built by [Adam Du](https://github.com/Beibei-Du). MIT licensed; see [LICENSE](LICENSE). SQLite is provided by the external `modernc.org/sqlite` Go module and its dependencies under their respective licenses.
