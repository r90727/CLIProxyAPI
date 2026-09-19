package localusage

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type LimitWindow struct {
	ID       string  `json:"id"`
	Label    string  `json:"label"`
	Used     float64 `json:"used_percent"`
	ResetsAt string  `json:"resets_at,omitempty"`
}

type AccountQuota struct {
	ID           string                `json:"id"`
	Provider     string                `json:"provider"`
	Label        string                `json:"label"`
	Plan         string                `json:"plan"`
	Credential   string                `json:"credential"`
	Windows      []LimitWindow         `json:"windows"`
	UpdatedAt    string                `json:"updated_at,omitempty"`
	Error        string                `json:"error,omitempty"`
	Refreshing   bool                  `json:"refreshing"`
	NextRefresh  string                `json:"next_refresh,omitempty"`
	Source       string                `json:"source"`
	ManualResets *ManualResetAllowance `json:"manual_resets,omitempty"`
}

type ManualResetAllowance struct {
	Available  *int `json:"available"`
	Applicable *int `json:"applicable"`
}

// AccountQuotaService reads the existing CLI login without refreshing or rewriting it.
// Only normalized quota windows and masked account metadata are stored or served.
type AccountQuotaService struct {
	store                        *Store
	credentialsPath, profilePath string
	client                       *http.Client
	endpoint                     string
	provider                     string
	now                          func() time.Time
	mu                           sync.Mutex
	state                        AccountQuota
	next                         time.Time
	manualNext                   time.Time
}

func NewClaudeQuotaService(s *Store, credentialsPath, profilePath string) *AccountQuotaService {
	q := &AccountQuotaService{store: s, provider: "claude", credentialsPath: credentialsPath, profilePath: profilePath,
		client:   &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		endpoint: "https://api.anthropic.com/api/oauth/usage", now: time.Now,
		state: AccountQuota{ID: "claude-local", Provider: "claude", Label: "claude-local", Plan: "Claude", Credential: ".claude/.credentials.json", Windows: []LimitWindow{}, Source: "Claude account usage"}}
	var cached string
	if s.db.QueryRow("SELECT payload FROM account_quotas WHERE provider='claude'").Scan(&cached) == nil {
		var saved AccountQuota
		if json.Unmarshal([]byte(cached), &saved) == nil {
			q.state = saved
			q.state.Refreshing = false
		}
	}
	return q
}

func (q *AccountQuotaService) Snapshot() AccountQuota {
	q.mu.Lock()
	defer q.mu.Unlock()
	s := q.state
	s.Windows = append([]LimitWindow{}, q.state.Windows...)
	if !q.next.IsZero() {
		s.NextRefresh = q.next.UTC().Format(time.RFC3339)
	}
	return s
}

func (q *AccountQuotaService) Run(ctx context.Context) {
	q.Refresh(ctx, false)
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			q.Refresh(ctx, false)
		}
	}
}

