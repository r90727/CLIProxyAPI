package localusage

import (
	"embed"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

//go:embed web/*
var assets embed.FS

func Handler(s *Store, importer *Importer, addr string, services ...*AccountQuotaService) http.Handler {
	mux := http.NewServeMux()
	var claude, codex *AccountQuotaService
	for _, service := range services {
		if service == nil {
			continue
		}
		if service.provider == "claude" {
			claude = service
		}
		if service.provider == "codex" {
			codex = service
		}
	}
	mux.HandleFunc("/api/quotas", func(w http.ResponseWriter, r *http.Request) {
		claudeAccount := AccountQuota{ID: "claude-local", Provider: "claude", Label: "claude-local", Plan: "Claude", Windows: []LimitWindow{}, Error: "Claude quota collection is not configured."}
		if claude != nil {
			claudeAccount = claude.Snapshot()
		}
		codexAccount := s.codexQuota(r.Context())
		if codex != nil {
			live := codex.Snapshot()
			if len(live.Windows) > 0 || len(codexAccount.Windows) == 0 {
				codexAccount = live
			} else {
				codexAccount.Error = live.Error
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"claude": []AccountQuota{claudeAccount}, "codex": []AccountQuota{codexAccount}, "status": importer.Status()})
	})
	mux.HandleFunc("/api/quotas/refresh", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Use POST", 405)
			return
		}
		service := claude
		switch r.URL.Query().Get("provider") {
		case "", "claude":
		case "codex":
			service = codex
		default:
			http.Error(w, "Unknown quota provider", 400)
			return
		}
		if service == nil {
			http.Error(w, "Account quota collection is not configured", 503)
			return
		}
		service.Refresh(r.Context(), true)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(service.Snapshot())
	})
	web, _ := fs.Sub(assets, "web")
	mux.Handle("/", http.FileServer(http.FS(web)))
	mux.HandleFunc("/api/summary", func(w http.ResponseWriter, r *http.Request) {
		f, err := parseFilter(r)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		result := map[string]any{"pricing": map[string]any{"checked": pricingChecked, "rates": apiRates, "currency": "USD"}, "status": importer.Status(), "source": f.Source, "from": f.From, "to": f.To}
		for _, group := range []string{"total", "models", "providers", "days", "projects", "threads"} {
			v, err := s.Aggregate(r.Context(), f, group)
			if err != nil {
				http.Error(w, "Could not read usage ledger", 500)
				return
			}
			result[group] = v
		}
		var q struct {
			At    string `json:"at"`
			Quota Quota  `json:"quota"`
		}
		var payload string
		if s.db.QueryRowContext(r.Context(), "SELECT at,payload FROM quotas ORDER BY at DESC LIMIT 1").Scan(&q.At, &payload) == nil {
			if json.Unmarshal([]byte(payload), &q.Quota) == nil {
				result["quota"] = q
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(result)
	})
	mux.HandleFunc("/api/export.csv", func(w http.ResponseWriter, r *http.Request) {
		f, err := parseFilter(r)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		where, args := f.where()
		rows, err := s.db.QueryContext(r.Context(), `SELECT e.at,e.source,e.provider,e.model,COALESCE(s.project,''),COALESCE(s.title,''),e.session,e.input,e.output,e.cached,e.cache_write,e.reasoning,e.total,e.failed FROM usage_events e LEFT JOIN sessions s ON e.session=s.session WHERE `+where+` ORDER BY e.at`, args...)
		if err != nil {
			http.Error(w, "Could not export usage ledger", 500)
			return
		}
		defer func() { _ = rows.Close() }()
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="t3-token-usage.csv"`)
		writer := csv.NewWriter(w)
		defer writer.Flush()
		_ = writer.Write([]string{"timestamp_utc", "source", "provider", "model", "project", "thread", "session", "input", "output", "cache_read", "cache_write", "reasoning", "total", "failed", "estimated_api_cost_usd", "pricing_status", "rates_checked"})
		for rows.Next() {
			values := make([]string, 14)
			dest := make([]any, 14)
			for n := range values {
				dest[n] = &values[n]
			}
			if rows.Scan(dest...) != nil {
				return
			}
			v := Totals{Events: 1}
			v.Input, _ = strconv.ParseInt(values[7], 10, 64)
			v.Output, _ = strconv.ParseInt(values[8], 10, 64)
			v.Cached, _ = strconv.ParseInt(values[9], 10, 64)
			v.CacheWrite, _ = strconv.ParseInt(values[10], 10, 64)
			cost := estimateCost(values[3], v)
			amount, status := "", "unpriced"
			if cost.PricedEvents > 0 {
				amount = strconv.FormatFloat(cost.USD, 'f', 8, 64)
				status = "estimated"
			}
			values = append(values, amount, status, pricingChecked)
			for n := range values {
				values[n] = csvSafe(values[n])
			}
			if writer.Write(values) != nil {
				return
			}
		}
	})
	_, port, _ := net.SplitHostPort(addr)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !(r.Method == http.MethodPost && r.URL.Path == "/api/quotas/refresh") {
			http.Error(w, "Read-only endpoint", 405)
			return
		}
		// Loopback binding alone does not protect against DNS rebinding.
		if r.Host != "127.0.0.1:"+port && r.Host != "localhost:"+port {
			http.Error(w, "Local access only", 403)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			u, err := url.Parse(origin)
			if err != nil || u.Host != r.Host || u.Scheme != "http" {
				http.Error(w, "Cross-origin access denied", 403)
				return
			}
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; frame-ancestors 'none'; base-uri 'none'")
		mux.ServeHTTP(w, r)
	})
}

func csvSafe(s string) string {
	trimmed := strings.TrimLeft(s, " \t\r\n")
	if (len(trimmed) > 0 && strings.ContainsRune("=+-@", rune(trimmed[0]))) || strings.HasPrefix(s, "\t") || strings.HasPrefix(s, "\r") {
		return "'" + s
	}
	return s
}

func parseFilter(r *http.Request) (Filter, error) {
	q := r.URL.Query()
	f := Filter{Source: q.Get("source"), From: "0000", To: "9999"}
	if f.Source == "" {
		f.Source = "t3"
	}
	if f.Source != "t3" && f.Source != "proxy" {
		return f, fmt.Errorf("source must be t3 or proxy")
	}
	for _, key := range []string{"from", "to"} {
		if v := q.Get(key); v != "" {
			t, err := time.Parse("2006-01-02", v)
			if err != nil {
				return f, fmt.Errorf("%s must be YYYY-MM-DD", key)
			}
			if key == "to" {
				f.To = t.AddDate(0, 0, 1).Format("2006-01-02T15:04:05.000000000Z")
			} else {
				f.From = t.Format("2006-01-02T15:04:05.000000000Z")
			}
		}
	}
	if f.From >= f.To {
		return f, fmt.Errorf("from must not follow to")
	}
	return f, nil
}

func LoopbackAddress(port int) (string, error) {
	if port < 1 || port > 65535 {
		return "", fmt.Errorf("invalid port")
	}
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), nil
}
