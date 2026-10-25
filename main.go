package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"math"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

//go:embed web/index.html examples/dashboard.json
var assets embed.FS

const (
	defaultListen = "127.0.0.1:8088"
	defaultLimit  = 50
	maxLimit      = 500
	httpTimeout   = 5 * time.Second
	sortBlock     = "block"
	sortProfit    = "profit"
	sortSize      = "size"
	outcomeWin    = "WIN"
	outcomeLost   = "FLOODED_LOST"
)

type mockRecord struct {
	Block      map[string]any   `json:"block"`
	OurAttempt map[string]any   `json:"our_attempt"`
	Winners    []map[string]any `json:"winners"`
}

type mockData struct {
	Snapshot     json.RawMessage `json:"snapshot"`
	Blocks       []mockRecord    `json:"blocks"`
	FloodSession json.RawMessage `json:"flood_session"`
	byKey        map[string]mockRecord
}

func loadDemo() (*mockData, error) {
	raw, err := assets.ReadFile("examples/dashboard.json")
	if err != nil {
		return nil, err
	}
	var data mockData
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, err
	}
	data.byKey = make(map[string]mockRecord, len(data.Blocks))
	for _, record := range data.Blocks {
		feed, ok := record.Block["feed"].(string)
		if !ok || !validFeed(feed) || number(record.Block, "block") < 1 {
			return nil, fmt.Errorf("invalid mock block")
		}
		key := recordKey(feed, int64(number(record.Block, "block")))
		if _, exists := data.byKey[key]; exists {
			return nil, fmt.Errorf("duplicate mock block")
		}
		data.byKey[key] = record
	}
	sort.Slice(data.Blocks, func(i, j int) bool {
		a, b := data.Blocks[i].Block, data.Blocks[j].Block
		if number(a, "block") != number(b, "block") {
			return number(a, "block") > number(b, "block")
		}
		return a["feed"].(string) < b["feed"].(string)
	})
	return &data, nil
}

