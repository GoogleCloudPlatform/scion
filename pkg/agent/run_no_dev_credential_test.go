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
	"encoding/json"
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
	fakeCreatorToken   = "fake.creator.supplied"
)

// TestStartHubCredentialIsHubIssuedOnly pins that an agent's hub credential
// is the token the hub issued for it: in broker mode StartOptions.HubIssuedToken
// and nothing else. Each case sets made-up values in the process env and the
// dev token file and asserts that the container env and the agent token file
// hold no token other than the hub-issued one, whatever the endpoint
// (co-located bridge endpoint, remote hub, agent config or config env
// override, no trusted endpoint, not broker mode). A made-up creator
// SCION_AUTH_TOKEN in opts.Env or in the agent config env neither counts as
// the hub token nor reaches the container env or the token file. A
// hub-dispatched start without a hub-issued token fails with ErrNoHubToken,
// whose text names no credential value, and starts no container. A token
// file an earlier start left in the agent home (a file or a link) is removed
// on a tokenless start, a link without following it, and replaced by a
// hub-issued token. Each started case asserts the hub and token keys of the
// container env exactly; every case asserts the agent token file.
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
		// wantErr: the start fails with ErrNoHubToken and starts no
		// container (a hub-dispatched start with no hub-issued token).
		wantErr bool
		// seed plants a token file holding the dev credential in the agent
		// home before the start, as an earlier build left it: "file" (plus
		// a temp file) or "symlink" (a link to a file outside the home).
		seed string
		// configEnvToken adds a made-up creator SCION_AUTH_TOKEN to the
		// agent config env (scion-agent.json env).
		configEnvToken bool
		// wantIgnored: the start warns that a SCION_AUTH_TOKEN it did not
		// get from the hub is ignored.
		wantIgnored bool
	}{
		{
			name: "creator SCION_AUTH_TOKEN in opts.Env (request config env), no hub-issued token",
			opts: api.StartOptions{
				BrokerMode: true,
				Env: map[string]string{"SCION_HUB_ENDPOINT": remoteEndpoint, "SCION_HUB_URL": remoteEndpoint,
					"SCION_AUTH_TOKEN": fakeCreatorToken},
				TrustedHubEndpoint: remoteEndpoint,
			},
			wantEndpoint: remoteEndpoint,
			wantErr:      true,
		},
		{
			name: "creator SCION_AUTH_TOKEN in the agent config env, no hub-issued token",
			opts: api.StartOptions{
				BrokerMode:         true,
				Env:                map[string]string{"SCION_HUB_ENDPOINT": remoteEndpoint, "SCION_HUB_URL": remoteEndpoint},
				TrustedHubEndpoint: remoteEndpoint,
			},
			configEnvToken: true,
			wantEndpoint:   remoteEndpoint,
			wantErr:        true,
		},
		{
			name: "hub-issued token with creator tokens in opts.Env and the agent config env: only the hub-issued one",
			opts: api.StartOptions{
				BrokerMode: true,
				Env: map[string]string{"SCION_HUB_ENDPOINT": remoteEndpoint, "SCION_HUB_URL": remoteEndpoint,
					"SCION_AUTH_TOKEN": fakeCreatorToken},
				TrustedHubEndpoint: remoteEndpoint,
				HubIssuedToken:     fakeHubIssuedToken,
			},
			configEnvToken: true,
			wantToken:      fakeHubIssuedToken,
			wantEndpoint:   remoteEndpoint,
			wantIgnored:    true,
		},
		{
			name: "not broker mode: the opts.Env token is the agent's token",
			opts: api.StartOptions{
				Env: map[string]string{"SCION_HUB_ENDPOINT": "http://localhost:9810", "SCION_HUB_URL": "http://localhost:9810",
					"SCION_AUTH_TOKEN": fakeHubIssuedToken},
				TrustedHubEndpoint: "http://localhost:9810",
			},
			wantToken:    fakeHubIssuedToken,
			wantEndpoint: "http://localhost:9810",
		},
		{
			name: "co-located dispatch, bridge-rewritten endpoint, no hub-issued token",
			opts: api.StartOptions{
				BrokerMode:         true,
				Env:                map[string]string{"SCION_HUB_ENDPOINT": bridgeEndpoint, "SCION_HUB_URL": bridgeEndpoint},
				TrustedHubEndpoint: bridgeEndpoint,
			},
			wantEndpoint: bridgeEndpoint,
			wantErr:      true,
		},
		{
			name: "co-located dispatch, no hub-issued token, earlier token file removed",
			opts: api.StartOptions{
				BrokerMode:         true,
				Env:                map[string]string{"SCION_HUB_ENDPOINT": bridgeEndpoint, "SCION_HUB_URL": bridgeEndpoint},
				TrustedHubEndpoint: bridgeEndpoint,
			},
			wantEndpoint: bridgeEndpoint,
			wantErr:      true,
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
			wantErr:      true,
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
			wantErr:      true,
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
			wantErr:      true,
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
			wantErr:      true,
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
			wantErr:      true,
		},
		{
			name: "no trusted endpoint, no hub-issued token",
			opts: api.StartOptions{
				BrokerMode: true,
				Env:        map[string]string{"SCION_HUB_ENDPOINT": bridgeEndpoint, "SCION_HUB_URL": bridgeEndpoint},
			},
			wantEndpoint: bridgeEndpoint,
			wantErr:      true,
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
			wantErr:      true,
		},
		{
			name: "not broker mode: no token, start succeeds",
			opts: api.StartOptions{
				Env:                map[string]string{"SCION_HUB_ENDPOINT": "http://localhost:9810", "SCION_HUB_URL": "http://localhost:9810"},
				TrustedHubEndpoint: "http://localhost:9810",
			},
			wantEndpoint: "http://localhost:9810",
		},
		{
			name: "hub-issued token, co-located endpoint: unchanged",
			opts: api.StartOptions{
				BrokerMode:         true,
				Env:                map[string]string{"SCION_HUB_ENDPOINT": bridgeEndpoint, "SCION_HUB_URL": bridgeEndpoint},
				TrustedHubEndpoint: bridgeEndpoint,
				HubIssuedToken:     fakeHubIssuedToken,
			},
			wantToken:    fakeHubIssuedToken,
			wantEndpoint: bridgeEndpoint,
		},
		{
			name: "hub-issued token over an earlier token file: replaced",
			opts: api.StartOptions{
				BrokerMode:         true,
				Env:                map[string]string{"SCION_HUB_ENDPOINT": bridgeEndpoint, "SCION_HUB_URL": bridgeEndpoint},
				TrustedHubEndpoint: bridgeEndpoint,
				HubIssuedToken:     fakeHubIssuedToken,
			},
			wantToken:    fakeHubIssuedToken,
			wantEndpoint: bridgeEndpoint,
			seed:         "file",
		},
		{
			name: "hub-issued token, remote hub: unchanged",
			opts: api.StartOptions{
				BrokerMode:         true,
				Env:                map[string]string{"SCION_HUB_ENDPOINT": remoteEndpoint, "SCION_HUB_URL": remoteEndpoint},
				TrustedHubEndpoint: remoteEndpoint,
				HubIssuedToken:     fakeHubIssuedToken,
			},
			wantToken:    fakeHubIssuedToken,
			wantEndpoint: remoteEndpoint,
		},
		{
			name:         "hub-issued token, config env override: unchanged",
			configEnvHub: configEndpoint,
			opts: api.StartOptions{
				BrokerMode:         true,
				Env:                map[string]string{"SCION_HUB_ENDPOINT": bridgeEndpoint, "SCION_HUB_URL": bridgeEndpoint},
				TrustedHubEndpoint: bridgeEndpoint,
				HubIssuedToken:     fakeHubIssuedToken,
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
			if tt.configEnvToken {
				addAgentConfigEnv(t, filepath.Join(projectScionDir, "agents", agentName, "scion-agent.json"), "SCION_AUTH_TOKEN", fakeCreatorToken)
			}

			var captured runtime.RunConfig
			ran := false
			mgr := NewManager(&runtime.MockRuntime{
				ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
					return []api.AgentInfo{}, nil
				},
				RunFunc: func(ctx context.Context, cfg runtime.RunConfig) (string, error) {
					ran = true
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
			agentHome := filepath.Dir(agentScionDir)
			if tt.wantErr {
				if !errors.Is(err, ErrNoHubToken) {
					t.Fatalf("Start: err = %v, want ErrNoHubToken", err)
				}
				if info != nil {
					t.Errorf("Start returned agent info %+v with the error", info)
				}
				if ran {
					t.Error("container started for a start with no hub-issued token")
				}
				msg := err.Error()
				for _, tok := range []string{fakeHostDevToken, fakeHubIssuedToken, fakeCreatorToken} {
					if strings.Contains(msg, tok) {
						t.Errorf("start error carries a token value: %q", msg)
					}
				}
				if !strings.Contains(msg, "hub issued no agent token") || !strings.Contains(msg, "agent token service") || !strings.Contains(msg, agentName) {
					t.Errorf("start error %q does not name the agent and the cause", msg)
				}
				assertNoAgentTokenFile(t, agentHome)
				if _, ok := opts.Env["SCION_AUTH_TOKEN"]; ok {
					t.Error("SCION_AUTH_TOKEN left in opts.Env after the refused start")
				}
				if tt.seed == "symlink" {
					if got, err := os.ReadFile(linkTarget); err != nil || string(got) != fakeHostDevToken {
						t.Errorf("token file link target changed (err %v): the link must be removed, not followed", err)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			if !ran {
				t.Fatal("container not started")
			}

			got := map[string]string{}
			for _, e := range captured.Env {
				k, v, _ := strings.Cut(e, "=")
				for _, tok := range []string{fakeHostDevToken, fakeHubIssuedToken, fakeCreatorToken} {
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
			case err == nil && strings.Contains(string(data), fakeCreatorToken):
				t.Fatal("agent token file holds the creator-supplied token")
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

			ignored := false
			for _, w := range append(append([]string(nil), info.Warnings...), info.RelayedWarnings...) {
				if strings.Contains(w, fakeHostDevToken) || strings.Contains(w, fakeHubIssuedToken) || strings.Contains(w, fakeCreatorToken) {
					t.Errorf("start warning carries a token value: %q", w)
				}
				if w == ignoredAuthTokenWarning {
					ignored = true
				}
			}
			if ignored != tt.wantIgnored {
				t.Errorf("ignored-token warning present = %v, want %v; warnings: %v", ignored, tt.wantIgnored, info.Warnings)
			}
		})
	}

	// hubDispatchedWithoutToken directly: either hub key marks a
	// hub-dispatched start; only HubIssuedToken counts as its token (a
	// SCION_AUTH_TOKEN in env does not); a non-broker start is never one.
	t.Run("hubDispatchedWithoutToken", func(t *testing.T) {
		for _, c := range []struct {
			broker   bool
			env      map[string]string
			hubToken string
			want     bool
		}{
			{true, map[string]string{"SCION_HUB_ENDPOINT": remoteEndpoint}, "", true},
			{true, map[string]string{"SCION_HUB_URL": remoteEndpoint}, "", true},
			{true, map[string]string{}, "", false},
			{true, map[string]string{"SCION_HUB_URL": remoteEndpoint}, fakeHubIssuedToken, false},
			{true, map[string]string{"SCION_HUB_URL": remoteEndpoint, "SCION_AUTH_TOKEN": fakeCreatorToken}, "", true},
			{false, map[string]string{"SCION_HUB_ENDPOINT": remoteEndpoint}, "", false},
		} {
			if got := hubDispatchedWithoutToken(api.StartOptions{Name: "a", BrokerMode: c.broker, Env: c.env, HubIssuedToken: c.hubToken}); got != c.want {
				t.Errorf("broker=%v env keys=%v hub token=%v: got %v, want %v", c.broker, slices.Sorted(maps.Keys(c.env)), c.hubToken != "", got, c.want)
			}
		}
	})

	// buildAgentEnv directly: in broker mode a SCION_AUTH_TOKEN from the
	// agent config env or the hub agent default env never reaches the agent
	// env (with the ignored-token warning); outside broker mode it does.
	t.Run("buildAgentEnv drops a creator SCION_AUTH_TOKEN in broker mode", func(t *testing.T) {
		for _, broker := range []bool{true, false} {
			for _, layer := range []string{"config", "default"} {
				cfg := &api.ScionConfig{Env: map[string]string{"ORDINARY": "x"}}
				var defaults map[string]string
				if layer == "config" {
					cfg.Env["SCION_AUTH_TOKEN"] = fakeCreatorToken
				} else {
					defaults = map[string]string{"SCION_AUTH_TOKEN": fakeCreatorToken}
				}
				env, warnings, _, _ := buildAgentEnv(cfg, nil, defaults, broker)
				got := envToMap(env)
				_, present := got["SCION_AUTH_TOKEN"]
				if present == broker {
					t.Errorf("broker=%v layer=%s: SCION_AUTH_TOKEN present = %v", broker, layer, present)
				}
				if got["ORDINARY"] != "x" {
					t.Errorf("broker=%v layer=%s: ORDINARY dropped", broker, layer)
				}
				if broker && layer == "config" && !slices.Contains(warnings, ignoredAuthTokenWarning) {
					t.Errorf("broker config layer: no ignored-token warning in %v", warnings)
				}
				for _, w := range warnings {
					if strings.Contains(w, fakeCreatorToken) {
						t.Errorf("warning carries a token value")
					}
				}
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

// assertNoAgentTokenFile fails t when the agent home holds a token file or
// its temp file (a file or a link).
func assertNoAgentTokenFile(t *testing.T, agentHome string) {
	t.Helper()
	for _, name := range []string{"scion-token", "scion-token.tmp"} {
		if _, err := os.Lstat(filepath.Join(agentHome, ".scion", name)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("agent token file %s: lstat err = %v, want not exist", name, err)
		}
	}
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

// addAgentConfigEnv sets key to value in the env object of the agent config
// file (scion-agent.json) at path.
func addAgentConfigEnv(t *testing.T, path, key, value string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	env, _ := cfg["env"].(map[string]any)
	if env == nil {
		env = map[string]any{}
	}
	env[key] = value
	cfg["env"] = env
	out, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}
}
