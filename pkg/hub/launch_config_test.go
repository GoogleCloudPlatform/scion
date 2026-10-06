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

//go:build !no_sqlite

// This file covers ServerConfig defaulting and the minimum clamp for
// LaunchTimeout, mirroring the StalledThreshold tests in
// stalled_detection_test.go.
package hub

import (
	"context"
	"testing"
	"time"
)

func TestNew_DefaultsLaunchTimeoutWhenZero(t *testing.T) {
	srv, _ := testServer(t)
	defaultTimeout := DefaultServerConfig().LaunchTimeout
	if srv.config.LaunchTimeout != defaultTimeout {
		t.Errorf("LaunchTimeout = %v, want %v", srv.config.LaunchTimeout, defaultTimeout)
	}
}

func TestNew_ClampsLaunchTimeoutBelowMinimum(t *testing.T) {
	s, err := newTestStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("failed to migrate test store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// Below the 30s minimum (the broker's fixed 20s abort margin would leave
	// no time for a launch to actually run).
	srv, err := New(ServerConfig{LaunchTimeout: 5 * time.Second}, s)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defaultTimeout := DefaultServerConfig().LaunchTimeout
	if srv.config.LaunchTimeout != defaultTimeout {
		t.Errorf("LaunchTimeout = %v, want %v (should clamp to default)", srv.config.LaunchTimeout, defaultTimeout)
	}
}

func TestNew_PreservesValidLaunchTimeout(t *testing.T) {
	s, err := newTestStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("failed to migrate test store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	srv, err := New(ServerConfig{LaunchTimeout: 10 * time.Minute}, s)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	if srv.config.LaunchTimeout != 10*time.Minute {
		t.Errorf("LaunchTimeout = %v, want %v", srv.config.LaunchTimeout, 10*time.Minute)
	}
}

func TestNew_DefaultsLaunchKeepaliveSecondsWhenZero(t *testing.T) {
	srv, _ := testServer(t)
	defaultKeepalive := DefaultServerConfig().LaunchKeepaliveSeconds
	if srv.config.LaunchKeepaliveSeconds != defaultKeepalive {
		t.Errorf("LaunchKeepaliveSeconds = %v, want %v", srv.config.LaunchKeepaliveSeconds, defaultKeepalive)
	}
}

func TestNew_PreservesValidLaunchKeepaliveSeconds(t *testing.T) {
	s, err := newTestStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("failed to migrate test store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	srv, err := New(ServerConfig{LaunchKeepaliveSeconds: 30}, s)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	if srv.config.LaunchKeepaliveSeconds != 30 {
		t.Errorf("LaunchKeepaliveSeconds = %v, want %v", srv.config.LaunchKeepaliveSeconds, 30)
	}
}
