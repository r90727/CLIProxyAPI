package localusage

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func fixture(t *testing.T) (*Store, *Importer, string) {
	t.Helper()
	root := t.TempDir()
	s, err := Open(filepath.Join(root, "usage.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	t3 := filepath.Join(root, "t3.sqlite")
	db, err := sql.Open("sqlite", fileDSN(t3, false))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE provider_session_runtime(provider_name TEXT,thread_id TEXT,resume_cursor_json TEXT,runtime_payload_json TEXT);
CREATE TABLE projection_threads(thread_id TEXT,title TEXT,project_id TEXT);
CREATE TABLE projection_projects(project_id TEXT,title TEXT);
INSERT INTO projection_projects VALUES('project','Sample project');
INSERT INTO projection_threads VALUES('thread','Sample thread','project');
INSERT INTO provider_session_runtime VALUES('codex','thread','{"threadId":"codex-session"}','{"model":"test-model"}');
INSERT INTO provider_session_runtime VALUES('claudeAgent','thread','{"resume":"claude-session"}','{"model":"claude-test"}');`)
	_ = db.Close()
	if err != nil {
		t.Fatal(err)
	}
	codex := filepath.Join(root, "codex")
	claude := filepath.Join(root, "claude")
	for _, p := range []string{codex, claude} {
		if err = os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	return s, &Importer{Store: s, T3DB: t3, CodexDir: codex, ClaudeDir: claude}, root
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}
func total(t *testing.T, s *Store, source string) Totals {
	t.Helper()
	a, err := s.Aggregate(context.Background(), Filter{source, "0000", "9999"}, "total")
	if err != nil {
		t.Fatal(err)
	}
	if len(a) == 0 {
		return Totals{}
	}
	return a[0]
}
func token(at string, input, output, cached, reasoning int) string {
	v := map[string]any{"type": "event_msg", "timestamp": at, "payload": map[string]any{"type": "token_count", "info": map[string]any{"total_token_usage": map[string]int{"input_tokens": input, "output_tokens": output, "cached_input_tokens": cached, "reasoning_output_tokens": reasoning, "total_tokens": input + output}}}}
	b, _ := json.Marshal(v)
	return string(b) + "\n"
}

func TestImporterRestartReplayAndPartialLine(t *testing.T) {
	s, i, _ := fixture(t)
	path := filepath.Join(i.CodexDir, "session.jsonl")
	meta := `{"type":"session_meta","payload":{"id":"codex-session","originator":"t3code_desktop","source":"cli"}}` + "\n"
	first := token("2026-09-18T00:00:00Z", 100, 20, 60, 5)
	second := token("2026-09-18T01:00:00Z", 160, 35, 90, 8)
	partial := strings.TrimSuffix(second, "\n")
	write(t, path, meta+first+first+partial)
	if err := i.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := total(t, s, "t3"); got.Total != 120 || got.Events != 1 || got.Cached != 60 || got.Reasoning != 5 {
		t.Fatalf("first scan: %+v", got)
	}
	// New importer simulates restart; it must use the committed byte offset and counters.
	i = &Importer{Store: s, T3DB: i.T3DB, CodexDir: i.CodexDir, ClaudeDir: i.ClaudeDir}
	write(t, path, meta+first+first+partial+"\n")
	if err := i.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := total(t, s, "t3"); got.Total != 195 || got.Events != 2 || got.Input != 160 || got.Output != 35 || got.Cached != 90 || got.Reasoning != 8 {
		t.Fatalf("restart: %+v", got)
	}
	write(t, filepath.Join(i.CodexDir, "copy.jsonl"), meta+first+first+second)
	if err := i.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := total(t, s, "t3"); got.Total != 195 || got.Events != 2 {
		t.Fatalf("replay double counted: %+v", got)
	}
	models, err := s.Aggregate(context.Background(), Filter{"t3", "0000", "9999"}, "models")
	if err != nil || models[0].Name != "test-model" {
		t.Fatalf("mapping: %+v %v", models, err)
	}
}

func TestClaudeDeduplicatesStreamingAndIncludesCache(t *testing.T) {
	s, i, _ := fixture(t)
	line := func(out int) string {
		b, _ := json.Marshal(map[string]any{"type": "assistant", "timestamp": "2026-09-18T00:00:00Z", "message": map[string]any{"id": "message-1", "model": "claude-test", "usage": map[string]int{"input_tokens": 10, "output_tokens": out, "cache_read_input_tokens": 50, "cache_creation_input_tokens": 30}}})
		return string(b) + "\n"
	}
	write(t, filepath.Join(i.ClaudeDir, "claude-session.jsonl"), line(1)+line(20)+line(20))
	// Other Claude CLI sessions must not enter the T3 total.
	write(t, filepath.Join(i.ClaudeDir, "unrelated.jsonl"), line(500))
	if err := i.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := total(t, s, "t3")
	if got.Total != 110 || got.Input != 90 || got.Output != 20 || got.Cached != 50 || got.CacheWrite != 30 || got.Events != 1 {
		t.Fatalf("Claude accounting: %+v", got)
	}
}

func TestCodexMissingCountersAndReset(t *testing.T) {
	s, i, _ := fixture(t)
	meta := `{"type":"session_meta","payload":{"id":"codex-session"}}` + "\n"
	missing := `{"type":"event_msg","timestamp":"2026-09-18T00:01:00Z","payload":{"type":"token_count","info":{"total_token_usage":null}}}` + "\n"
	content := meta + token("2026-09-18T00:00:00Z", 100, 20, 60, 5) + missing + token("2026-09-18T00:02:00Z", 100, 20, 60, 5) + token("2026-09-18T00:03:00Z", 10, 2, 5, 1)
	write(t, filepath.Join(i.CodexDir, "session.jsonl"), content)
	if err := i.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := total(t, s, "t3"); got.Total != 132 || got.Events != 2 {
		t.Fatalf("missing counters or reset double counted: %+v", got)
	}
}

func TestClaudeSubagentMapsToParentThread(t *testing.T) {
	s, i, _ := fixture(t)
	dir := filepath.Join(i.ClaudeDir, "claude-session", "subagents")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "agent-123.jsonl"), `{"type":"assistant","timestamp":"2026-09-18T00:00:00Z","message":{"id":"child-message","model":"child-model","usage":{"input_tokens":10,"output_tokens":5}}}`+"\n")
	if err := i.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	groups, err := s.Aggregate(context.Background(), Filter{"t3", "0000", "9999"}, "threads")
	if err != nil || len(groups) != 1 || groups[0].Name != "Sample thread" || groups[0].Total != 15 {
		t.Fatalf("subagent mapping: %+v %v", groups, err)
	}
}

func TestImporterRollsBackCheckpointOnMalformedCompleteLine(t *testing.T) {
	s, i, _ := fixture(t)
	path := filepath.Join(i.CodexDir, "session.jsonl")
	meta := `{"type":"session_meta","payload":{"id":"codex-session"}}` + "\n"
	valid := meta + token("2026-09-18T00:00:00Z", 100, 20, 0, 0)
	write(t, path, valid+"{invalid}\n")
	if err := i.Scan(context.Background()); err == nil {
		t.Fatal("expected import error")
	}
	if got := total(t, s, "t3"); got.Total != 0 {
		t.Fatal("failed transaction persisted usage")
	}
	write(t, path, valid)
	if err := i.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := total(t, s, "t3"); got.Total != 120 {
		t.Fatalf("repair lost usage: %+v", got)
	}
}

func TestReadOnlySourceAndDurableLedger(t *testing.T) {
	s, i, root := fixture(t)
	db, err := sql.Open("sqlite", fileDSN(i.T3DB, true))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err = db.Exec("DELETE FROM provider_session_runtime"); err == nil {
		t.Fatal("T3 connection was writable")
	}
	e := Event{ID: "event", Source: "t3-codex", At: "2026-09-18T00:00:00Z", Input: 10, Output: 5, Total: 15}
	if err = s.Put(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	other, err := Open(filepath.Join(root, "usage.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close() }()
	if got := total(t, other, "t3"); got.Total != 15 {
		t.Fatalf("not durable: %+v", got)
	}
}

func TestProxyPluginAndSeparateViews(t *testing.T) {
	s, _, _ := fixture(t)
	p := &Plugin{Store: s}
	p.HandleUsage(context.Background(), usage.Record{Provider: "codex", Model: "test", APIKey: "must-not-be-stored", Detail: usage.Detail{InputTokens: 100, OutputTokens: 20, CachedTokens: 60, CacheReadTokens: 60, ReasoningTokens: 5, TotalTokens: 120}})
	if got := total(t, s, "proxy"); got.Total != 120 || got.Cached != 60 || got.Reasoning != 5 {
		t.Fatalf("proxy accounting: %+v", got)
	}
	if got := total(t, s, "t3"); got.Total != 0 {
		t.Fatal("proxy mixed into T3 totals")
	}
	var schema string
	if err := s.db.QueryRow("SELECT sql FROM sqlite_master WHERE name='usage_events'").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(schema, "api_key") {
		t.Fatal("secret persisted")
	}
}

func TestHTTPDateBoundariesAndLocalAccess(t *testing.T) {
	s, i, _ := fixture(t)
	for _, e := range []Event{{ID: "first", Source: "t3-codex", At: "2026-09-18T00:00:00Z", Input: 10, Total: 10}, {ID: "last", Source: "t3-codex", At: "2026-09-18T23:59:59.999Z", Input: 20, Total: 20}, {ID: "next", Source: "t3-codex", At: "2026-09-19T00:00:00Z", Input: 40, Total: 40}} {
		if err := s.Put(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
	h := Handler(s, i, "127.0.0.1:8318")
	r := httptest.NewRequest("GET", "http://127.0.0.1:8318/api/summary?from=2026-09-18&to=2026-09-18", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var body struct {
		Total []Totals `json:"total"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Total) != 1 || body.Total[0].Total != 30 {
		t.Fatalf("date boundary: %s", w.Body.String())
	}
	for _, tc := range []struct {
		url, origin, method string
		code                int
	}{{"http://evil.example:8318/api/summary", "", "GET", 403}, {"http://127.0.0.1:8318/api/summary", "https://evil.example", "GET", 403}, {"http://127.0.0.1:8318/api/summary", "", "POST", 405}, {"http://127.0.0.1:8318/api/summary?source=all", "", "GET", 400}, {"http://127.0.0.1:8318/api/summary?from=invalid", "", "GET", 400}} {
		r = httptest.NewRequest(tc.method, tc.url, nil)
		r.Header.Set("Origin", tc.origin)
		w = httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.code {
			t.Fatalf("%s: got %d", tc.url, w.Code)
		}
	}
	for _, v := range []string{"=CMD()", " +1", "@SUM(A1)", "\tcommand"} {
		if !strings.HasPrefix(csvSafe(v), "'") {
			t.Fatalf("unsafe CSV: %q", v)
		}
	}
}
