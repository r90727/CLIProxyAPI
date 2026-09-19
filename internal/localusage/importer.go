package localusage

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Session struct{ Thread, Title, Project, Model string }
type Importer struct {
	Store                     *Store
	T3DB, CodexDir, ClaudeDir string
	mu                        sync.Mutex
	lastScan                  string
	lastError                 string
}

type ImportStatus struct {
	LastScan string `json:"last_scan"`
	Error    string `json:"error"`
}

func (i *Importer) Status() ImportStatus {
	i.mu.Lock()
	defer i.mu.Unlock()
	return ImportStatus{i.lastScan, i.lastError}
}

func (i *Importer) Scan(ctx context.Context) (err error) {
	defer func() {
		i.mu.Lock()
		defer i.mu.Unlock()
		i.lastScan = time.Now().UTC().Format(time.RFC3339)
		i.lastError = ""
		if err != nil {
			i.lastError = err.Error()
		}
	}()
	sessions, err := i.sessions(ctx)
	if err != nil {
		return err
	}
	for _, root := range []struct{ path, provider string }{{i.CodexDir, "codex"}, {i.ClaudeDir, "claude"}} {
		if root.path == "" {
			continue
		}
		if _, errStat := os.Stat(root.path); errors.Is(errStat, os.ErrNotExist) {
			continue
		}
		err = filepath.WalkDir(root.path, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if errCtx := ctx.Err(); errCtx != nil {
				return errCtx
			}
			if d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
				return nil
			}
			parent := ""
			if root.provider == "claude" {
				id := strings.TrimSuffix(d.Name(), ".jsonl")
				if _, ok := sessions[id]; !ok {
					for sid := range sessions {
						if strings.Contains(filepath.ToSlash(path), "/"+sid+"/subagents/") {
							parent = sid
							break
						}
					}
					if parent == "" {
						return nil
					}
				}
			}
			return i.importFile(ctx, path, root.provider, parent, sessions)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (i *Importer) sessions(ctx context.Context) (map[string]Session, error) {
	db, err := sql.Open("sqlite", fileDSN(i.T3DB, true))
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	rows, err := db.QueryContext(ctx, `SELECT r.provider_name,r.thread_id,COALESCE(r.resume_cursor_json,'{}'),COALESCE(r.runtime_payload_json,'{}'),COALESCE(t.title,''),COALESCE(p.title,'') FROM provider_session_runtime r LEFT JOIN projection_threads t ON t.thread_id=r.thread_id LEFT JOIN projection_projects p ON p.project_id=t.project_id`)
	if err != nil {
		return nil, fmt.Errorf("read T3 session mappings: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := map[string]Session{}
	for rows.Next() {
		var provider, thread, cursor, payload, title, project string
		if err = rows.Scan(&provider, &thread, &cursor, &payload, &title, &project); err != nil {
			return nil, err
		}
		var c struct {
			ThreadID string `json:"threadId"`
			Resume   string `json:"resume"`
		}
		var p struct {
			Model string `json:"model"`
		}
		if json.Unmarshal([]byte(cursor), &c) != nil {
			continue
		}
		_ = json.Unmarshal([]byte(payload), &p)
		id := c.ThreadID
		if provider == "claudeAgent" || provider == "claude" {
			id = c.Resume
		} else if provider != "codex" {
			continue
		}
		if id == "" {
			continue
		}
		v := Session{thread, title, project, p.Model}
		result[id] = v
		if err = i.saveSession(ctx, id, v); err != nil {
			return nil, err
		}
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	// Keep historical mappings after T3 replaces a runtime's resume cursor.
	old, err := i.Store.db.QueryContext(ctx, "SELECT session,thread,title,project,model FROM sessions")
	if err != nil {
		return nil, err
	}
	defer func() { _ = old.Close() }()
	for old.Next() {
		var id string
		var v Session
		if err = old.Scan(&id, &v.Thread, &v.Title, &v.Project, &v.Model); err != nil {
			return nil, err
		}
		if _, ok := result[id]; !ok {
			result[id] = v
		}
	}
	return result, old.Err()
}

func (i *Importer) saveSession(ctx context.Context, id string, s Session) error {
	_, err := i.Store.db.ExecContext(ctx, `INSERT INTO sessions VALUES(?,?,?,?,?) ON CONFLICT(session) DO UPDATE SET thread=excluded.thread,title=excluded.title,project=excluded.project,model=excluded.model`, id, s.Thread, s.Title, s.Project, s.Model)
	return err
}

type counters struct {
	Input     int64 `json:"input_tokens"`
	Cached    int64 `json:"cached_input_tokens"`
	Write     int64 `json:"cache_write_input_tokens"`
	Output    int64 `json:"output_tokens"`
	Reasoning int64 `json:"reasoning_output_tokens"`
	Total     int64 `json:"total_tokens"`
}
type fileState struct {
	Session, Model string
	Eligible       bool
	Previous       counters
}

type QuotaWindow struct {
	Used    float64 `json:"used_percent"`
	Minutes int64   `json:"window_minutes"`
	Resets  int64   `json:"resets_at"`
}
type Quota struct {
	Primary   *QuotaWindow `json:"primary"`
	Secondary *QuotaWindow `json:"secondary"`
	Plan      string       `json:"plan_type"`
}

func (i *Importer) importFile(ctx context.Context, path, provider, parent string, sessions map[string]Session) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	var offset int64
	var saved string
	var state fileState
	err = i.Store.db.QueryRowContext(ctx, "SELECT offset,state FROM import_files WHERE path=?", path).Scan(&offset, &saved)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if saved != "" {
		if err = json.Unmarshal([]byte(saved), &state); err != nil {
			return err
		}
	}
	if offset > info.Size() {
		offset = 0
		state = fileState{}
	}
	if offset == info.Size() {
		return nil
	}
	if offset == 0 && provider == "claude" {
		state.Session = strings.TrimSuffix(filepath.Base(path), ".jsonl")
		state.Eligible = true
		if parent != "" {
			if v, ok := sessions[parent]; ok {
				sessions[state.Session] = v
				if err = i.saveSession(ctx, state.Session, v); err != nil {
					return err
				}
			}
		}
		state.Model = sessions[state.Session].Model
	}
	if _, err = f.Seek(offset, io.SeekStart); err != nil {
		return err
	}
	tx, err := i.Store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	r := bufio.NewReader(f)
	for {
		line, readErr := r.ReadBytes('\n')
		// An unfinished last line belongs to the next scan; checkpoint only complete lines.
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return readErr
		}
		var envelope struct {
			Type      string          `json:"type"`
			Timestamp string          `json:"timestamp"`
			Payload   json.RawMessage `json:"payload"`
			Message   json.RawMessage `json:"message"`
		}
		if err = json.Unmarshal(line, &envelope); err != nil {
			return fmt.Errorf("invalid complete usage log line at offset %d: %w", offset, err)
		}
		offset += int64(len(line))
		if provider == "codex" {
			switch envelope.Type {
			case "session_meta":
				var meta struct {
					ID         string `json:"id"`
					Originator string `json:"originator"`
					Source     struct {
						Subagent struct {
							Spawn struct {
								Parent string `json:"parent_thread_id"`
							} `json:"thread_spawn"`
						} `json:"subagent"`
					} `json:"source"`
				}
				// source can also be a string, so decode basic identity independently.
				var basic struct {
					ID         string `json:"id"`
					Originator string `json:"originator"`
				}
				_ = json.Unmarshal(envelope.Payload, &basic)
				_ = json.Unmarshal(envelope.Payload, &meta)
				state.Session = basic.ID
				v, linked := sessions[state.Session]
				if !linked && meta.Source.Subagent.Spawn.Parent != "" {
					v, linked = sessions[meta.Source.Subagent.Spawn.Parent]
					if linked {
						_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO sessions VALUES(?,?,?,?,?)`, state.Session, v.Thread, v.Title, v.Project, v.Model)
						if err != nil {
							return err
						}
						sessions[state.Session] = v
					}
				}
				state.Eligible = linked || strings.HasPrefix(basic.Originator, "t3code")
				state.Model = v.Model
				if !state.Eligible {
					return nil
				}
			case "turn_context":
				var c struct {
					Model string `json:"model"`
				}
				_ = json.Unmarshal(envelope.Payload, &c)
				if c.Model != "" {
					state.Model = c.Model
				}
			case "event_msg":
				if !state.Eligible {
					continue
				}
				var p struct {
					Type string `json:"type"`
					Info *struct {
						Total *counters `json:"total_token_usage"`
					} `json:"info"`
					Quota *Quota `json:"rate_limits"`
				}
				if err = json.Unmarshal(envelope.Payload, &p); err != nil {
					return err
				}
				if p.Type != "token_count" {
					continue
				}
				if p.Quota != nil {
					q, _ := json.Marshal(p.Quota)
					_, err = tx.ExecContext(ctx, `INSERT INTO quotas VALUES(?,?,?) ON CONFLICT(session) DO UPDATE SET at=excluded.at,payload=excluded.payload WHERE excluded.at>=quotas.at`, state.Session, envelope.Timestamp, string(q))
					if err != nil {
						return err
					}
				}
				if p.Info == nil || p.Info.Total == nil {
					continue
				}
				c := *p.Info.Total
				prev := state.Previous
				if c == prev {
					continue
				}
				// Cumulative counters can reset when a session resumes or compacts.
				if c.Input < prev.Input || c.Output < prev.Output {
					prev = counters{}
				}
				e := Event{Source: "t3-codex", Session: state.Session, Provider: "codex", Model: state.Model, At: envelope.Timestamp, Input: max(0, c.Input-prev.Input), Output: max(0, c.Output-prev.Output), Cached: max(0, c.Cached-prev.Cached), CacheWrite: max(0, c.Write-prev.Write), Reasoning: max(0, c.Reasoning-prev.Reasoning)}
				e.Total = e.Input + e.Output
				// Stable across replays, renamed files, and repeated token_count notifications.
				raw, _ := json.Marshal(c)
				e.ID = fmt.Sprintf("codex:%s:%x", state.Session, sha256.Sum256(append([]byte(envelope.Timestamp), raw...)))
				if e.Total > 0 {
					if err = putEvent(ctx, tx, e); err != nil {
						return err
					}
				}
				state.Previous = c
			}
		} else if envelope.Type == "assistant" {
			var m struct {
				ID    string `json:"id"`
				Model string `json:"model"`
				Usage *struct {
					Input   int64 `json:"input_tokens"`
					Output  int64 `json:"output_tokens"`
					Read    int64 `json:"cache_read_input_tokens"`
					Write   int64 `json:"cache_creation_input_tokens"`
					Details struct {
						Thinking int64 `json:"thinking_tokens"`
					} `json:"output_tokens_details"`
				} `json:"usage"`
			}
			if err = json.Unmarshal(envelope.Message, &m); err != nil {
				return err
			}
			if m.Usage == nil || m.ID == "" {
				continue
			}
			u := m.Usage
			e := Event{ID: "claude:" + state.Session + ":" + m.ID, Source: "t3-claude", Session: state.Session, Provider: "claude", Model: m.Model, At: envelope.Timestamp, Input: u.Input + u.Read + u.Write, Output: u.Output, Cached: u.Read, CacheWrite: u.Write, Reasoning: u.Details.Thinking, Total: u.Input + u.Read + u.Write + u.Output}
			if e.Total == 0 {
				continue
			}
			if err = putEvent(ctx, tx, e); err != nil {
				return err
			}
		}
	}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO import_files VALUES(?,?,?) ON CONFLICT(path) DO UPDATE SET offset=excluded.offset,state=excluded.state`, path, offset, string(data))
	if err != nil {
		return err
	}
	return tx.Commit()
}
