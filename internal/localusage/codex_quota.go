package localusage

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"
)

func NewCodexQuotaService(s *Store, credentialsPath string) *AccountQuotaService {
	q := &AccountQuotaService{store: s, provider: "codex", credentialsPath: credentialsPath,
		client:   &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		endpoint: "https://chatgpt.com/backend-api/wham/usage", now: time.Now,
		state: AccountQuota{ID: "codex-local", Provider: "codex", Label: "codex-local", Plan: "Codex", Credential: ".codex/auth.json", Windows: []LimitWindow{}, Source: "Codex account usage"}}
	var cached string
	if s.db.QueryRow("SELECT payload FROM account_quotas WHERE provider='codex'").Scan(&cached) == nil {
		var saved AccountQuota
		if json.Unmarshal([]byte(cached), &saved) == nil {
			q.state = saved
			q.state.Refreshing = false
		}
	}
	return q
}

func (q *AccountQuotaService) refreshCodex(ctx context.Context) {
	var auth struct {
		Tokens struct {
			AccessToken string `json:"access_token"`
			AccountID   string `json:"account_id"`
		} `json:"tokens"`
	}
	b, err := os.ReadFile(q.credentialsPath)
	if err != nil || json.Unmarshal(b, &auth) != nil || auth.Tokens.AccessToken == "" {
		q.fail("Codex account login unavailable. Sign in to Codex on this computer.", 0)
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, q.endpoint, nil)
	if err != nil {
		q.fail("Could not create Codex quota request.", 0)
		return
	}
	req.Header.Set("Authorization", "Bearer "+auth.Tokens.AccessToken)
	if auth.Tokens.AccountID != "" {
		req.Header.Set("ChatGPT-Account-Id", auth.Tokens.AccountID)
	}
	resp, err := q.client.Do(req)
	if err != nil {
		q.fail("Could not reach Codex quota service. Last known values are shown.", 0)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		message := fmt.Sprintf("Codex quota service returned HTTP %d. Last known values are shown.", resp.StatusCode)
		var retry time.Duration
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			message = "Codex login expired or lacks quota access. Sign in to Codex, then refresh."
		}
		if resp.StatusCode == 429 {
			message = "Codex is limiting quota checks. Refresh will retry automatically."
			retry = 15 * time.Minute
			if seconds, errParse := strconv.Atoi(resp.Header.Get("Retry-After")); errParse == nil && seconds > 0 {
				retry = max(retry, time.Duration(seconds)*time.Second)
			} else if date, errDate := http.ParseTime(resp.Header.Get("Retry-After")); errDate == nil {
				retry = max(retry, date.Sub(q.now()))
			}
		}
		q.fail(message, retry)
		return
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		q.fail("Could not read Codex quota response.", 0)
		return
	}
	account, err := parseCodexAccountQuota(body)
	if err != nil {
		q.fail("Codex returned no recognized quota windows.", 0)
		return
	}
	account.UpdatedAt = q.now().UTC().Format(time.RFC3339)
	q.mu.Lock()
	q.state = account
	q.state.Refreshing = true
	q.mu.Unlock()
	payload, _ := json.Marshal(account)
	if _, err = q.store.db.ExecContext(ctx, `INSERT INTO account_quotas VALUES('codex',?) ON CONFLICT(provider) DO UPDATE SET payload=excluded.payload`, string(payload)); err != nil {
		q.fail("Quota loaded, but its local cache could not be saved.", 0)
	}
}

func parseCodexAccountQuota(raw []byte) (AccountQuota, error) {
	type window struct {
		Used    *float64 `json:"used_percent"`
		Seconds int64    `json:"limit_window_seconds"`
		Reset   int64    `json:"reset_at"`
	}
	var response struct {
		Email     string `json:"email"`
		Plan      string `json:"plan_type"`
		RateLimit struct {
			Primary   *window `json:"primary_window"`
			Secondary *window `json:"secondary_window"`
		} `json:"rate_limit"`
		Resets *struct {
			Available  *int `json:"available_count"`
			Applicable *int `json:"applicable_available_count"`
		} `json:"rate_limit_reset_credits"`
	}
	a := AccountQuota{ID: "codex-local", Provider: "codex", Label: "codex-local", Plan: "Codex", Credential: ".codex/auth.json", Windows: []LimitWindow{}, Source: "Codex account usage"}
	if err := json.Unmarshal(raw, &response); err != nil {
		return a, err
	}
	if response.Email != "" {
		a.Label = "codex-" + maskEmail(response.Email)
	}
	plans := map[string]string{"prolite": "Pro Lite", "pro": "Pro", "plus": "Plus", "free": "Free", "team": "Team", "business": "Business", "enterprise": "Enterprise"}
	if plan, ok := plans[response.Plan]; ok {
		a.Plan = plan
	} else if response.Plan != "" {
		a.Plan = response.Plan
	}
	for n, w := range []*window{response.RateLimit.Primary, response.RateLimit.Secondary} {
		if w == nil || w.Used == nil || *w.Used < 0 {
			continue
		}
		label := fmt.Sprintf("%d-hour limit", w.Seconds/3600)
		if w.Seconds >= 86400 {
			label = fmt.Sprintf("%d-day limit", w.Seconds/86400)
		}
		if w.Seconds == 604800 {
			label = "Weekly limit"
		}
		reset := ""
		if w.Reset > 0 {
			reset = time.Unix(w.Reset, 0).UTC().Format(time.RFC3339)
		}
		a.Windows = append(a.Windows, LimitWindow{ID: strconv.Itoa(n), Label: label, Used: *w.Used, ResetsAt: reset})
	}
	if response.Resets != nil {
		a.ManualResets = &ManualResetAllowance{Available: response.Resets.Available, Applicable: response.Resets.Applicable}
	}
	if len(a.Windows) == 0 {
		return a, fmt.Errorf("no quota windows")
	}
	return a, nil
}
