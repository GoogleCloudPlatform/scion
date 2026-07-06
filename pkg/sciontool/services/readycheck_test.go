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

package services

import (
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

func TestReadyCheck_TCP(t *testing.T) {
	// Start a TCP listener
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start listener: %v", err)
	}
	defer func() { _ = listener.Close() }()

	addr := listener.Addr().String()

	check := &api.ReadyCheck{
		Type:    "tcp",
		Target:  addr,
		Timeout: "5s",
	}

	if err := waitForReady(check); err != nil {
		t.Errorf("TCP ready check failed unexpectedly: %v", err)
	}
}

func TestReadyCheck_HTTP(t *testing.T) {
	// Start an HTTP server
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "ok")
	})

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start listener: %v", err)
	}

	server := &http.Server{Handler: mux}
	go func() { _ = server.Serve(listener) }()
	defer func() { _ = server.Close() }()

	addr := listener.Addr().String()
	check := &api.ReadyCheck{
		Type:    "http",
		Target:  fmt.Sprintf("http://%s/health", addr),
		Timeout: "5s",
	}

	if err := waitForReady(check); err != nil {
		t.Errorf("HTTP ready check failed unexpectedly: %v", err)
	}
}

func TestReadyCheck_Delay(t *testing.T) {
	check := &api.ReadyCheck{
		Type:    "delay",
		Target:  "500ms",
		Timeout: "1s",
	}

	start := time.Now()
	if err := waitForReady(check); err != nil {
		t.Errorf("Delay ready check failed: %v", err)
	}
	elapsed := time.Since(start)

	if elapsed < 400*time.Millisecond {
		t.Errorf("delay check returned too quickly: %v", elapsed)
	}
}

func TestReadyCheck_Timeout(t *testing.T) {
	// Use a port that nothing is listening on
	check := &api.ReadyCheck{
		Type:    "tcp",
		Target:  "127.0.0.1:1", // port 1 is almost certainly not listening
		Timeout: "1s",
	}

	start := time.Now()
	err := waitForReady(check)
	elapsed := time.Since(start)

	if err == nil {
		t.Error("expected timeout error, got nil")
	}

	if elapsed < 900*time.Millisecond {
		t.Errorf("timeout returned too quickly: %v", elapsed)
	}
}

func TestReadyCheck_InvalidTimeout(t *testing.T) {
	check := &api.ReadyCheck{
		Type:    "tcp",
		Target:  "localhost:8080",
		Timeout: "not-a-duration",
	}

	if err := waitForReady(check); err == nil {
		t.Error("expected error for invalid timeout, got nil")
	}
}

func TestReadyCheck_InvalidDelayTarget(t *testing.T) {
	check := &api.ReadyCheck{
		Type:    "delay",
		Target:  "not-a-duration",
		Timeout: "1s",
	}

	if err := waitForReady(check); err == nil {
		t.Error("expected error for invalid delay target, got nil")
	}
}
