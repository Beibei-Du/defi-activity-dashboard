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
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

//go:embed web/* examples/activity.json
var assets embed.FS

const (
	defaultListen   = "127.0.0.1:8088"
	defaultPageSize = 8
	maxPageSize     = 100
	pathActivity    = "/api/activity"
	pathBlock       = "/api/block"
	pathSummary     = "/api/summary"
	headerTimeout   = 5 * time.Second
	shutdownTimeout = 5 * time.Second
)

func main() {
	listen := flag.String("listen", defaultListen, "HTTP listen address")
	data := flag.String("data", "", "optional JSON dataset; defaults to embedded synthetic examples")
	flag.Parse()
	in, err := assets.Open("examples/activity.json")
	if err != nil {
		log.Fatal(err)
	}
	if *data != "" {
		_ = in.Close()
		in, err = os.Open(*data)
		if err != nil {
			log.Fatal(err)
		}
	}
	store, err := openStore(in)
	_ = in.Close()
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()
	web, err := fs.Sub(assets, "web")
	if err != nil {
		log.Fatal(err)
	}
	server := &http.Server{Addr: *listen, Handler: newHandler(store, web), ReadHeaderTimeout: headerTimeout}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	go func() {
		<-ctx.Done()
		shutdown, done := context.WithTimeout(context.Background(), shutdownTimeout)
		defer done()
		_ = server.Shutdown(shutdown)
	}()
	log.Printf("DeFi activity dashboard: http://%s (%s)", *listen, store.label)
	if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func newHandler(store *Store, web fs.FS) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+pathSummary, func(w http.ResponseWriter, r *http.Request) {
		value, err := store.Summary(r.Context())
		if err != nil {
			http.Error(w, "could not read summary", http.StatusInternalServerError)
			return
		}
		writeJSON(w, value)
	})
	mux.HandleFunc("GET "+pathActivity, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		p := ListParams{Feed: Market(q.Get("market")), Sort: SortOrder(q.Get("sort")), Limit: defaultPageSize}
		if p.Sort == "" {
			p.Sort = SortNewest
		}
		if !validMarket(p.Feed) || (p.Sort != SortNewest && p.Sort != SortLargest) {
			http.Error(w, "unknown market or sort", http.StatusBadRequest)
			return
		}
		if raw := q.Get("limit"); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 1 || n > maxPageSize {
				http.Error(w, "invalid limit", http.StatusBadRequest)
				return
			}
			p.Limit = n
		}
		if raw := q.Get("cursor"); raw != "" {
			var err error
			p.Cursor, err = decodeCursor(raw, p.Sort, p.Feed)
			if err != nil {
				http.Error(w, "invalid cursor for these filters", http.StatusBadRequest)
				return
			}
		}
		page, err := store.List(r.Context(), p)
		if err != nil {
			http.Error(w, "could not read activity", http.StatusInternalServerError)
			return
		}
		writeJSON(w, page)
	})
	mux.HandleFunc("GET "+pathBlock, func(w http.ResponseWriter, r *http.Request) {
		market := Market(r.URL.Query().Get("market"))
		block, err := strconv.ParseInt(r.URL.Query().Get("block"), 10, 64)
		if !validMarket(market) || market == "" || err != nil || block < 1 {
			http.Error(w, "valid market and block required", http.StatusBadRequest)
			return
		}
		value, found, err := store.Block(r.Context(), market, block)
		if err != nil {
			http.Error(w, "could not read block", http.StatusInternalServerError)
			return
		}
		if !found {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, value)
	})
	mux.Handle("GET /", http.FileServerFS(web))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; frame-ancestors 'none'")
		mux.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("encode response: %v", err)
	}
}

func validMarket(m Market) bool {
	return m == "" || m == MarketETH || m == MarketBTC || m == MarketCBETH
}

func invalidData(format string, args ...any) error { return fmt.Errorf("dataset: "+format, args...) }
