package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHealth(t *testing.T) {
	srv := httptest.NewServer(newMux())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "ok\n" {
		t.Errorf("GET /health = %d %q, want 200 \"ok\\n\"", resp.StatusCode, body)
	}

	resp2, err := http.Get(srv.URL + "/webhook")
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Errorf("GET /webhook = %d, want 404 until P2", resp2.StatusCode)
	}
}

func TestRunRejectsInvalidConfig(t *testing.T) {
	var log strings.Builder
	err := run(func(string) string { return "" }, &log)
	if err == nil || !strings.Contains(err.Error(), "invalid cr-ci config") {
		t.Fatalf("run with empty env: err = %v", err)
	}
	if log.Len() != 0 {
		t.Errorf("unexpected log output before config validation: %q", log.String())
	}
}

// TestServeDrainsBeforeReturning checks that serve waits for in-flight
// requests to finish after shutdown starts, instead of returning as soon as
// the listener closes.
func TestServeDrainsBeforeReturning(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /slow", func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		_, _ = io.WriteString(w, "done")
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: time.Second}
	served := make(chan error, 1)
	go func() { served <- serve(ctx, srv, ln, slog.New(slog.NewTextHandler(io.Discard, nil))) }()

	got := make(chan string, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/slow")
		if err != nil {
			got <- "error: " + err.Error()
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		got <- string(b)
	}()
	<-entered
	cancel()

	select {
	case err := <-served:
		t.Fatalf("serve returned (%v) while a request was still in flight", err)
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	if err := <-served; err != nil {
		t.Fatalf("serve: %v", err)
	}
	if body := <-got; body != "done" {
		t.Errorf("in-flight request got %q, want %q", body, "done")
	}
}
