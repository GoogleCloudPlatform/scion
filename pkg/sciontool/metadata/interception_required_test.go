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

package metadata

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestConfigFromEnv_InterceptionRequired(t *testing.T) {
	tests := []struct {
		value string
		set   bool
		want  bool
	}{
		{value: "required", set: true, want: true},
		{value: "off", set: true, want: false},
		{value: "", set: true, want: false},
		{value: "REQUIRED", set: true, want: false},
		{set: false, want: false},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%q set=%v", tt.value, tt.set), func(t *testing.T) {
			t.Setenv("SCION_METADATA_MODE", "block")
			t.Setenv(InterceptionEnvVar, tt.value)
			if !tt.set {
				if err := os.Unsetenv(InterceptionEnvVar); err != nil {
					t.Fatal(err)
				}
			}
			cfg := ConfigFromEnv()
			if cfg == nil {
				t.Fatal("ConfigFromEnv() = nil, want a block-mode config")
			}
			if cfg.InterceptionRequired != tt.want {
				t.Errorf("InterceptionRequired = %v, want %v", cfg.InterceptionRequired, tt.want)
			}
		})
	}
}

func TestStartFailureIsFatal(t *testing.T) {
	listenErr := errors.New("metadata server listen: address already in use")
	interceptErr := fmt.Errorf("%w: filter rule: exit status 1", ErrInterceptionUnavailable)
	tests := []struct {
		name        string
		mode        string
		networkMode string
		err         error
		want        bool
	}{
		{name: "no error", mode: modeAssign, err: nil, want: false},
		{name: "assign bridge", mode: modeAssign, err: listenErr, want: true},
		{name: "assign host", mode: modeAssign, networkMode: "host", err: listenErr, want: true},
		{name: "block bridge", mode: modeBlock, err: listenErr, want: true},
		{name: "block host", mode: modeBlock, networkMode: "host", err: listenErr, want: false},
		{name: "block host interception unavailable", mode: modeBlock, networkMode: "host", err: interceptErr, want: true},
		{name: "block bridge interception unavailable", mode: modeBlock, err: interceptErr, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{Mode: tt.mode, NetworkMode: tt.networkMode}
			if got := cfg.StartFailureIsFatal(tt.err); got != tt.want {
				t.Errorf("StartFailureIsFatal(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// stubInterception replaces the iptables helpers for one test and records
// whether each ran.
func stubInterception(t *testing.T, redirectErr, blockErr error) (redirectCalled, blockCalled *bool) {
	t.Helper()
	origRedirect, origBlock := runSetupIPTablesRedirect, runSetupMetadataBlock
	origHasIPv6, origIPv6Block := runNetnsHasIPv6, runSetupIPv6MetadataBlock
	t.Cleanup(func() {
		runSetupIPTablesRedirect, runSetupMetadataBlock = origRedirect, origBlock
		runNetnsHasIPv6, runSetupIPv6MetadataBlock = origHasIPv6, origIPv6Block
	})
	var r, b bool
	runSetupIPTablesRedirect = func(int) error {
		r = true
		return redirectErr
	}
	runSetupMetadataBlock = func() (blockMethod, error) {
		b = true
		if blockErr != nil {
			return blockNone, blockErr
		}
		return blockIPTables, nil
	}
	runNetnsHasIPv6 = func() bool { return false }
	runSetupIPv6MetadataBlock = func() error { return nil }
	return &r, &b
}

// TestStop_Idempotent: sciontool init stops the server from a deferred call
// on every return path and again on normal shutdown, so a second Stop on a
// started server, and Stop on a server that never started, must be safe and
// leave the port free.
func TestStop_Idempotent(t *testing.T) {
	stubInterception(t, nil, nil)

	t.Run("started", func(t *testing.T) {
		port := freePort(t)
		s := New(Config{Mode: modeBlock, Port: port})
		if err := s.Start(context.Background()); err != nil {
			t.Fatalf("Start: %v", err)
		}
		s.Stop()
		s.Stop()
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			t.Fatalf("port %d still bound after two Stops: %v", port, err)
		}
		_ = ln.Close()
	})

	t.Run("never started", func(t *testing.T) {
		s := New(Config{Mode: modeAssign, Port: freePort(t)})
		s.Stop()
		s.Stop()
	})
}

// freePort returns an available loopback TCP port.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

// portFree reports whether 127.0.0.1:port can be bound right now.
func portFree(t *testing.T, port int) bool {
	t.Helper()
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

// TestRestartHTTP_AfterStop: a health-check restart that runs after Stop
// must not bind the port again.
func TestRestartHTTP_AfterStop(t *testing.T) {
	stubInterception(t, nil, nil)
	port := freePort(t)
	s := New(Config{Mode: modeBlock, Port: port})
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	s.Stop()
	if err := s.restartHTTP(context.Background()); !errors.Is(err, errRestartAfterStop) {
		t.Fatalf("restartHTTP after Stop = %v, want errRestartAfterStop", err)
	}
	if !portFree(t, port) {
		t.Fatalf("port %d bound after a restart following Stop", port)
	}
}

// TestStart_AfterStop: Start on a stopped server refuses at its entry,
// before binding or installing any interception.
func TestStart_AfterStop(t *testing.T) {
	redirectCalled, blockCalled := stubInterception(t, nil, nil)
	orig := startBeforeServeHook
	startBeforeServeHook = func(*Server) { t.Error("Start went past its entry check on a stopped server") }
	t.Cleanup(func() { startBeforeServeHook = orig })

	port := freePort(t)
	s := New(Config{Mode: modeBlock, Port: port})
	s.Stop()
	if err := s.Start(context.Background()); !errors.Is(err, errStartAfterStop) {
		t.Fatalf("Start after Stop = %v, want errStartAfterStop", err)
	}
	if *redirectCalled || *blockCalled {
		t.Errorf("interception attempted on a stopped server (redirect %v, block %v)", *redirectCalled, *blockCalled)
	}
	if !portFree(t, port) {
		t.Fatalf("port %d bound by Start on a stopped server", port)
	}
}

// TestStart_StopBeforeServe_Backstop: a Stop that lands after Start's entry
// check but before it installs the server (forced here through
// startBeforeServeHook) makes Start refuse and release the port.
func TestStart_StopBeforeServe_Backstop(t *testing.T) {
	stubInterception(t, nil, nil)
	orig := startBeforeServeHook
	startBeforeServeHook = func(s *Server) { s.Stop() }
	t.Cleanup(func() { startBeforeServeHook = orig })

	port := freePort(t)
	s := New(Config{Mode: modeBlock, Port: port})
	if err := s.Start(context.Background()); !errors.Is(err, errStartAfterStop) {
		t.Fatalf("Start with a Stop before serve = %v, want errStartAfterStop", err)
	}
	if !portFree(t, port) {
		t.Fatalf("port %d still bound after the backstop refused", port)
	}
}

func TestConfigFromEnv_InterceptionSource(t *testing.T) {
	t.Setenv("SCION_METADATA_MODE", "block")
	t.Setenv(InterceptionEnvVar, InterceptionRequired)
	t.Setenv(InterceptionSourceEnvVar, InterceptionSourceRuntime)
	cfg := ConfigFromEnv()
	if cfg == nil {
		t.Fatal("ConfigFromEnv() = nil")
	}
	if !cfg.InterceptionRequired {
		t.Error("InterceptionRequired = false, want true")
	}
	if cfg.InterceptionSource != InterceptionSourceRuntime {
		t.Errorf("InterceptionSource = %q, want %q", cfg.InterceptionSource, InterceptionSourceRuntime)
	}
}

func TestProbeHandler_ExactPathEveryMode(t *testing.T) {
	for _, mode := range []string{modeAssign, modeBlock, "unknown-coerced"} {
		t.Run(mode, func(t *testing.T) {
			s := New(Config{Mode: mode, Port: 18380})
			s2 := New(Config{Mode: mode, Port: 18380})
			path := s.ProbePath()
			token := s.ProbeToken()
			if len(path) != 65 || path[0] != '/' {
				t.Fatalf("ProbePath() = %q, want leading '/' + 64 hex chars", path)
			}
			if len(token) != 64 {
				t.Fatalf("ProbeToken() = %q, want 64 hex chars", token)
			}
			if token == path[1:] {
				t.Fatal("ProbeToken() must be independent of ProbePath()")
			}
			if token == s2.ProbeToken() || path == s2.ProbePath() {
				t.Fatal("distinct Server instances must generate distinct probe paths and tokens")
			}
			h := s.buildMux()

			// Exact probe path answers 200 with text/plain, Cache-Control: no-store,
			// and the per-boot probe token without requiring Metadata-Flavor.
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			if rec.Code != http.StatusOK {
				t.Errorf("GET %s status = %d, want 200", path, rec.Code)
			}
			if got := rec.Header().Get("Content-Type"); got != "text/plain" {
				t.Errorf("Content-Type = %q, want text/plain", got)
			}
			if got := rec.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", got)
			}
			if got := rec.Body.String(); got != token {
				t.Errorf("body = %q, want %q", got, token)
			}

			// Wrong paths return 404 when Metadata-Flavor is present and never
			// return the probe token.
			for _, other := range []string{path + "/extra", "/not-the-probe"} {
				req := httptest.NewRequest(http.MethodGet, other, nil)
				req.Header.Set("Metadata-Flavor", "Google")
				rec2 := httptest.NewRecorder()
				h.ServeHTTP(rec2, req)
				if rec2.Code != http.StatusNotFound {
					t.Errorf("GET %s (with Metadata-Flavor) status = %d, want 404", other, rec2.Code)
				}
				if rec2.Body.String() == token {
					t.Errorf("GET %s unexpectedly returned probe token", other)
				}
			}
		})
	}
}

func TestStart_RuntimeSourceSkipsInContainerInterception(t *testing.T) {
	redirectCalled, blockCalled := stubInterception(t, errors.New("should not run"), errors.New("should not run"))
	port := freePort(t)
	s := New(Config{
		Mode:                 modeBlock,
		Port:                 port,
		InterceptionRequired: true,
		InterceptionSource:   InterceptionSourceRuntime,
	})
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v, want nil", err)
	}
	t.Cleanup(s.Stop)
	if *redirectCalled || *blockCalled {
		t.Errorf("in-container interception ran under runtime source (redirect=%v, block=%v)", *redirectCalled, *blockCalled)
	}
}

func TestInstallInterceptionRules(t *testing.T) {
	failure := errors.New("exit status 4")
	tests := []struct {
		name        string
		mode        string
		hasIPv6     bool
		redirectErr error
		blockErr    error
		ipv6Err     error
		wantErr     bool
		wantBlock   bool
		wantIPv6    bool
	}{
		{name: "block succeeds no ipv6", mode: modeBlock, wantBlock: true},
		{name: "assign succeeds no ipv6", mode: modeAssign, wantBlock: false},
		{name: "block succeeds with ipv6", mode: modeBlock, hasIPv6: true, wantBlock: true, wantIPv6: true},
		{name: "assign succeeds with ipv6", mode: modeAssign, hasIPv6: true, wantBlock: false, wantIPv6: true},
		{name: "redirect fails", mode: modeBlock, hasIPv6: true, redirectErr: failure, wantErr: true},
		{name: "filter fails", mode: modeBlock, hasIPv6: true, blockErr: failure, wantErr: true, wantBlock: true},
		{name: "ipv6 fails in block mode", mode: modeBlock, hasIPv6: true, ipv6Err: failure, wantErr: true, wantBlock: true, wantIPv6: true},
		{name: "ipv6 fails in assign mode", mode: modeAssign, hasIPv6: true, ipv6Err: failure, wantErr: true, wantBlock: false, wantIPv6: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			redirectCalled, blockCalled := stubInterception(t, tt.redirectErr, tt.blockErr)
			var ipv6Called bool
			runNetnsHasIPv6 = func() bool { return tt.hasIPv6 }
			runSetupIPv6MetadataBlock = func() error {
				ipv6Called = true
				return tt.ipv6Err
			}
			err := InstallInterceptionRules(tt.mode, 18380)
			if (err != nil) != tt.wantErr {
				t.Fatalf("InstallInterceptionRules() = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr && !errors.Is(err, ErrInterceptionUnavailable) {
				t.Errorf("error = %v, want wrapped ErrInterceptionUnavailable", err)
			}
			if !*redirectCalled {
				t.Error("redirect was not called")
			}
			if *blockCalled != tt.wantBlock {
				t.Errorf("block called = %v, want %v", *blockCalled, tt.wantBlock)
			}
			if ipv6Called != tt.wantIPv6 {
				t.Errorf("ipv6 block called = %v, want %v", ipv6Called, tt.wantIPv6)
			}
		})
	}
}
