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

package auth

import (
	"context"
	"net/http"
	"net/url"
	"testing"
	"time"
)

func TestLocalhostAuthServer_StartAndShutdown(t *testing.T) {
	server := NewLocalhostAuthServer()

	ctx := context.Background()
	callbackURL, state, err := server.Start(ctx)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	if callbackURL == "" {
		t.Error("callbackURL is empty")
	}
	if state == "" {
		t.Error("state is empty")
	}

	// Verify callback URL format
	parsed, err := url.Parse(callbackURL)
	if err != nil {
		t.Fatalf("failed to parse callbackURL: %v", err)
	}
	if parsed.Host != "127.0.0.1:18271" {
		t.Errorf("unexpected host: %s", parsed.Host)
	}
	if parsed.Path != "/callback" {
		t.Errorf("unexpected path: %s", parsed.Path)
	}

	// Shutdown
	if err := server.Shutdown(); err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}
}

func TestLocalhostAuthServer_SuccessfulAuth(t *testing.T) {
	server := NewLocalhostAuthServer().WithPort(18272) // Use different port to avoid conflicts

	ctx := context.Background()
	callbackURL, state, err := server.Start(ctx)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer server.Shutdown()

	// Simulate OAuth callback in a goroutine
	expectedCode := "test-auth-code-12345"
	go func() {
		time.Sleep(100 * time.Millisecond) // Small delay to ensure server is ready

		// Make callback request with valid state and code
		callbackWithParams := callbackURL + "?state=" + url.QueryEscape(state) + "&code=" + url.QueryEscape(expectedCode)
		resp, err := http.Get(callbackWithParams)
		if err != nil {
			t.Errorf("callback request failed: %v", err)
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("callback returned status %d, want %d", resp.StatusCode, http.StatusOK)
		}
	}()

	// Wait for code
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	code, err := server.WaitForCode(ctx)
	if err != nil {
		t.Fatalf("WaitForCode failed: %v", err)
	}

	if code != expectedCode {
		t.Errorf("code mismatch: got %q, want %q", code, expectedCode)
	}
}

func TestLocalhostAuthServer_StateMismatch(t *testing.T) {
	server := NewLocalhostAuthServer().WithPort(18273) // Use different port

	ctx := context.Background()
	callbackURL, _, err := server.Start(ctx)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer server.Shutdown()

	// Simulate OAuth callback with wrong state
	go func() {
		time.Sleep(100 * time.Millisecond)

		// Make callback request with wrong state
		callbackWithParams := callbackURL + "?state=wrong-state&code=test-code"
		resp, err := http.Get(callbackWithParams)
		if err != nil {
			t.Errorf("callback request failed: %v", err)
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("callback returned status %d, want %d", resp.StatusCode, http.StatusBadRequest)
		}
	}()

	// Wait for code - should get an error
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	_, err = server.WaitForCode(ctx)
	if err == nil {
		t.Error("WaitForCode should have failed for state mismatch")
	}
}

func TestLocalhostAuthServer_ErrorResponse(t *testing.T) {
	server := NewLocalhostAuthServer().WithPort(18274) // Use different port

	ctx := context.Background()
	callbackURL, state, err := server.Start(ctx)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer server.Shutdown()

	// Simulate OAuth error callback
	go func() {
		time.Sleep(100 * time.Millisecond)

		// Make callback request with error
		callbackWithParams := callbackURL + "?state=" + url.QueryEscape(state) + "&error=access_denied&error_description=User+denied+access"
		resp, err := http.Get(callbackWithParams)
		if err != nil {
			t.Errorf("callback request failed: %v", err)
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("callback returned status %d, want %d", resp.StatusCode, http.StatusBadRequest)
		}
	}()

	// Wait for code - should get an error
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	_, err = server.WaitForCode(ctx)
	if err == nil {
		t.Error("WaitForCode should have failed for error response")
	}
}

func TestLocalhostAuthServer_Timeout(t *testing.T) {
	server := NewLocalhostAuthServer().WithPort(18275) // Use different port

	ctx := context.Background()
	_, _, err := server.Start(ctx)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer server.Shutdown()

	// Wait for code with short timeout - should timeout
	ctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()

	_, err = server.WaitForCode(ctx)
	if err == nil {
		t.Error("WaitForCode should have timed out")
	}
	if err != context.DeadlineExceeded {
		t.Errorf("expected DeadlineExceeded, got %v", err)
	}
}

func TestLocalhostAuthServer_DoubleStart(t *testing.T) {
	server := NewLocalhostAuthServer().WithPort(18276)

	ctx := context.Background()
	_, _, err := server.Start(ctx)
	if err != nil {
		t.Fatalf("First Start failed: %v", err)
	}
	defer server.Shutdown()

	// Second start should fail
	_, _, err = server.Start(ctx)
	if err == nil {
		t.Error("Second Start should have failed")
	}
}

func TestGenerateRandomState(t *testing.T) {
	state1, err := generateRandomState()
	if err != nil {
		t.Fatalf("generateRandomState failed: %v", err)
	}

	state2, err := generateRandomState()
	if err != nil {
		t.Fatalf("generateRandomState failed: %v", err)
	}

	if state1 == "" || state2 == "" {
		t.Error("state should not be empty")
	}

	if state1 == state2 {
		t.Error("states should be unique")
	}
}
