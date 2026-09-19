package localusage

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClaudeModernAndLegacyQuotaWindows(t *testing.T) {
	modern := `{"five_hour":{"utilization":99},"limits":[{"kind":"session","percent":43,"resets_at":"2026-09-19T03:30:00Z"},{"kind":"weekly_all","percent":4},{"kind":"weekly_scoped","percent":5,"scope":{"model":{"display_name":"Fable"}}},{"kind":"unknown","percent":70}]}`
	windows, err := parseClaudeLimits([]byte(modern))
	if err != nil {
		t.Fatal(err)
	}
	if len(windows) != 3 || windows[0].Label != "7-day Fable" || windows[0].Used != 5 || windows[1].Used != 43 || windows[2].Used != 4 {
		t.Fatalf("modern quotas: %+v", windows)
	}
	legacy := `{"five_hour":{"utilization":0,"resets_at":null},"seven_day":{"utilization":55},"seven_day_opus":null,"extra_usage":{"is_enabled":false},"seven_day_sonnet":{"utilization":null}}`
	windows, err = parseClaudeLimits([]byte(legacy))
	if err != nil {
		t.Fatal(err)
	}
	if len(windows) != 2 || windows[0].Label != "5-hour limit" || windows[0].Used != 0 || windows[1].Used != 55 {
		t.Fatalf("legacy quotas: %+v", windows)
	}
	for _, raw := range []string{`{}`, `{"five_hour":null}`, `{"limits":[{"kind":"session","percent":null}]}`, `invalid`} {
		if _, err = parseClaudeLimits([]byte(raw)); err == nil {
			t.Fatalf("unknown quota was accepted: %s", raw)
		}
	}
}

func quotaFixture(t *testing.T) (*AccountQuotaService, string) {
	t.Helper()
	s, _, root := fixture(t)
	credentials := filepath.Join(root, "credentials.json")
	profile := filepath.Join(root, "profile.json")
	write(t, credentials, `{"claudeAiOauth":{"accessToken":"test-only-secret","subscriptionType":"max","rateLimitTier":"default_claude_max_5x"}}`)
	write(t, profile, `{"oauthAccount":{"emailAddress":"example@example.com"}}`)
	return NewClaudeQuotaService(s, credentials, profile), credentials
}

func TestClaudeQuotaCachingBackoffAndCredentialPrivacy(t *testing.T) {
	q, credentials := quotaFixture(t)
	now := time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC)
	q.now = func() time.Time { return now }
	calls, status := 0, http.StatusOK
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer test-only-secret" || r.Header.Get("anthropic-beta") != "oauth-2025-04-20" {
			t.Error("missing quota auth headers")
		}
		if status != 200 {
			w.Header().Set("Retry-After", "1200")
			w.WriteHeader(status)
			_, _ = w.Write([]byte("upstream body must not escape: test-only-secret"))
			return
		}
		_, _ = w.Write([]byte(`{"five_hour":{"utilization":43},"seven_day":{"utilization":4}}`))
	}))
	defer server.Close()
	q.endpoint = server.URL
	q.Refresh(context.Background(), false)
	state := q.Snapshot()
	if state.Error != "" || state.Refreshing || len(state.Windows) != 2 || state.Plan != "Max 5×" || strings.Contains(state.Label, "example") {
		t.Fatalf("snapshot: %+v", state)
	}
	q.Refresh(context.Background(), true)
	q.Refresh(context.Background(), false)
	if calls != 1 {
		t.Fatal("quota endpoint polled too often")
	}
	var saved string
	if err := q.store.db.QueryRow("SELECT payload FROM account_quotas WHERE provider='claude'").Scan(&saved); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(saved, "test-only-secret") || strings.Contains(saved, "example@example.com") {
		t.Fatal("credentials or raw identity leaked to cache")
	}
	loaded := NewClaudeQuotaService(q.store, q.credentialsPath, q.profilePath)
	if len(loaded.Snapshot().Windows) != 2 {
		t.Fatal("last-known quota not restored")
	}
	now = now.Add(time.Minute)
	status = 429
	q.Refresh(context.Background(), true)
	state = q.Snapshot()
	if state.Error == "" || len(state.Windows) != 2 || state.Windows[0].Used != 43 || strings.Contains(state.Error, "test-only-secret") {
		t.Fatalf("rate-limit state: %+v", state)
	}
	now = now.Add(10 * time.Minute)
	q.Refresh(context.Background(), true)
	if calls != 2 {
		t.Fatal("manual refresh bypassed backoff")
	}
	now = now.Add(11 * time.Minute)
	status = 200
	q.Refresh(context.Background(), false)
	if calls != 3 || q.Snapshot().Error != "" {
		t.Fatal("did not recover after backoff")
	}
	before, err := os.ReadFile(credentials)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(before), "test-only-secret") {
		t.Fatal("CLI credential was modified")
	}
}

func TestClaudeQuotaDoesNotFollowRedirects(t *testing.T) {
	q, _ := quotaFixture(t)
	leaked := false
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked = true }))
	defer destination.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, destination.URL, 302) }))
	defer server.Close()
	q.endpoint = server.URL
	q.Refresh(context.Background(), false)
	if leaked || !strings.Contains(q.Snapshot().Error, "302") {
		t.Fatal("quota credential followed a redirect")
	}
}

func TestQuotaHTTPAndUnavailableClaude(t *testing.T) {
	q, _ := quotaFixture(t)
	q.credentialsPath = filepath.Join(t.TempDir(), "missing.json")
	q.Refresh(context.Background(), false)
	if q.Snapshot().Error == "" || len(q.Snapshot().Windows) != 0 {
		t.Fatal("missing credentials showed invented quota")
	}
	h := Handler(q.store, &Importer{}, "127.0.0.1:8318", q)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "http://127.0.0.1:8318/api/quotas", nil))
	var response struct {
		Claude []AccountQuota `json:"claude"`
		Codex  []AccountQuota `json:"codex"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &response) != nil || len(response.Claude) != 1 || len(response.Codex) != 1 {
		t.Fatalf("quota response: %s", w.Body.String())
	}
	r := httptest.NewRequest("POST", "http://127.0.0.1:8318/api/quotas/refresh", nil)
	r.Header.Set("Origin", "https://example.com")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("cross-origin quota refresh allowed")
	}
}
