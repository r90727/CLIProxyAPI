package localusage

import (
	"context"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

// RegisterFromEnv opts the proxy into the same durable ledger as the T3 importer.
// Only accounting fields are persisted; keys, headers and request bodies are omitted.
func RegisterFromEnv() error {
	path := os.Getenv("T3_USAGE_DB")
	if path == "" {
		return nil
	}
	s, err := Open(path)
	if err != nil {
		return err
	}
	usage.RegisterPlugin(&Plugin{Store: s})
	return nil
}

type Plugin struct{ Store *Store }

func (p *Plugin) HandleUsage(_ context.Context, r usage.Record) {
	d := usage.EnsureTokenBreakdownForProvider(r.Detail, r.Provider, r.ExecutorType).TokenBreakdown
	at := r.RequestedAt
	if at.IsZero() {
		at = time.Now()
	}
	err := p.Store.Put(context.Background(), Event{ID: "proxy:" + uuid.NewString(), Source: "proxy", Session: r.SessionID, Provider: r.Provider, Model: r.Model, At: at.Format(time.RFC3339Nano), Input: d.Input.TotalTokens, Output: d.Output.TotalTokens, Cached: d.Input.CacheReadTokens, CacheWrite: d.Input.CacheWriteTokens, Reasoning: d.Output.ReasoningTokens, Total: d.TotalTokens, Failed: r.Failed})
	if err != nil {
		log.WithError(err).Error("persist local token usage")
	}
}
