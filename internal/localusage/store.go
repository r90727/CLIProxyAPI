// Package localusage provides durable, local-only token accounting for T3 Code.
package localusage

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct{ db *sql.DB }

type Event struct {
	ID         string `json:"id"`
	Source     string `json:"source"`
	Session    string `json:"session"`
	Provider   string `json:"provider"`
	Model      string `json:"model"`
	At         string `json:"at"`
	Input      int64  `json:"input"`
	Output     int64  `json:"output"`
	Cached     int64  `json:"cached"`
	CacheWrite int64  `json:"cache_write"`
	Reasoning  int64  `json:"reasoning"`
	Total      int64  `json:"total"`
	Failed     bool   `json:"failed"`
}

func fileDSN(path string, readonly bool) string {
	p, _ := filepath.Abs(path)
	u := url.URL{Scheme: "file", Path: "/" + filepath.ToSlash(p)}
	if filepath.IsAbs(p) && p[0] == '/' {
		u.Path = p
	}
	q := url.Values{"_pragma": {"busy_timeout(5000)"}}
	if readonly {
		q.Set("mode", "ro")
		q.Add("_pragma", "query_only(1)")
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", fileDSN(path, false))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL;
CREATE TABLE IF NOT EXISTS usage_events (
 id TEXT PRIMARY KEY, source TEXT NOT NULL, session TEXT NOT NULL,
 provider TEXT NOT NULL, model TEXT NOT NULL, at TEXT NOT NULL,
 input INTEGER NOT NULL CHECK(input>=0), output INTEGER NOT NULL CHECK(output>=0),
 cached INTEGER NOT NULL CHECK(cached>=0), cache_write INTEGER NOT NULL CHECK(cache_write>=0),
 reasoning INTEGER NOT NULL CHECK(reasoning>=0), total INTEGER NOT NULL CHECK(total>=0), failed INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS usage_time ON usage_events(at,source);
CREATE TABLE IF NOT EXISTS sessions (session TEXT PRIMARY KEY, thread TEXT NOT NULL, title TEXT NOT NULL, project TEXT NOT NULL, model TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS import_files (path TEXT PRIMARY KEY, offset INTEGER NOT NULL, state TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS quotas (session TEXT PRIMARY KEY, at TEXT NOT NULL, payload TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS account_quotas (provider TEXT PRIMARY KEY, payload TEXT NOT NULL);`)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("initialize usage database: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

type execer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func putEvent(ctx context.Context, db execer, e Event) error {
	if e.ID == "" || e.Source == "" {
		return fmt.Errorf("usage event needs an id and source")
	}
	t, err := time.Parse(time.RFC3339Nano, e.At)
	if err != nil {
		return fmt.Errorf("usage timestamp: %w", err)
	}
	e.At = t.UTC().Format("2006-01-02T15:04:05.000000000Z")
	_, err = db.ExecContext(ctx, `INSERT INTO usage_events VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET input=MAX(input,excluded.input),output=MAX(output,excluded.output),
cached=MAX(cached,excluded.cached),cache_write=MAX(cache_write,excluded.cache_write),
reasoning=MAX(reasoning,excluded.reasoning),total=MAX(total,excluded.total),failed=excluded.failed`,
		e.ID, e.Source, e.Session, e.Provider, e.Model, e.At, e.Input, e.Output, e.Cached, e.CacheWrite, e.Reasoning, e.Total, e.Failed)
	return err
}

func (s *Store) Put(ctx context.Context, e Event) error { return putEvent(ctx, s.db, e) }

type Totals struct {
	Cost       CostEstimate `json:"cost"`
	Name       string       `json:"name"`
	Events     int64        `json:"events"`
	Input      int64        `json:"input"`
	Output     int64        `json:"output"`
	Cached     int64        `json:"cached"`
	CacheWrite int64        `json:"cache_write"`
	Reasoning  int64        `json:"reasoning"`
	Total      int64        `json:"total"`
	Failed     int64        `json:"failed"`
}

type Filter struct{ Source, From, To string }

func (f Filter) where() (string, []any) {
	source := "e.source LIKE 't3-%' AND e.total > 0"
	if f.Source == "proxy" {
		source = "e.source='proxy'"
	}
	return source + " AND e.at>=? AND e.at<?", []any{f.From, f.To}
}

func (s *Store) Aggregate(ctx context.Context, f Filter, group string) ([]Totals, error) {
	groups := map[string]string{"total": "'Total'", "models": "e.model", "providers": "e.provider", "days": "substr(e.at,1,10)", "projects": "COALESCE(NULLIF(s.project,''),'Unlinked T3 sessions')", "threads": "COALESCE(NULLIF(s.title,''),e.session,'Unknown')"}
	expr, ok := groups[group]
	if !ok {
		return nil, fmt.Errorf("invalid group")
	}
	where, args := f.where()
	rows, err := s.db.QueryContext(ctx, `SELECT `+expr+`,e.model,COUNT(*),COALESCE(SUM(e.input),0),COALESCE(SUM(e.output),0),COALESCE(SUM(e.cached),0),COALESCE(SUM(e.cache_write),0),COALESCE(SUM(e.reasoning),0),COALESCE(SUM(e.total),0),COALESCE(SUM(e.failed),0) FROM usage_events e LEFT JOIN sessions s ON e.session=s.session WHERE `+where+` GROUP BY `+expr+`,e.model,(e.cached+e.cache_write>e.input) ORDER BY SUM(e.total) DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := []Totals{}
	indices := map[string]int{}
	for rows.Next() {
		var v Totals
		var model string
		if err = rows.Scan(&v.Name, &model, &v.Events, &v.Input, &v.Output, &v.Cached, &v.CacheWrite, &v.Reasoning, &v.Total, &v.Failed); err != nil {
			return nil, err
		}
		v.Cost = estimateCost(model, v)
		if index, exists := indices[v.Name]; exists {
			target := &result[index]
			target.Events += v.Events
			target.Input += v.Input
			target.Output += v.Output
			target.Cached += v.Cached
			target.CacheWrite += v.CacheWrite
			target.Reasoning += v.Reasoning
			target.Total += v.Total
			target.Failed += v.Failed
			target.Cost.add(v.Cost)
		} else {
			indices[v.Name] = len(result)
			result = append(result, v)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Total == result[j].Total {
			return result[i].Name < result[j].Name
		}
		return result[i].Total > result[j].Total
	})
	return result, rows.Err()
}
