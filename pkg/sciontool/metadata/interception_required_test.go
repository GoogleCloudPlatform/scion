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
	t.Cleanup(func() {
		runSetupIPTablesRedirect, runSetupMetadataBlock = origRedirect, origBlock
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
	return &r, &b
}

func TestConfigureMetadataInterception_Required(t *testing.T) {
	failure := errors.New("exit status 4")
	tests := []struct {
		name        string
		mode        string
		networkMode string
		uid         int
		required    bool
		redirectErr error
		blockErr    error
		wantErr     bool
		wantBlock   bool
	}{
		{name: "required block all in place", mode: modeBlock, required: true, wantBlock: true},
		{name: "required assign redirect in place", mode: modeAssign, required: true},
		{name: "required block redirect fails", mode: modeBlock, required: true, redirectErr: failure, wantErr: true},
		{name: "required assign redirect fails", mode: modeAssign, required: true, redirectErr: failure, wantErr: true},
		{name: "required block filter rule fails", mode: modeBlock, required: true, blockErr: failure, wantErr: true, wantBlock: true},
		{name: "required non-root", mode: modeBlock, uid: 1000, required: true, wantErr: true},
		{name: "required host network", mode: modeBlock, networkMode: "host", required: true, wantErr: true},
		{name: "not required redirect fails", mode: modeBlock, redirectErr: failure, wantBlock: true},
		{name: "not required filter rule fails", mode: modeBlock, blockErr: failure, wantBlock: true},
		{name: "not required non-root", mode: modeAssign, uid: 1000},
		{name: "not required host network", mode: modeBlock, networkMode: "host"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			redirectCalled, blockCalled := stubInterception(t, tt.redirectErr, tt.blockErr)
			s := New(Config{Mode: tt.mode, NetworkMode: tt.networkMode, InterceptionRequired: tt.required, Port: 18380})
			err := s.configureMetadataInterception(tt.uid)
			if (err != nil) != tt.wantErr {
				t.Fatalf("configureMetadataInterception() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil && !errors.Is(err, ErrInterceptionUnavailable) {
				t.Errorf("error %v is not ErrInterceptionUnavailable", err)
			}
			attempted := tt.uid == 0 && tt.networkMode != "host"
			if *redirectCalled != attempted {
				t.Errorf("redirect attempted = %v, want %v", *redirectCalled, attempted)
			}
			if *blockCalled != tt.wantBlock {
				t.Errorf("filter rule attempted = %v, want %v", *blockCalled, tt.wantBlock)
			}
		})
	}
}

// TestStart_InterceptionRequiredUnavailable_ReleasesPort checks that a Start
// that fails on a required interception returns ErrInterceptionUnavailable
// and leaves no listener behind. The redirect is stubbed to fail, so the
// result is the same whether or not the test process is root.
func TestStart_InterceptionRequiredUnavailable_ReleasesPort(t *testing.T) {
	stubInterception(t, errors.New("exit status 4"), nil)
	port := freePort(t)
	s := New(Config{Mode: modeBlock, Port: port, ProjectID: "test-project", InterceptionRequired: true})
	err := s.Start(context.Background())
	if !errors.Is(err, ErrInterceptionUnavailable) {
		t.Fatalf("Start() error = %v, want ErrInterceptionUnavailable", err)
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("port %d still held after the failed Start: %v", port, err)
	}
	_ = ln.Close()
}

// TestInterceptionInstalled pins what init's CAP_NET_ADMIN drop keys its
// fail-closed decision on: true once any rule is in place, false when
// interception was skipped or nothing could be installed.
func TestInterceptionInstalled(t *testing.T) {
	failure := errors.New("exit status 4")
	tests := []struct {
		name                  string
		mode                  string
		networkMode           string
		uid                   int
		redirectErr, blockErr error
		want                  bool
	}{
		{name: "assign redirect", mode: modeAssign, want: true},
		{name: "assign redirect fails", mode: modeAssign, redirectErr: failure, want: false},
		{name: "block both", mode: modeBlock, want: true},
		{name: "block redirect fails, filter in place", mode: modeBlock, redirectErr: failure, want: true},
		{name: "block filter fails, redirect in place", mode: modeBlock, blockErr: failure, want: true},
		{name: "block both fail", mode: modeBlock, redirectErr: failure, blockErr: failure, want: false},
		{name: "non-root skipped", mode: modeAssign, uid: 1000, want: false},
		{name: "host network skipped", mode: modeBlock, networkMode: "host", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stubInterception(t, tt.redirectErr, tt.blockErr)
			s := New(Config{Mode: tt.mode, NetworkMode: tt.networkMode, Port: 18380})
			_ = s.configureMetadataInterception(tt.uid)
			if got := s.InterceptionInstalled(); got != tt.want {
				t.Errorf("InterceptionInstalled() = %v, want %v", got, tt.want)
			}
		})
	}
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
	if s.InterceptionInstalled() {
		t.Error("interception installed on a stopped server")
	}
	if !portFree(t, port) {
		t.Fatalf("port %d bound by Start on a stopped server", port)
	}
}

// TestStart_StopBeforeServe_Backstop: a Stop that lands after Start's entry
// check but before it installs the server (forced here through
// startBeforeServeHook) makes Start refuse, release the port and remove
// any interception it installed.
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
	if s.InterceptionInstalled() {
		t.Error("interception left installed after the backstop refused")
	}
	if !portFree(t, port) {
		t.Fatalf("port %d still bound after the backstop refused", port)
	}
}
