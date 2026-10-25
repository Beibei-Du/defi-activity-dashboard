package main

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	_ "modernc.org/sqlite"
)

type Market string

const (
	MarketETH   Market = "ETH"
	MarketBTC   Market = "BTC"
	MarketCBETH Market = "CBETH"
)

type SortOrder string

const (
	SortNewest  SortOrder = "newest"
	SortLargest SortOrder = "largest"
)

type Activity struct {
	Market           Market `json:"market"`
	Block            int64  `json:"block"`
	Timestamp        int64  `json:"timestamp"`
	Positions        int    `json:"positions"`
	RepaidUSDC       int64  `json:"repaid_usdc,string"`
	OracleIndex      int    `json:"oracle_index"`
	LiquidationIndex int    `json:"liquidation_index"`
	Liquidator       string `json:"liquidator"`
}

type Position struct {
	Borrower   string `json:"borrower"`
	RepaidUSDC int64  `json:"repaid_usdc,string"`
}

type BlockDetail struct {
	Activity
	Liquidations []Position `json:"liquidations"`
}

type dataset struct {
	Label     string        `json:"label"`
	Synthetic bool          `json:"synthetic"`
	Blocks    []BlockDetail `json:"blocks"`
}

type Store struct {
	db        *sql.DB
	label     string
	synthetic bool
}

const schema = `
CREATE TABLE liq_block (
 feed TEXT NOT NULL, block INTEGER NOT NULL, ts INTEGER NOT NULL,
 n_liqs INTEGER NOT NULL, repaid_usdc INTEGER NOT NULL,
 oracle_idx INTEGER NOT NULL, liq_idx INTEGER NOT NULL, liquidator TEXT NOT NULL,
 PRIMARY KEY(feed,block)
);
CREATE TABLE liq_position (
 feed TEXT NOT NULL, block INTEGER NOT NULL, ordinal INTEGER NOT NULL,
 borrower TEXT NOT NULL, repaid_usdc INTEGER NOT NULL,
 PRIMARY KEY(feed,block,ordinal)
);
CREATE INDEX ix_liq_block_bf ON liq_block(block DESC, feed ASC);
CREATE INDEX ix_liq_block_sbf ON liq_block(repaid_usdc DESC, block DESC, feed ASC);
CREATE INDEX ix_liq_block_fsb ON liq_block(feed, repaid_usdc DESC, block DESC);
`

const selectColumns = "feed,block,ts,n_liqs,repaid_usdc,oracle_idx,liq_idx,liquidator"

func openStore(input io.Reader) (*Store, error) {
	var data dataset
	dec := json.NewDecoder(input)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&data); err != nil {
		return nil, invalidData("%w", err)
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return nil, invalidData("expected one JSON document")
	}
	if data.Label == "" || len(data.Blocks) == 0 {
		return nil, invalidData("label and blocks required")
	}
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	fail := func(err error) (*Store, error) { _ = db.Close(); return nil, err }
	if _, err := db.Exec(schema); err != nil {
		return fail(err)
	}
	tx, err := db.Begin()
	if err != nil {
		return fail(err)
	}
	for _, row := range data.Blocks {
		if !validMarket(row.Market) || row.Market == "" || row.Block < 1 || row.Timestamp < 1 || row.RepaidUSDC < 0 || row.Positions != len(row.Liquidations) || row.Positions < 1 || row.OracleIndex < -1 || row.LiquidationIndex < 0 {
			_ = tx.Rollback()
			return fail(invalidData("invalid block %d", row.Block))
		}
		if _, err := tx.Exec("INSERT INTO liq_block VALUES(?,?,?,?,?,?,?,?)", row.Market, row.Block, row.Timestamp, row.Positions, row.RepaidUSDC, row.OracleIndex, row.LiquidationIndex, row.Liquidator); err != nil {
			_ = tx.Rollback()
			return fail(invalidData("%w", err))
		}
		var total int64
		for i, pos := range row.Liquidations {
			// Bound each addition by the declared nonnegative total, avoiding overflow.
			if pos.Borrower == "" || pos.RepaidUSDC < 0 || pos.RepaidUSDC > row.RepaidUSDC-total {
				_ = tx.Rollback()
				return fail(invalidData("invalid position in block %d", row.Block))
			}
			total += pos.RepaidUSDC
			if _, err := tx.Exec("INSERT INTO liq_position VALUES(?,?,?,?,?)", row.Market, row.Block, i, pos.Borrower, pos.RepaidUSDC); err != nil {
				_ = tx.Rollback()
				return fail(err)
			}
		}
		if total != row.RepaidUSDC {
			_ = tx.Rollback()
			return fail(invalidData("position totals differ in block %d", row.Block))
		}
	}
	if err := tx.Commit(); err != nil {
		return fail(err)
	}
	return &Store{db: db, label: data.Label, synthetic: data.Synthetic}, nil
}