func (q *AccountQuotaService) Refresh(ctx context.Context, manual bool) {
	q.mu.Lock()
	now := q.now()
	// Manual refresh is allowed after 30 seconds, except during rate-limit backoff.
	if q.state.Refreshing || now.Before(q.next) && (!manual || now.Before(q.manualNext)) {
		q.mu.Unlock()
		return
	}
	q.state.Refreshing = true
	q.next = now.Add(5 * time.Minute)
	q.manualNext = now.Add(30 * time.Second)
	q.mu.Unlock()
	defer func() { q.mu.Lock(); q.state.Refreshing = false; q.mu.Unlock() }()
	if q.provider == "codex" {
		q.refreshCodex(ctx)
		return
	}
	var credentials struct {
		OAuth struct {
			AccessToken  string `json:"accessToken"`
			Subscription string `json:"subscriptionType"`
			Tier         string `json:"rateLimitTier"`
		} `json:"claudeAiOauth"`
	}
	raw, err := os.ReadFile(q.credentialsPath)
	if err != nil || json.Unmarshal(raw, &credentials) != nil || credentials.OAuth.AccessToken == "" {
		q.fail("Claude login unavailable. Run claude auth login on this computer.", 0)
		return
	}
	label := "claude-local"
	var profile struct {
		Account struct {
			Email string `json:"emailAddress"`
		} `json:"oauthAccount"`
	}
	if b, errRead := os.ReadFile(q.profilePath); errRead == nil && json.Unmarshal(b, &profile) == nil && profile.Account.Email != "" {
		label = "claude-" + maskEmail(profile.Account.Email)
	}
	plan := claudePlan(credentials.OAuth.Subscription, credentials.OAuth.Tier)
	q.mu.Lock()
	if q.state.Label != label {
		q.state.Windows = []LimitWindow{}
		q.state.UpdatedAt = ""
	}
	q.state.Label = label
	q.state.Plan = plan
	q.mu.Unlock()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, q.endpoint, nil)
	if err != nil {
		q.fail("Could not create Claude quota request.", 0)
		return
	}
	req.Header.Set("Authorization", "Bearer "+credentials.OAuth.AccessToken)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("User-Agent", "claude-code/2.1.80")
	resp, err := q.client.Do(req)
	if err != nil {
		q.fail("Could not reach Claude quota service. Last known values are shown.", 0)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		message := fmt.Sprintf("Claude quota service returned HTTP %d. Last known values are shown.", resp.StatusCode)
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			message = "Claude login expired or lacks usage access. Run claude auth login, then refresh."
		}
		var retry time.Duration
		if resp.StatusCode == 429 {
			message = "Claude is limiting quota checks. Refresh will retry automatically."
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
		q.fail("Could not read Claude quota response.", 0)
		return
	}
	windows, err := parseClaudeLimits(body)
	if err != nil {
		q.fail("Claude returned no recognized quota windows.", 0)
		return
	}
	q.mu.Lock()
	q.state.Windows = windows
	q.state.Error = ""
	q.state.UpdatedAt = q.now().UTC().Format(time.RFC3339)
	saved := q.state
	saved.Refreshing = false
	q.mu.Unlock()
	payload, _ := json.Marshal(saved)
	if _, err = q.store.db.ExecContext(ctx, `INSERT INTO account_quotas VALUES('claude',?) ON CONFLICT(provider) DO UPDATE SET payload=excluded.payload`, string(payload)); err != nil {
		q.fail("Quota loaded, but its local cache could not be saved.", 0)
	}
}

func (q *AccountQuotaService) fail(message string, retry time.Duration) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.state.Error = message
	if retry > 0 {
		q.next = q.now().Add(retry)
		q.manualNext = q.next
	}
}

func maskEmail(email string) string {
	parts := strings.SplitN(email, "@", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "local"
	}
	local, domain := []rune(parts[0]), []rune(parts[1])
	suffix := ""
	if dot := strings.LastIndex(parts[1], "."); dot >= 0 {
		suffix = parts[1][dot:]
	}
	return string(local[0]) + "•••@" + string(domain[0]) + "•••" + suffix
}

func claudePlan(subscription, tier string) string {
	if subscription == "max" {
		if strings.Contains(tier, "20x") {
			return "Max 20×"
		}
		if strings.Contains(tier, "5x") {
			return "Max 5×"
		}
		return "Max"
	}
	if subscription == "pro" {
		return "Pro"
	}
	if subscription == "" {
		return "Claude"
	}
	return strings.ToUpper(subscription[:1]) + subscription[1:]
}

