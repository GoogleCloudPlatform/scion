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

package agent

import (
	"context"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// Made-up values; never real credentials.
const (
	fakeHostDevToken   = "fake-host-dev-token"
	fakeHubIssuedToken = "fake.hub.issued"
)

// TestStartHubCredentialIsHubIssuedOnly pins that an agent's hub credential
// is the token the hub issued for it, or none. Every case plants a dev
// credential in this process's SCION_AUTH_TOKEN, SCION_DEV_TOKEN and
// SCION_DEV_TOKEN_FILE and in ~/.scion/dev-token; none of it may reach the
// container env or the agent token file, whatever the endpoint (co-located
// bridge endpoint, remote hub, agent config or config env override, no
// trusted endpoint, not broker mode). A hub-dispatched agent without a
// hub-issued token gets a start warning, also carried for the hub, that
// names no credential value. A token file an earlier start left in the
// agent home (a file or a link) is removed on a tokenless start, a link
// without following it, and replaced by a hub-issued token. Each case
// asserts the hub and token keys of the container env exactly, and the
// agent token file.
func TestStartHubCredentialIsHubIssuedOnly(t *testing.T) {
	const (
		bridgeEndpoint = "http://host.docker.internal:8080"
		remoteEndpoint = "https://hub.example.com"
		agentEndpoint  = "http://agent-config.example:9810"
		configEndpoint = "http://config-env.example:9810"
	)
	tests := []struct {
		name string
		// agentHub and configEnvHub are the agent config's hub.endpoint and
		// env.SCION_HUB_ENDPOINT ("" = unset).
		agentHub, configEnvHub string
		opts                   api.StartOptions
		// wantToken is the token file content; "" means no token file.
		wantToken    string
		wantEndpoint string
		// wantURL is the container's SCION_HUB_URL; "" means wantEndpoint.
		wantURL string
		// wantWarning: the start reports that the agent has no hub credential.
		wantWarning bool
		// seed plants a token file holding the dev credential in the agent
		// home before the start, as an earlier build left it: "file" (plus
		// a temp file) or "symlink" (a link to a file outside the home).
		seed string
	}{
		{
			name: "co-located dispatch, bridge-rewritten endpoint, no hub-issued token",
			opts: api.StartOptions{
				BrokerMode:         true,
				Env:                map[string]string{"SCION_HUB_ENDPOINT": bridgeEndpoint, "SCION_HUB_URL": bridgeEndpoint},
				TrustedHubEndpoint: bridgeEndpoint,
			},
			wantEndpoint: bridgeEndpoint,
			wantWarning:  true,
		},
		{
			name: "co-located dispatch, no hub-issued token, earlier token file removed",
			opts: api.StartOptions{
				BrokerMode:         true,
				Env:                map[string]string{"SCION_HUB_ENDPOINT": bridgeEndpoint, "SCION_HUB_URL": bridgeEndpoint},
				TrustedHubEndpoint: bridgeEndpoint,
			},
			wantEndpoint: bridgeEndpoint,
			wantWarning:  true,
			seed:         "file",
		},
		{
			name: "co-located dispatch, no hub-issued token, earlier token file link removed, not followed",
			opts: api.StartOptions{
				BrokerMode:         true,
				Env:                map[string]string{"SCION_HUB_ENDPOINT": bridgeEndpoint, "SCION_HUB_URL": bridgeEndpoint},
				TrustedHubEndpoint: bridgeEndpoint,
			},
			wantEndpoint: bridgeEndpoint,
			wantWarning:  true,
			seed:         "symlink",
		},
		{
			name: "remote hub, no hub-issued token",
			opts: api.StartOptions{
				BrokerMode:         true,
				Env:                map[string]string{"SCION_HUB_ENDPOINT": remoteEndpoint, "SCION_HUB_URL": remoteEndpoint},
				TrustedHubEndpoint: remoteEndpoint,
			},
			wantEndpoint: remoteEndpoint,
			wantWarning:  true,
		},
		{
			name:     "agent config hub.endpoint override, no hub-issued token",
			agentHub: agentEndpoint,
			opts: api.StartOptions{
				BrokerMode:         true,
				Env:                map[string]string{"SCION_HUB_ENDPOINT": bridgeEndpoint, "SCION_HUB_URL": bridgeEndpoint},
				TrustedHubEndpoint: bridgeEndpoint,
			},
			wantEndpoint: agentEndpoint,
			wantWarning:  true,
		},
		{
			name:         "config env SCION_HUB_ENDPOINT override, no hub-issued token",
			configEnvHub: configEndpoint,
			opts: api.StartOptions{
				BrokerMode:         true,
				Env:                map[string]string{"SCION_HUB_ENDPOINT": bridgeEndpoint, "SCION_HUB_URL": bridgeEndpoint},
				TrustedHubEndpoint: bridgeEndpoint,
			},
			wantEndpoint: configEndpoint,
			wantWarning:  true,
		},
		{
			name:     "agent config hub.endpoint equal to the trusted endpoint, no hub-issued token",
			agentHub: bridgeEndpoint,
			opts: api.StartOptions{
				BrokerMode:         true,
				Env:                map[string]string{"SCION_HUB_ENDPOINT": bridgeEndpoint, "SCION_HUB_URL": bridgeEndpoint},
				TrustedHubEndpoint: bridgeEndpoint,
			},
			wantEndpoint: bridgeEndpoint,
			wantWarning:  true,
		},
		{
			name: "no trusted endpoint, no hub-issued token",
			opts: api.StartOptions{
				BrokerMode: true,
				Env:        map[string]string{"SCION_HUB_ENDPOINT": bridgeEndpoint, "SCION_HUB_URL": bridgeEndpoint},
			},
			wantEndpoint: bridgeEndpoint,
			wantWarning:  true,
		},
		{
			name: "SCION_HUB_URL differs from SCION_HUB_ENDPOINT, no hub-issued token",
			opts: api.StartOptions{
				BrokerMode:         true,
				Env:                map[string]string{"SCION_HUB_ENDPOINT": bridgeEndpoint, "SCION_HUB_URL": remoteEndpoint},
				TrustedHubEndpoint: bridgeEndpoint,
			},
			wantEndpoint: bridgeEndpoint,
			wantURL:      remoteEndpoint,
			wantWarning:  true,
		},
		{
			name: "not broker mode: no token, no warning",
			opts: api.StartOptions{
				Env:                map[string]string{"SCION_HUB_ENDPOINT": "http://localhost:9810", "SCION_HUB_URL": "http://localhost:9810"},
				TrustedHubEndpoint: "http://localhost:9810",
			},
			wantEndpoint: "http://localhost:9810",
		},
		{
			name: "hub-issued token, co-located endpoint: unchanged",
			opts: api.StartOptions{
				BrokerMode: true,
				Env: map[string]string{"SCION_HUB_ENDPOINT": bridgeEndpoint, "SCION_HUB_URL": bridgeEndpoint,
					"SCION_AUTH_TOKEN": fakeHubIssuedToken},
				TrustedHubEndpoint: bridgeEndpoint,
			},
			wantToken:    fakeHubIssuedToken,
			wantEndpoint: bridgeEndpoint,
		},
		{
			name: "hub-issued token over an earlier token file: replaced",
			opts: api.StartOptions{
				BrokerMode: true,
				Env: map[string]string{"SCION_HUB_ENDPOINT": bridgeEndpoint, "SCION_HUB_URL": bridgeEndpoint,
					"SCION_AUTH_TOKEN": fakeHubIssuedToken},
				TrustedHubEndpoint: bridgeEndpoint,
			},
			wantToken:    fakeHubIssuedToken,
			wantEndpoint: bridgeEndpoint,
			seed:         "file",
		},
		{
			name: "hub-issued token, remote hub: unchanged",
			opts: api.StartOptions{
				BrokerMode: true,
				Env: map[string]string{"SCION_HUB_ENDPOINT": remoteEndpoint, "SCION_HUB_URL": remoteEndpoint,
					"SCION_AUTH_TOKEN": fakeHubIssuedToken},
				TrustedHubEndpoint: remoteEndpoint,
			},
			wantToken:    fakeHubIssuedToken,
			wantEndpoint: remoteEndpoint,
		},
		{
			name:         "hub-issued token, config env override: unchanged",
			configEnvHub: configEndpoint,
			opts: api.StartOptions{
				BrokerMode: true,
				Env: map[string]string{"SCION_HUB_ENDPOINT": bridgeEndpoint, "SCION_HUB_URL": bridgeEndpoint,
					"SCION_AUTH_TOKEN": fakeHubIssuedToken},
				TrustedHubEndpoint: bridgeEndpoint,
			},
			wantToken:    fakeHubIssuedToken,
			wantEndpoint: configEndpoint,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const agentName = "dev-token-agent"
			projectScionDir := setupTrustedHubEndpointTestProject(t, agentName, tt.agentHub, tt.configEnvHub)
			home := os.Getenv("HOME")
			devTokenFile := filepath.Join(home, ".scion", "dev-token")
			if err := os.WriteFile(devTokenFile, []byte(fakeHostDevToken), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("SCION_AUTH_TOKEN", fakeHostDevToken)
			t.Setenv("SCION_DEV_TOKEN", fakeHostDevToken)
			t.Setenv("SCION_DEV_TOKEN_FILE", devTokenFile)
			agentScionDir := filepath.Join(projectScionDir, "agents", agentName, "home", ".scion")
			linkTarget := filepath.Join(home, "outside-token")
			seedAgentTokenFile(t, agentScionDir, linkTarget, tt.seed)

			var captured runtime.RunConfig
			mgr := NewManager(&runtime.MockRuntime{
				ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
					return []api.AgentInfo{}, nil
				},
				RunFunc: func(ctx context.Context, cfg runtime.RunConfig) (string, error) {
					captured = cfg
					return "mock-id", nil
				},
			})
			opts := tt.opts
			opts.Env = maps.Clone(tt.opts.Env)
			opts.Name = agentName
			opts.ProjectPath = projectScionDir
			opts.NoAuth = true
			info, err := mgr.Start(context.Background(), opts)
			if err != nil {
				t.Fatalf("Start: %v", err)
			}

			got := map[string]string{}
			for _, e := range captured.Env {
				k, v, _ := strings.Cut(e, "=")
				for _, tok := range []string{fakeHostDevToken, fakeHubIssuedToken} {
					if strings.Contains(v, tok) {
						t.Errorf("container env %s carries a token value", k)
					}
				}
				if strings.HasPrefix(k, "SCION_HUB_") || strings.HasPrefix(k, "SCION_AUTH_") || strings.HasPrefix(k, "SCION_DEV_") {
					got[k] = v
				}
			}
			wantURL := tt.wantURL
			if wantURL == "" {
				wantURL = tt.wantEndpoint
			}
			want := map[string]string{"SCION_HUB_ENDPOINT": tt.wantEndpoint, "SCION_HUB_URL": wantURL}
			if !maps.Equal(got, want) {
				t.Errorf("container hub/token env = %v, want %v", got, want)
			}
			if v, ok := opts.Env["SCION_AUTH_TOKEN"]; ok {
				t.Errorf("SCION_AUTH_TOKEN left in opts.Env (len %d)", len(v))
			}

			tokenPath := filepath.Join(captured.HomeDir, ".scion", "scion-token")
			data, err := os.ReadFile(tokenPath)
			switch {
			case err == nil && strings.Contains(string(data), fakeHostDevToken):
				t.Fatal("agent token file holds the dev credential")
			case tt.wantToken == "":
				if !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("agent token file: err = %v, want not exist", err)
				}
			case err != nil:
				t.Fatalf("read agent token file: %v", err)
			case string(data) != tt.wantToken:
				t.Errorf("agent token file holds the wrong token (len %d, want len %d)", len(data), len(tt.wantToken))
			}

			if _, err := os.Lstat(tokenPath + ".tmp"); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("agent token temp file: lstat err = %v, want not exist", err)
			}
			if tt.seed == "symlink" {
				if got, err := os.ReadFile(linkTarget); err != nil || string(got) != fakeHostDevToken {
					t.Errorf("token file link target changed (err %v): the link must be removed, not followed", err)
				}
			}
			if tt.seed != "" {
				gotHome, err1 := filepath.EvalSymlinks(captured.HomeDir)
				seeded, err2 := filepath.EvalSymlinks(filepath.Dir(agentScionDir))
				if err1 != nil || err2 != nil || gotHome != seeded {
					t.Fatalf("agent home %q is not the seeded home %q (%v, %v)", captured.HomeDir, filepath.Dir(agentScionDir), err1, err2)
				}
			}

			for _, w := range append(append([]string(nil), info.Warnings...), info.RelayedWarnings...) {
				if strings.Contains(w, fakeHostDevToken) || strings.Contains(w, fakeHubIssuedToken) {
					t.Errorf("start warning carries a token value: %q", w)
				}
			}
			hasNoCredential := func(ws []string) bool {
				for _, w := range ws {
					if strings.Contains(w, "starts without a hub credential") && strings.Contains(w, agentName) {
						return true
					}
				}
				return false
			}
			if got := hasNoCredential(info.Warnings); got != tt.wantWarning {
				t.Errorf("no-hub-credential start warning present = %v, want %v; warnings: %v", got, tt.wantWarning, info.Warnings)
			}
			if got := hasNoCredential(info.RelayedWarnings); got != tt.wantWarning {
				t.Errorf("no-hub-credential warning relayed to the hub = %v, want %v; relayed: %v", got, tt.wantWarning, info.RelayedWarnings)
			}
		})
	}

	// noHubCredentialWarning directly: either hub key marks a hub-dispatched
	// start; a hub-issued token or a non-broker start gets no warning.
	t.Run("noHubCredentialWarning", func(t *testing.T) {
		for _, c := range []struct {
			broker bool
			env    map[string]string
			want   bool
		}{
			{true, map[string]string{"SCION_HUB_ENDPOINT": remoteEndpoint}, true},
			{true, map[string]string{"SCION_HUB_URL": remoteEndpoint}, true},
			{true, map[string]string{}, false},
			{true, map[string]string{"SCION_HUB_URL": remoteEndpoint, "SCION_AUTH_TOKEN": fakeHubIssuedToken}, false},
			{false, map[string]string{"SCION_HUB_ENDPOINT": remoteEndpoint}, false},
		} {
			w := noHubCredentialWarning(api.StartOptions{Name: "a", BrokerMode: c.broker, Env: c.env})
			if (w != "") != c.want {
				t.Errorf("broker=%v env keys=%v: warning %q, want present=%v", c.broker, slices.Sorted(maps.Keys(c.env)), w, c.want)
			}
			if strings.Contains(w, fakeHubIssuedToken) {
				t.Errorf("warning carries a token value")
			}
		}
	})

	// removeAgentTokenFile directly: what the tokenless start relies on.
	t.Run("removeAgentTokenFile", func(t *testing.T) {
		for _, seed := range []string{"", "file", "symlink"} {
			home := t.TempDir()
			scionDir := filepath.Join(home, ".scion")
			outside := filepath.Join(t.TempDir(), "outside-token")
			seedAgentTokenFile(t, scionDir, outside, seed)
			if err := removeAgentTokenFile(home); err != nil {
				t.Fatalf("seed %q: removeAgentTokenFile: %v", seed, err)
			}
			for _, name := range []string{"scion-token", "scion-token.tmp"} {
				if _, err := os.Lstat(filepath.Join(scionDir, name)); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("seed %q: %s still present (lstat err %v)", seed, name, err)
				}
			}
			if seed == "symlink" {
				if got, err := os.ReadFile(outside); err != nil || string(got) != fakeHostDevToken {
					t.Errorf("link target changed (err %v): the link must be removed, not followed", err)
				}
			}
		}
		if err := removeAgentTokenFile(filepath.Join(t.TempDir(), "missing-home")); err != nil {
			t.Errorf("missing agent home: %v, want nil", err)
		}
	})

}

// seedAgentTokenFile plants, in the agent home's .scion directory, a token
// file holding the made-up dev credential as an earlier build left it:
// seed "file" writes scion-token and scion-token.tmp; "symlink" makes
// scion-token a link to linkTarget, a file outside the home that holds it;
// "" plants nothing.
func seedAgentTokenFile(t *testing.T, scionDir, linkTarget, seed string) {
	t.Helper()
	if seed == "" {
		return
	}
	if err := os.MkdirAll(scionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	tokenPath := filepath.Join(scionDir, "scion-token")
	switch seed {
	case "file":
		for _, p := range []string{tokenPath, tokenPath + ".tmp"} {
			if err := os.WriteFile(p, []byte(fakeHostDevToken), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	case "symlink":
		if err := os.WriteFile(linkTarget, []byte(fakeHostDevToken), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(linkTarget, tokenPath); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unknown seed %q", seed)
	}
}
