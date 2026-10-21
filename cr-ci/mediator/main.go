// Command mediator is the cr-ci Cloud Run service that turns GitHub
// workflow_job webhooks into Cloud Run Job executions running one ephemeral
// GitHub Actions runner each.
//
// P1 STUB: it only validates the config contract and serves /health (not
// /healthz, which Cloud Run reserves). The webhook, claim and reconcile
// endpoints arrive in P2 (see cr-ci/README.md).
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	if err := run(os.Getenv, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(getenv func(string) string, logOut io.Writer) error {
	logger := slog.New(slog.NewJSONHandler(logOut, nil))
	cfg, err := Load(getenv)
	if err != nil {
		return err
	}
	logger.Info("config loaded",
		"target_repos", len(cfg.TargetRepos),
		"install_scope", string(cfg.InstallScope),
		"runner_label", cfg.RunnerLabel,
		"runner_job", cfg.RunnerJobName,
		"max_concurrent_runners", cfg.MaxConcurrentRunners)

	port := getenv("PORT") // set by Cloud Run
	if port == "" {
		port = "8080"
	}
	srv := &http.Server{
		Handler:           newMux(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	ln, err := net.Listen("tcp", ":"+port)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return serve(ctx, srv, ln, logger)
}

// serve runs srv until ctx is cancelled, then drains in-flight requests
// (Cloud Run sends SIGTERM and allows 10 s) before returning.
func serve(ctx context.Context, srv *http.Server, ln net.Listener, logger *slog.Logger) error {
	drained := make(chan error, 1)
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 9*time.Second)
		defer cancel()
		drained <- srv.Shutdown(shutdownCtx)
	}()

	logger.Info("listening", "addr", ln.Addr().String())
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	// Serve returns as soon as Shutdown starts; wait for the drain.
	return <-drained
}

func newMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "ok\n")
	})
	return mux
}
