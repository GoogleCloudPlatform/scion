package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHealthz(t *testing.T) {
	srv := httptest.NewServer(newMux())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "ok\n" {
		t.Errorf("GET /healthz = %d %q, want 200 \"ok\\n\"", resp.StatusCode, body)
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
