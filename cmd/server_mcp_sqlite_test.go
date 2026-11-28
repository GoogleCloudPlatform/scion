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

package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/knadh/koanf/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func freeMCPTestPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	require.NoError(t, ln.Close())
	return port
}

// TestStartEmbeddedMCPServer_ExperimentWiringAndBindGuard exercises the real
// production startEmbeddedMCPServer entry point against a SQLite-backed
// hub.Server using hubSrv.ExperimentEnabled:
//   - not configured (MCP.Enabled=false) -> no listener bound;
//   - configured + hub.mcp_server off (default) -> /mcp returns 404;
//   - hub.mcp_server turned on live -> MCP initialize returns 200 OK.
func TestStartEmbeddedMCPServer_ExperimentWiringAndBindGuard(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	hubSrv, err := hub.New(hub.ServerConfig{}, st)
	require.NoError(t, err)
	t.Cleanup(func() { _ = hubSrv.Shutdown(context.Background()) })

	ops := hub.NewOperationalSettings(st, koanf.New("."), koanf.New("."))
	_, err = ops.Refresh(ctx)
	require.NoError(t, err)
	hubSrv.SetOperationalSettings(ops)

	// Seed a real user and mint an access token so /api/v1/auth/me succeeds.
	user := &store.User{
		ID:          uuid.NewString(),
		Email:       "mcp-cmd-test@example.com",
		DisplayName: "MCP Cmd Test",
		Role:        store.UserRoleMember,
		Status:      "active",
	}
	require.NoError(t, st.CreateUser(ctx, user))
	userJWT, _, err := hubSrv.GetUserTokenService().MintAccessToken(hub.UserTokenSubject{
		UserID:      user.ID,
		Email:       user.Email,
		DisplayName: user.DisplayName,
		Role:        user.Role,
		ClientType:  hub.ClientTypeCLI,
	})
	require.NoError(t, err)

	t.Run("not_configured_binds_no_listener", func(t *testing.T) {
		port := freeMCPTestPort(t)
		cfg := config.DefaultGlobalConfig()
		cfg.MCP.Enabled = false
		cfg.MCP.Host = "127.0.0.1"
		cfg.MCP.Port = port

		var wg sync.WaitGroup
		errCh := make(chan error, 1)
		shutdownSeq := newServerShutdownSequence(func() {}, nil)

		require.NoError(t, startEmbeddedMCPServer(ctx, shutdownSeq, &cfg, hubSrv, &wg, errCh))
		wg.Wait()
		select {
		case err := <-errCh:
			t.Fatalf("unexpected error from disabled MCP start: %v", err)
		default:
		}

		conn, dialErr := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 100*time.Millisecond)
		if dialErr == nil {
			_ = conn.Close()
			t.Fatalf("expected no listener on port %d when server.mcp.enabled is false", port)
		}
	})

	t.Run("production_experiment_wiring", func(t *testing.T) {
		port := freeMCPTestPort(t)
		cfg := config.DefaultGlobalConfig()
		cfg.MCP.Enabled = true
		cfg.MCP.Host = "127.0.0.1"
		cfg.MCP.Port = port
		cfg.MCP.Path = "/mcp"

		srvCtx, cancel := context.WithCancel(ctx)
		var wg sync.WaitGroup
		errCh := make(chan error, 1)
		shutdownSeq := newServerShutdownSequence(cancel, nil)

		require.NoError(t, startEmbeddedMCPServer(srvCtx, shutdownSeq, &cfg, hubSrv, &wg, errCh))
		t.Cleanup(func() {
			cancel()
			wg.Wait()
		})

		baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)

		// Wait for listener readiness via /healthz.
		deadline := time.Now().Add(3 * time.Second)
		ready := false
		for time.Now().Before(deadline) {
			resp, err := http.Get(baseURL + "/healthz")
			if err == nil {
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					ready = true
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
		require.True(t, ready, "embedded MCP server did not become healthy on %s", baseURL)

		postInit := func() (int, string) {
			t.Helper()
			initBody := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"cmd-wiring-test","version":"0.1"}}}`
			req, err := http.NewRequest(http.MethodPost, baseURL+"/mcp", strings.NewReader(initBody))
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer "+userJWT)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			defer func() { _ = resp.Body.Close() }()
			b, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			return resp.StatusCode, string(b)
		}

		// 1. Default: hub.mcp_server is off -> /mcp returns 404.
		require.False(t, hubSrv.ExperimentEnabled(experiments.MCPServer))
		code, body := postInit()
		assert.Equal(t, http.StatusNotFound, code, "body: %s", body)

		// 2. Turn hub.mcp_server on via operational settings -> /mcp initialize succeeds.
		_, err := st.UpsertHubSetting(ctx, "experiments", json.RawMessage(`{"overrides":{"hub.mcp_server":true}}`), "test", -1, "seeded")
		require.NoError(t, err)
		_, err = ops.Refresh(ctx)
		require.NoError(t, err)
		require.True(t, hubSrv.ExperimentEnabled(experiments.MCPServer))

		code, body = postInit()
		assert.Equal(t, http.StatusOK, code, "body: %s", body)
		assert.Contains(t, body, `"protocolVersion"`)

		// 3. Clean shutdown reports no error.
		cancel()
		wg.Wait()
		select {
		case err := <-errCh:
			t.Fatalf("unexpected error on MCP server shutdown: %v", err)
		default:
		}
	})
}
