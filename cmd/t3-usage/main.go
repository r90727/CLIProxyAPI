// t3-usage serves a single-machine usage dashboard and imports T3 session telemetry.
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/localusage"
	log "github.com/sirupsen/logrus"
)

func main() {
	if err := run(); err != nil {
		log.WithError(err).Error("T3 usage stopped")
		os.Exit(1)
	}
}

func run() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	db := flag.String("db", filepath.Join(home, ".t3-usage", "usage.sqlite"), "Persistent usage ledger")
	t3 := flag.String("t3-db", filepath.Join(home, ".t3", "userdata", "state.sqlite"), "T3 database, opened read-only")
	codex := flag.String("codex-dir", filepath.Join(home, ".codex", "sessions"), "Codex session logs")
	claude := flag.String("claude-dir", filepath.Join(home, ".claude", "projects"), "Claude session logs")
	port := flag.Int("port", 8318, "Loopback dashboard port")
	interval := flag.Duration("interval", 10*time.Second, "Import interval")
	once := flag.Bool("once", false, "Import once and exit")
	flag.Parse()
	if *interval < time.Second {
		return fmt.Errorf("interval must be at least one second")
	}
	s, err := localusage.Open(*db)
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()
	importer := &localusage.Importer{Store: s, T3DB: *t3, CodexDir: *codex, ClaudeDir: *claude}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err = importer.Scan(ctx); err != nil {
		if *once {
			return err
		}
		log.WithError(err).Warn("T3 import failed; will retry")
	}
	if *once {
		return nil
	}
	addr, err := localusage.LoopbackAddress(*port)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	server := &http.Server{Addr: addr, Handler: localusage.Handler(s, importer, addr)}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	log.Infof("T3 token usage dashboard: http://%s", addr)
	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return server.Close()
		case err = <-serveErr:
			if err == http.ErrServerClosed {
				return nil
			}
			return err
		case <-ticker.C:
			if err = importer.Scan(ctx); err != nil {
				log.WithError(err).Warn("T3 import failed; will retry")
			}
		}
	}
}
