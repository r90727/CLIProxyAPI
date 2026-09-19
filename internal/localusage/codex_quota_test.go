package localusage

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

const codexQuotaFixture = `{"email":"example@example.com","plan_type":"prolite","rate_limit":{"primary_window":{"used_percent":4,"limit_window_seconds":604800,"reset_at":1790374715},"secondary_window":null},"rate_limit_reset_credits":{"available_count":0,"applicable_available_count":0}}`

func TestCodexQuotaResetTimeAndZeroAllowance(t *testing.T) {
	a, err := parseCodexAccountQuota([]byte(codexQuotaFixture))
	if err != nil {
		t.Fatal(err)
	}
	if a.ManualResets == nil || a.ManualResets.Available == nil || *a.ManualResets.Available != 0 || a.Plan != "Pro Lite" || strings.Contains(a.Label, "example") {
		t.Fatalf("account: %+v", a)
	}
	if len(a.Windows) != 1 || a.Windows[0].ResetsAt != "2026-09-25T22:18:35Z" || a.Windows[0].Used != 4 {
		t.Fatalf("window: %+v", a.Windows)
	}
	raw := `{"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_at":1790374715}}}`
	a, err = parseCodexAccountQuota([]byte(raw))
	if err != nil || a.ManualResets != nil || a.Windows[0].Label != "5-hour limit" {
		t.Fatalf("missing allowance should remain unknown: %+v %v", a, err)
	}
	if _, err = parseCodexAccountQuota([]byte(`{"rate_limit":{"primary_window":{"used_percent":null}}}`)); err == nil {
		t.Fatal("missing used percentage became zero")
	}
}

func TestCodexLiveQuotaRefreshAndCache(t *testing.T) {
	s, i, root := fixture(t)
	credentials := filepath.Join(root, "codex-auth.json")
	write(t, credentials, `{"tokens":{"access_token":"fake-access-token","account_id":"fake-account-id"}}`)
	q := NewCodexQuotaService(s, credentials)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fake-access-token" || r.Header.Get("ChatGPT-Account-Id") != "fake-account-id" {
			t.Error("missing account quota auth")
		}
		_, _ = w.Write([]byte(codexQuotaFixture))
	}))
	defer server.Close()
	q.endpoint = server.URL
	h := Handler(s, i, "127.0.0.1:8318", q)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "http://127.0.0.1:8318/api/quotas/refresh?provider=codex", nil))
	if w.Code != 200 || q.Snapshot().Error != "" || q.Snapshot().ManualResets == nil {
		t.Fatalf("live refresh: %s", w.Body.String())
	}
	var saved string
	if err := s.db.QueryRow("SELECT payload FROM account_quotas WHERE provider='codex'").Scan(&saved); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"fake-access-token", "fake-account-id", "example@example.com"} {
		if strings.Contains(saved, secret) {
			t.Fatal("private credential or raw account data persisted")
		}
	}
	loaded := NewCodexQuotaService(s, credentials)
	if loaded.Snapshot().ManualResets == nil || len(loaded.Snapshot().Windows) != 1 {
		t.Fatal("Codex quota did not survive restart")
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "http://127.0.0.1:8318/api/quotas", nil))
	if !strings.Contains(w.Body.String(), `"available":0`) {
		t.Fatal("zero reset allowance missing from dashboard API")
	}
	q.Refresh(context.Background(), true)
}
