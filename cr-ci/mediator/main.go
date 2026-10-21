// Command mediator is the cr-ci Cloud Run service that turns GitHub
// workflow_job webhooks into Cloud Run Job executions running one ephemeral
// GitHub Actions runner each.
//
// P1 STUB: it only validates the config contract and serves /healthz. The
// webhook, claim and reconcile endpoints arrive in P2 (see cr-ci/README.md).
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
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
		Addr:              ":" + port,
		Handler:           newMux(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	logger.Info("listening", "port", port)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func newMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "ok\n")
	})
	return mux
}
