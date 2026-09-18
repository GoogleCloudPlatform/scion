// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Command a2a-testserver runs a minimal A2A SDK HTTP server backed by
// PostgresTaskStore for cross-process integration tests.
//
// Usage:
//
//	a2a-testserver -port=PORT -database-url=URL [-project=PROJ] [-agent=AGENT] [-caller-id=ID]
//
// The server prints "READY <pid> <port>" to stdout once listening, then serves
// A2A JSON-RPC requests until stdin is closed or SIGTERM is received.
package main

import (
	"context"
	"flag"
	"fmt"
	"iter"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/internal/bridge"
)

var (
	port        = flag.Int("port", 0, "port to listen on (0 for random)")
	databaseURL = flag.String("database-url", "", "Postgres connection URL")
	project     = flag.String("project", "test-proj", "project slug for route context")
	agent       = flag.String("agent", "test-agent", "agent slug for route context")
	callerID    = flag.String("caller-id", "", "optional caller identity (UserID) for ownership isolation")
)

// testExecutor is a minimal AgentExecutor that creates tasks and transitions
// them to completed without requiring a real Scion Hub.
type testExecutor struct{}

func (e *testExecutor) Execute(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		// Emit submitted task.
		if execCtx.StoredTask == nil {
			task := a2a.NewSubmittedTask(execCtx, execCtx.Message)
			if !yield(task, nil) {
				return
			}
		}

		// Emit working status.
		if !yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateWorking, nil), nil) {
			return
		}

		// Emit completed status with the echoed message.
		parts := execCtx.Message.Parts
		var sdkParts []*a2a.Part
		for _, p := range parts {
			sdkParts = append(sdkParts, p)
		}
		if len(sdkParts) == 0 {
			sdkParts = append(sdkParts, a2a.NewTextPart("echo"))
		}
		replyMsg := a2a.NewMessageForTask(a2a.MessageRoleAgent, execCtx, sdkParts...)
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCompleted, replyMsg), nil)
	}
}

func (e *testExecutor) Cancel(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCanceled, nil), nil)
	}
}

func main() {
	flag.Parse()
	if *databaseURL == "" {
		fmt.Fprintln(os.Stderr, "missing -database-url")
		os.Exit(1)
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	store, err := bridge.NewPostgresTaskStore(*databaseURL)
	if err != nil {
		log.Error("failed to create store", "error", err)
		os.Exit(1)
	}
	defer store.Close()

	handler := a2asrv.NewHandler(
		&testExecutor{},
		a2asrv.WithTaskStore(store),
		a2asrv.WithLogger(log),
	)

	jsonrpcHandler := a2asrv.NewJSONRPCHandler(handler)

	// Wrap with middleware that sets RouteInfo and optional CallerIdentity.
	routeHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := bridge.WithRouteInfo(r.Context(), bridge.RouteInfo{
			ProjectSlug: *project,
			AgentSlug:   *agent,
		})
		if *callerID != "" {
			ctx = bridge.WithCallerIdentity(ctx, &bridge.CallerIdentity{
				UserID:    *callerID,
				TokenType: "uat",
			})
		}
		jsonrpcHandler.ServeHTTP(w, r.WithContext(ctx))
	})

	mux := http.NewServeMux()
	mux.Handle("/", routeHandler)

	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *port))
	if err != nil {
		log.Error("listen failed", "error", err)
		os.Exit(1)
	}

	actualPort := listener.Addr().(*net.TCPAddr).Port

	server := &http.Server{Handler: mux}

	// Print readiness signal with PID and port.
	fmt.Fprintf(os.Stdout, "READY %d %d\n", os.Getpid(), actualPort)
	os.Stdout.Sync()

	// Shutdown on signal or stdin close.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		select {
		case <-sigCh:
		case <-ctx.Done():
		}
		server.Shutdown(context.Background())
	}()

	// Also shutdown when stdin is closed (parent process exits).
	go func() {
		buf := make([]byte, 1)
		for {
			_, err := os.Stdin.Read(buf)
			if err != nil {
				cancel()
				return
			}
		}
	}()

	if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
		log.Error("server error", "error", err)
		os.Exit(1)
	}
}