func main() {
	listen := flag.String("listen", defaultListen, "local HTTP listen address")
	flag.Parse()
	demo, err := loadDemo()
	if err != nil {
		log.Fatal(err)
	}
	server := &http.Server{Addr: *listen, Handler: newHandler(demo, time.Now), ReadHeaderTimeout: httpTimeout}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	go func() {
		<-ctx.Done()
		shutdown, done := context.WithTimeout(context.Background(), httpTimeout)
		defer done()
		_ = server.Shutdown(shutdown)
	}()
	log.Printf("Actual liq-dashboard UI with mock data: http://%s", *listen)
	if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func number(m map[string]any, key string) float64 { n, _ := m[key].(float64); return n }
func object(m map[string]any, key string) map[string]any {
	value, _ := m[key].(map[string]any)
	return value
}
func recordKey(feed string, block int64) string { return feed + ":" + strconv.FormatInt(block, 10) }
func validFeed(feed string) bool {
	return feed == "ETH" || feed == "BTC" || feed == "CBETH" || feed == "CBXRP"
}

func (d *mockData) snapshot(now int64) (map[string]any, error) {
	var snapshot map[string]any
	if err := json.Unmarshal(d.Snapshot, &snapshot); err != nil {
		return nil, err
	}
	snapshot["updated_unix"] = now
	for _, raw := range object(snapshot, "feeds") {
		feed := raw.(map[string]any)
		for _, section := range []string{"oracle_chain", "onchain", "onchain_gate", "conn", "econ"} {
			data := object(feed, section)
			for _, key := range []string{"updated_unix", "age_unix", "ts", "last_ok_unix", "anchor_unix"} {
				if value, ok := data[key].(float64); ok {
					data[key] = now + int64(value)
				}
			}
		}
		state := object(object(feed, "state"), "candidate_state")
		state["last_commit_unix_ms"] = now * 1000
		pred := object(feed, "prediction")
		for _, key := range []string{"blocks", "future_obs"} {
			for _, raw := range pred[key].([]any) {
				point := raw.(map[string]any)
				point["block_sec"] = now - now%2 + int64(number(point, "block_sec"))
			}
		}
	}
	return snapshot, nil
}

func newHandler(d *mockData, clock func() time.Time) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/meta", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"mock": true, "resmon_url": ""})
	})
	mux.HandleFunc("/api/snapshot", func(w http.ResponseWriter, r *http.Request) {
		value, err := d.snapshot(clock().Unix())
		if err != nil {
			http.Error(w, "invalid mock snapshot", http.StatusInternalServerError)
			return
		}
		writeJSON(w, value)
	})
	mux.HandleFunc("/api/liqblocks", func(w http.ResponseWriter, r *http.Request) {
		rows, err := d.list(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, rows)
	})
	detail := func(r *http.Request) (mockRecord, bool) {
		feed := r.URL.Query().Get("feed")
		block, err := strconv.ParseInt(r.URL.Query().Get("block"), 10, 64)
		if !validFeed(feed) || err != nil || block < 1 {
			return mockRecord{}, false
		}
		value, ok := d.byKey[recordKey(feed, block)]
		return value, ok
	}
	mux.HandleFunc("/api/liqblock", func(w http.ResponseWriter, r *http.Request) {
		value, found := detail(r)
		if !found {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, value)
	})
	mux.HandleFunc("/api/floodsession", func(w http.ResponseWriter, r *http.Request) {
		record, found := detail(r)
		if !found {
			http.NotFound(w, r)
			return
		}
		var value map[string]any
		if err := json.Unmarshal(d.FloodSession, &value); err != nil {
			http.Error(w, "invalid mock session", http.StatusInternalServerError)
			return
		}
		for _, raw := range value["rows"].([]any) {
			row := raw.(map[string]any)
			row["b"] = number(record.Block, "block") + number(row, "b")
		}
		floods := number(record.OurAttempt, "floods")
		if floods > 0 {
			block := number(record.Block, "block")
			value["landscape"] = false
			value["oracle_block"] = block
			value["start_block"], value["stop_block"] = block-4, block+4
			value["target_lo"], value["target_hi"] = block, block
			value["min_offset"], value["max_offset"] = 0, 0
			value["total_sent"] = floods
			for _, raw := range value["rows"].([]any) {
				row := raw.(map[string]any)
				if number(row, "b") == block {
					row["sent"], row["land_n"] = floods, floods
					row["land"] = []map[string]any{{"t": block, "n": floods, "o": 0}}
					row["ilo"], row["ihi"] = record.OurAttempt["idx_lo"], record.OurAttempt["idx_hi"]
					row["pn"], row["plo"], row["phi"] = record.OurAttempt["post_n"], record.OurAttempt["post_lo"], record.OurAttempt["post_hi"]
				}
			}
		}
		writeJSON(w, value)
	})
	mux.HandleFunc("/api/liqindex", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"eth": map[string]any{"frontier": 44000000, "backfill_done": true}, "btc": map[string]any{"frontier": 44000000, "backfill_done": true}})
	})
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	web, _ := fs.Sub(assets, "web")
	mux.Handle("/", http.FileServerFS(web))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		// The original UI uses inline scripts, event handlers and styles.
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; connect-src 'self'; frame-ancestors 'none'")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "mock-data demo is read-only", http.StatusMethodNotAllowed)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (d *mockData) list(r *http.Request) ([]map[string]any, error) {
	q := r.URL.Query()
	feed := q.Get("feed")
	if feed != "" && feed != "both" && !validFeed(feed) {
		return nil, fmt.Errorf("invalid feed")
	}
	order := q.Get("sort")
	if order == "" {
		order = sortBlock
	}
	if order != sortBlock && order != sortProfit && order != sortSize {
		return nil, fmt.Errorf("invalid sort")
	}
	outcome := q.Get("outcome")
	if outcome != "" && outcome != "all" && outcome != "win" && outcome != "lost" {
		return nil, fmt.Errorf("invalid outcome")
	}
	numbers := map[string]float64{}
	for _, key := range []string{"min_usd", "from", "to"} {
		if raw := q.Get(key); raw != "" {
			n, err := strconv.ParseFloat(raw, 64)
			if err != nil || n < 0 || math.IsNaN(n) || math.IsInf(n, 0) {
				return nil, fmt.Errorf("invalid %s", key)
			}
			numbers[key] = n
		}
	}
	limit := defaultLimit
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxLimit {
			return nil, fmt.Errorf("invalid limit")
		}
		limit = n
	}
	var curBlock int64
	var curFeed string
	if raw := q.Get("cursor"); raw != "" && order == sortBlock {
		fields := strings.Split(raw, ",")
		if len(fields) != 2 || !validFeed(fields[1]) {
			return nil, fmt.Errorf("invalid cursor")
		}
		var err error
		curBlock, err = strconv.ParseInt(fields[0], 10, 64)
		if err != nil || curBlock < 1 {
			return nil, fmt.Errorf("invalid cursor")
		}
		curFeed = fields[1]
	}
	rows := []map[string]any{}
	for _, item := range d.Blocks {
		row := item.Block
		if feed != "" && feed != "both" && row["feed"] != feed {
			continue
		}
		if outcome == "win" && row["outcome"] != outcomeWin || outcome == "lost" && row["outcome"] != outcomeLost {
			continue
		}
		if number(row, "total_liq_usd") < numbers["min_usd"] || number(row, "ts") < numbers["from"] || (numbers["to"] > 0 && number(row, "ts") > numbers["to"]) {
			continue
		}
		if curBlock > 0 && (int64(number(row, "block")) > curBlock || (int64(number(row, "block")) == curBlock && row["feed"].(string) <= curFeed)) {
			continue
		}
		rows = append(rows, row)
	}
	if order != sortBlock {
		key := "total_liq_usd"
		if order == sortProfit {
			key = "total_profit_usd"
		}
		sort.SliceStable(rows, func(i, j int) bool { return number(rows[i], key) > number(rows[j], key) })
	}
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, nil
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("encode response: %v", err)
	}
}