func (s *Store) Close() error { return s.db.Close() }

type Cursor struct {
	Sort   SortOrder `json:"sort"`
	Filter Market    `json:"filter"`
	Block  int64     `json:"block"`
	Market Market    `json:"market"`
	Amount int64     `json:"amount"`
}

type ListParams struct {
	Feed   Market
	Sort   SortOrder
	Limit  int
	Cursor *Cursor
}

type Page struct {
	Rows []Activity `json:"rows"`
	Next string     `json:"next"`
}

func decodeCursor(raw string, order SortOrder, filter Market) (*Cursor, error) {
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, err
	}
	var c Cursor
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	if c.Sort != order || c.Filter != filter || !validMarket(c.Market) || c.Market == "" || c.Block < 1 || c.Amount < 0 || (filter != "" && c.Market != filter) {
		return nil, fmt.Errorf("cursor does not match query")
	}
	return &c, nil
}

// Adapted from the production dashboard's composite (block, feed) keyset.
// A value-sorted page must include that value in its cursor, ahead of tie-breakers.
func buildListQuery(p ListParams) (string, []any) {
	where := []string{"1=1"}
	args := []any{}
	if p.Feed != "" {
		where = append(where, "feed=?")
		args = append(args, p.Feed)
	}
	order := "block DESC, feed ASC"
	if p.Sort == SortLargest {
		order = "repaid_usdc DESC, block DESC, feed ASC"
	}
	if c := p.Cursor; c != nil {
		tail := "(block<? OR (block=? AND feed>?))"
		if p.Sort == SortLargest {
			where = append(where, "(repaid_usdc<? OR (repaid_usdc=? AND "+tail+"))")
			args = append(args, c.Amount, c.Amount, c.Block, c.Block, c.Market)
		} else {
			where = append(where, tail)
			args = append(args, c.Block, c.Block, c.Market)
		}
	}
	args = append(args, p.Limit+1)
	return "SELECT " + selectColumns + " FROM liq_block WHERE " + strings.Join(where, " AND ") + " ORDER BY " + order + " LIMIT ?", args
}

func scanActivity(scanner interface{ Scan(...any) error }) (Activity, error) {
	var row Activity
	err := scanner.Scan(&row.Market, &row.Block, &row.Timestamp, &row.Positions, &row.RepaidUSDC, &row.OracleIndex, &row.LiquidationIndex, &row.Liquidator)
	return row, err
}

func (s *Store) List(ctx context.Context, p ListParams) (Page, error) {
	q, args := buildListQuery(p)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return Page{}, err
	}
	defer rows.Close()
	page := Page{Rows: []Activity{}}
	for rows.Next() {
		row, err := scanActivity(rows)
		if err != nil {
			return Page{}, err
		}
		page.Rows = append(page.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return Page{}, err
	}
	if len(page.Rows) > p.Limit {
		page.Rows = page.Rows[:p.Limit]
		last := page.Rows[len(page.Rows)-1]
		b, err := json.Marshal(Cursor{p.Sort, p.Feed, last.Block, last.Market, last.RepaidUSDC})
		if err != nil {
			return Page{}, err
		}
		page.Next = base64.RawURLEncoding.EncodeToString(b)
	}
	return page, nil
}

func (s *Store) Block(ctx context.Context, market Market, block int64) (BlockDetail, bool, error) {
	row, err := scanActivity(s.db.QueryRowContext(ctx, "SELECT "+selectColumns+" FROM liq_block WHERE feed=? AND block=?", market, block))
	if err == sql.ErrNoRows {
		return BlockDetail{}, false, nil
	}
	if err != nil {
		return BlockDetail{}, false, err
	}
	rows, err := s.db.QueryContext(ctx, "SELECT borrower,repaid_usdc FROM liq_position WHERE feed=? AND block=? ORDER BY ordinal", market, block)
	if err != nil {
		return BlockDetail{}, false, err
	}
	defer rows.Close()
	out := BlockDetail{Activity: row, Liquidations: []Position{}}
	for rows.Next() {
		var pos Position
		if err := rows.Scan(&pos.Borrower, &pos.RepaidUSDC); err != nil {
			return BlockDetail{}, false, err
		}
		out.Liquidations = append(out.Liquidations, pos)
	}
	return out, true, rows.Err()
}

type Summary struct {
	Label     string `json:"label"`
	Synthetic bool   `json:"synthetic"`
	Blocks    int    `json:"blocks"`
	Positions int    `json:"positions"`
	First     int64  `json:"first"`
	Last      int64  `json:"last"`
}

func (s *Store) Summary(ctx context.Context) (Summary, error) {
	out := Summary{Label: s.label, Synthetic: s.synthetic}
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*),SUM(n_liqs),MIN(ts),MAX(ts) FROM liq_block").Scan(&out.Blocks, &out.Positions, &out.First, &out.Last)
	return out, err
}