func parseClaudeLimits(raw []byte) ([]LimitWindow, error) {
	var data struct {
		Limits []struct {
			Kind    string   `json:"kind"`
			Percent *float64 `json:"percent"`
			Resets  string   `json:"resets_at"`
			Scope   *struct {
				Model *struct {
					Display string `json:"display_name"`
				} `json:"model"`
			} `json:"scope"`
		} `json:"limits"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, err
	}
	windows := []LimitWindow{}
	for _, l := range data.Limits {
		if l.Percent == nil || *l.Percent < 0 {
			continue
		}
		label := ""
		switch l.Kind {
		case "session":
			label = "5-hour limit"
		case "weekly_all":
			label = "7-day limit"
		case "weekly_scoped":
			if l.Scope != nil && l.Scope.Model != nil && l.Scope.Model.Display != "" {
				label = "7-day " + l.Scope.Model.Display
			}
		}
		if label != "" {
			windows = append(windows, LimitWindow{ID: l.Kind + ":" + label, Label: label, Used: *l.Percent, ResetsAt: l.Resets})
		}
	}
	if len(windows) == 0 {
		var legacy map[string]*struct {
			Utilization *float64 `json:"utilization"`
			Resets      string   `json:"resets_at"`
		}
		// Decode recognized keys individually; unrelated response fields have other shapes.
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			return nil, err
		}
		legacy = make(map[string]*struct {
			Utilization *float64 `json:"utilization"`
			Resets      string   `json:"resets_at"`
		})
		for _, name := range []string{"five_hour", "seven_day", "seven_day_fable", "seven_day_opus", "seven_day_sonnet"} {
			if b, ok := fields[name]; ok {
				var v *struct {
					Utilization *float64 `json:"utilization"`
					Resets      string   `json:"resets_at"`
				}
				if json.Unmarshal(b, &v) == nil {
					legacy[name] = v
				}
			}
		}
		labels := map[string]string{"five_hour": "5-hour limit", "seven_day": "7-day limit", "seven_day_fable": "7-day Fable", "seven_day_opus": "7-day Opus", "seven_day_sonnet": "7-day Sonnet"}
		for name, v := range legacy {
			if v != nil && v.Utilization != nil && *v.Utilization >= 0 {
				windows = append(windows, LimitWindow{ID: name, Label: labels[name], Used: *v.Utilization, ResetsAt: v.Resets})
			}
		}
	}
	if len(windows) == 0 {
		return nil, fmt.Errorf("no quota windows")
	}
	rank := func(w LimitWindow) int {
		if w.Label == "5-hour limit" {
			return 1
		}
		if w.Label == "7-day limit" {
			return 2
		}
		return 0
	}
	sort.Slice(windows, func(a, b int) bool {
		if rank(windows[a]) == rank(windows[b]) {
			return windows[a].Label < windows[b].Label
		}
		return rank(windows[a]) < rank(windows[b])
	})
	return windows, nil
}

func (s *Store) codexQuota(ctx context.Context) AccountQuota {
	a := AccountQuota{ID: "codex-local", Provider: "codex", Label: "codex-local", Plan: "Codex", Credential: ".codex/sessions", Source: "Latest Codex session snapshot", Windows: []LimitWindow{}}
	var payload string
	if s.db.QueryRowContext(ctx, "SELECT at,payload FROM quotas ORDER BY at DESC LIMIT 1").Scan(&a.UpdatedAt, &payload) != nil {
		a.Error = "No quota reported yet. Start a Codex turn in T3 Code."
		return a
	}
	var quota Quota
	if json.Unmarshal([]byte(payload), &quota) != nil {
		a.Error = "Could not read the Codex quota snapshot."
		return a
	}
	if quota.Plan != "" {
		a.Plan = strings.ToUpper(quota.Plan[:1]) + quota.Plan[1:]
	}
	for n, w := range []*QuotaWindow{quota.Primary, quota.Secondary} {
		if w == nil {
			continue
		}
		label := fmt.Sprintf("%d-hour limit", w.Minutes/60)
		if w.Minutes >= 1440 {
			label = fmt.Sprintf("%d-day limit", w.Minutes/1440)
		}
		if w.Minutes == 10080 {
			label = "Weekly limit"
		}
		reset := ""
		if w.Resets > 0 {
			reset = time.Unix(w.Resets, 0).UTC().Format(time.RFC3339)
		}
		a.Windows = append(a.Windows, LimitWindow{ID: strconv.Itoa(n), Label: label, Used: w.Used, ResetsAt: reset})
	}
	return a
}
