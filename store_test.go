package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func exampleStore(t *testing.T) *Store {
	t.Helper()
	in, err := assets.Open("examples/activity.json")
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	s, err := openStore(in)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// Same-block rows and equal amounts must survive every page boundary for both sorts.
func TestPaginationCoversEveryRow(t *testing.T) {
	s := exampleStore(t)
	for _, order := range []SortOrder{SortNewest, SortLargest} {
		for _, market := range []Market{"", MarketETH} {
			t.Run(string(order)+"/"+string(market), func(t *testing.T) {
				p := ListParams{Feed: market, Sort: order, Limit: 2}
				var seen = map[string]bool{}
				var previous *Activity
				for {
					page, err := s.List(context.Background(), p)
					if err != nil {
						t.Fatal(err)
					}
					for _, row := range page.Rows {
						key := fmt.Sprintf("%s/%d", row.Market, row.Block)
						if seen[key] {
							t.Fatalf("duplicate row: %s", key)
						}
						seen[key] = true
						if market != "" && row.Market != market {
							t.Fatal("market filter ignored")
						}
						if previous != nil {
							if order == SortLargest && previous.RepaidUSDC < row.RepaidUSDC {
								t.Fatal("amount ordering regressed")
							}
							if order == SortNewest || previous.RepaidUSDC == row.RepaidUSDC {
								if previous.Block < row.Block || (previous.Block == row.Block && previous.Market > row.Market) {
									t.Fatal("tie-break ordering regressed")
								}
							}
						}
						copy := row
						previous = &copy
					}
					if page.Next == "" {
						break
					}
					p.Cursor, err = decodeCursor(page.Next, order, market)
					if err != nil {
						t.Fatal(err)
					}
					if len(seen) > 24 {
						t.Fatal("pagination did not terminate")
					}
				}
				want := 24
				if market != "" {
					want = 8
				}
				if len(seen) != want {
					t.Fatalf("got %d records, want %d", len(seen), want)
				}
			})
		}
	}
}

func TestQueryPlanUsesOrderedIndexes(t *testing.T) {
	s := exampleStore(t)
	for _, order := range []SortOrder{SortNewest, SortLargest} {
		for _, market := range []Market{"", MarketETH} {
			q, args := buildListQuery(ListParams{Feed: market, Sort: order, Limit: 8})
			rows, err := s.db.Query("EXPLAIN QUERY PLAN "+q, args...)
			if err != nil {
				t.Fatal(err)
			}
			var plan string
			for rows.Next() {
				var id, parent, unused int
				var detail string
				if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
					t.Fatal(err)
				}
				plan += detail
			}
			err = rows.Err()
			_ = rows.Close()
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(plan, "TEMP B-TREE") || !strings.Contains(plan, "INDEX") {
				t.Fatalf("unordered query: %s", plan)
			}
		}
	}
}

func TestBlockDetailsAndExactAmounts(t *testing.T) {
	s := exampleStore(t)
	row, found, err := s.Block(context.Background(), MarketETH, 44_000_000)
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	var sum int64
	for _, p := range row.Liquidations {
		sum += p.RepaidUSDC
	}
	if sum != row.RepaidUSDC || row.Positions != len(row.Liquidations) {
		t.Fatal("detail and block totals differ")
	}
	b, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"repaid_usdc":"125000000000"`) {
		t.Fatalf("raw USDC lost its string representation: %s", b)
	}
	_, found, err = s.Block(context.Background(), MarketETH, 1)
	if err != nil || found {
		t.Fatalf("missing block: found=%v err=%v", found, err)
	}
}

func TestDatasetRejectsInconsistentPositions(t *testing.T) {
	b, err := assets.ReadFile("examples/activity.json")
	if err != nil {
		t.Fatal(err)
	}
	var data dataset
	if err := json.Unmarshal(b, &data); err != nil {
		t.Fatal(err)
	}
	data.Blocks[0].Liquidations[0].RepaidUSDC++
	b, err = json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if s, err := openStore(strings.NewReader(string(b))); err == nil {
		_ = s.Close()
		t.Fatal("inconsistent totals accepted")
	}
}

func TestHTTPReadOnlyAndInvalidQueries(t *testing.T) {
	s := exampleStore(t)
	web, err := fs.Sub(assets, "web")
	if err != nil {
		t.Fatal(err)
	}
	handler := newHandler(s, web)
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{http.MethodGet, "/", http.StatusOK},
		{http.MethodGet, pathSummary, http.StatusOK},
		{http.MethodGet, pathActivity + "?sort=largest&market=BTC", http.StatusOK},
		{http.MethodGet, pathBlock + "?market=ETH&block=44000000", http.StatusOK},
		{http.MethodGet, pathBlock + "?market=ETH&block=1", http.StatusNotFound},
		{http.MethodGet, pathActivity + "?market=unknown", http.StatusBadRequest},
		{http.MethodGet, pathActivity + "?sort=sql", http.StatusBadRequest},
		{http.MethodGet, pathActivity + "?limit=101", http.StatusBadRequest},
		{http.MethodGet, pathActivity + "?cursor=bad", http.StatusBadRequest},
		{http.MethodPost, pathActivity, http.StatusMethodNotAllowed},
		{http.MethodPost, "/api/runtime", http.StatusMethodNotAllowed},
		{http.MethodPost, "/api/restart", http.StatusMethodNotAllowed},
	} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
		if w.Code != tc.status {
			t.Errorf("%s %s: got %d want %d", tc.method, tc.path, w.Code, tc.status)
		}
	}
	page, err := s.List(context.Background(), ListParams{Sort: SortLargest, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeCursor(page.Next, SortNewest, ""); err == nil {
		t.Fatal("cursor reused with another sort")
	}
	if _, err := decodeCursor(page.Next, SortLargest, MarketETH); err == nil {
		t.Fatal("cursor reused with another market filter")
	}
}
