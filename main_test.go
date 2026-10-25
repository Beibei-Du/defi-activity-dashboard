package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testDemo(t *testing.T) (*mockData, http.Handler) {
	t.Helper()
	d, err := loadDemo()
	if err != nil {
		t.Fatal(err)
	}
	return d, newHandler(d, func() time.Time { return time.Unix(1768000000, 0) })
}

func requestJSON(t *testing.T, h http.Handler, path string, target any) {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("%s: status=%d body=%s", path, w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), target); err != nil {
		t.Fatal(err)
	}
}

func TestMockEndpointsRejectEveryWrite(t *testing.T) {
	_, h := testDemo(t)
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		for _, path := range []string{"/api/runtime", "/api/restart", "/api/breaker", "/api/reset", "/api/snapshot"} {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(`{"feed":"ETH"}`)))
			if w.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s %s accepted: %d", method, path, w.Code)
			}
		}
	}
}

func TestSnapshotsDoNotAccumulateTimeOrMutateFixture(t *testing.T) {
	d, _ := testDemo(t)
	for _, now := range []int64{1768000000, 1768000100} {
		s, err := d.snapshot(now)
		if err != nil {
			t.Fatal(err)
		}
		if s["updated_unix"] != now {
			t.Fatal("wrong snapshot timestamp")
		}
		for _, raw := range object(s, "feeds") {
			feed := raw.(map[string]any)
			if object(feed, "oracle_chain")["updated_unix"] != now-45 {
				t.Fatal("oracle time accumulated")
			}
			if object(feed, "runtime")["flooding_dry"] != true || object(feed, "runtime")["candidate_reg_dry"] != true {
				t.Fatal("demo should display dry controls")
			}
			onchain := object(feed, "onchain")
			if len(onchain["cands"].([]any)) != int(number(onchain, "count")) {
				t.Fatal("on-chain count and rendered candidates differ")
			}
			blocks := object(feed, "prediction")["blocks"].([]any)
			if blocks[0].(map[string]any)["block_sec"] != now-now%2-4 {
				t.Fatal("prediction times were not rebased")
			}
		}
	}
}

func TestHistoryPaginationKeepsSameBlockFeedTwins(t *testing.T) {
	d, h := testDemo(t)
	seen := map[string]bool{}
	path := "/api/liqblocks?limit=1"
	for {
		var rows []map[string]any
		requestJSON(t, h, path, &rows)
		if len(rows) == 0 {
			break
		}
		r := rows[0]
		key := recordKey(r["feed"].(string), int64(number(r, "block")))
		if seen[key] {
			t.Fatal("duplicate row:", key)
		}
		seen[key] = true
		if len(seen) > len(d.Blocks) {
			t.Fatal("pagination did not terminate")
		}
		path = fmt.Sprintf("/api/liqblocks?limit=1&cursor=%d,%s", int64(number(r, "block")), r["feed"])
	}
	if len(seen) != len(d.Blocks) {
		t.Fatalf("got %d rows; want %d", len(seen), len(d.Blocks))
	}
}

func TestHistoryFiltersAndOriginalTopNSorts(t *testing.T) {
	_, h := testDemo(t)
	var rows []map[string]any
	requestJSON(t, h, "/api/liqblocks?feed=ETH&outcome=win&min_usd=50000&sort=profit", &rows)
	if len(rows) == 0 {
		t.Fatal("expected sample wins")
	}
	for i, r := range rows {
		if r["feed"] != "ETH" || r["outcome"] != outcomeWin || number(r, "total_liq_usd") < 50000 {
			t.Fatal("filter ignored")
		}
		if i > 0 && number(rows[i-1], "total_profit_usd") < number(r, "total_profit_usd") {
			t.Fatal("sort ignored")
		}
	}
	requestJSON(t, h, "/api/liqblocks?feed=CBXRP", &rows)
	if len(rows) != 0 {
		t.Fatal("expected empty mock feed")
	}
	requestJSON(t, h, "/api/liqblocks?from=1800000000", &rows)
	if len(rows) != 0 {
		t.Fatal("date range ignored")
	}
	for _, q := range []string{"sort=unknown", "limit=501", "cursor=bad", "feed=unknown", "min_usd=NaN"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/liqblocks?"+q, nil))
		if w.Code != http.StatusBadRequest {
			t.Errorf("accepted %s", q)
		}
	}
}

func TestOriginalDetailAndLandscapeShapes(t *testing.T) {
	_, h := testDemo(t)
	var record mockRecord
	requestJSON(t, h, "/api/liqblock?feed=ETH&block=44000000", &record)
	if record.Block["feed"] != "ETH" || len(record.Winners) == 0 {
		t.Fatal("detail missing")
	}
	var session map[string]any
	requestJSON(t, h, "/api/floodsession?feed=ETH&block=44000000", &session)
	rows := session["rows"].([]any)
	if rows[4].(map[string]any)["b"] != float64(44000000) {
		t.Fatal("landscape not anchored to selected block")
	}
	if session["landscape"] != false || session["total_sent"] != float64(180) {
		t.Fatal("flooded record did not render its mock session")
	}
	requestJSON(t, h, "/api/floodsession?feed=ETH&block=44000008", &session)
	if session["landscape"] != true {
		t.Fatal("non-flooded record should show landscape")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/liqblock?feed=ETH&block=1", nil))
	if w.Code != http.StatusNotFound {
		t.Fatal("missing record accepted")
	}
}
