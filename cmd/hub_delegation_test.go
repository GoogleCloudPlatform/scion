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

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// resetDelegationFlags restores the delegation command flags to their
// defaults for the test and afterwards.
func resetDelegationFlags(t *testing.T) {
	t.Helper()
	reset := func() {
		delegationPermissions, delegationBoundary, delegationBoundaryPrj = nil, "project", ""
		delegationExpires, delegationName, delegationPurpose = "", "", ""
		delegationLabels, delegationMaxCredTTL, delegationCredential = nil, 0, ""
		delegationMine, delegationJSON = false, false
		autoConfirm, nonInteractive = false, false
	}
	reset()
	t.Cleanup(reset)
}

type delegationHubRequest struct {
	method, path string
	body         map[string]interface{}
}

// delegationTestHub answers the agent read and the grant routes and records
// every request; it installs a client for it as the commands' client.
func delegationTestHub(t *testing.T) func() []delegationHubRequest {
	t.Helper()
	var mu sync.Mutex
	var reqs []delegationHubRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		rec := delegationHubRequest{method: r.Method, path: r.URL.Path}
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &rec.body)
		}
		mu.Lock()
		reqs = append(reqs, rec)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		grant := `{"id":"g-1","agentId":"agent-1","issuerUserId":"u-1","boundary":{"kind":"project","projectId":"proj-1"},"permissions":["agent.read"],"ceilingVersion":1,"name":"n","expiresAt":"2026-10-31T00:00:00Z","status":"active","userSuppliedFields":["name"],"activeCredentialCount":0,"activeCredentials":[]}`
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/agents/agent-1":
			_, _ = w.Write([]byte(`{"id":"agent-1","name":"a","slug":"a","projectId":"proj-1"}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/revoke-all"):
			_, _ = w.Write([]byte(`{"revoked":["g-1"],"skipped":[{"grantId":"g-2","agentId":"agent-1","reason":"forbidden"}]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/agents/agent-1/delegations":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(grant))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/delegations"):
			_, _ = w.Write([]byte(`{"delegations":[` + grant + `]}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/delegations/"):
			_, _ = w.Write([]byte(grant))
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"no"}}`))
		}
	}))
	t.Cleanup(srv.Close)
	prev := hubDelegationClientFn
	hubDelegationClientFn = func() (hubclient.Client, error) { return hubclient.New(srv.URL) }
	t.Cleanup(func() { hubDelegationClientFn = prev })
	return func() []delegationHubRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]delegationHubRequest(nil), reqs...)
	}
}

// runDelegationCmd runs fn on a fresh command with captured output.
func runDelegationCmd(t *testing.T, fn func(*cobra.Command, []string) error, args ...string) (string, string, error) {
	t.Helper()
	c := &cobra.Command{}
	c.SetContext(context.Background())
	var out, errOut bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&errOut)
	err := fn(c, args)
	return out.String(), errOut.String(), err
}

// TestHubDelegationCmd_UserModeOnly: scion hub delegation is removed in
// agent mode on the real command tree and kept in human mode (the recorded
// mode decision: user mode only, §18.8).
func TestHubDelegationCmd_UserModeOnly(t *testing.T) {
	for _, path := range []string{"hub.delegation", "hub.delegation.create", "hub.delegation.list", "hub.delegation.get",
		"hub.delegation.revoke", "hub.delegation.revoke-all"} {
		require.NotNil(t, resolveCommandPath(rootCmd, path), "%s must exist in the real command tree", path)
		assert.False(t, agentAllowed[path], "%s must not be in agentAllowed", path)
	}
	hubReal := resolveCommandPath(rootCmd, "hub")
	require.NotNil(t, hubReal)

	root := &cobra.Command{Use: "scion"}
	root.AddCommand(cloneCommandShape(hubReal))
	t.Setenv("SCION_CLI_MODE", "agent")
	applyModeRestrictions(root, resolveMode())
	assert.Nil(t, resolveCommandPath(root, "hub.delegation"), "hub delegation must be removed in agent mode")

	root = &cobra.Command{Use: "scion"}
	root.AddCommand(cloneCommandShape(hubReal))
	t.Setenv("SCION_CLI_MODE", "human")
	applyModeRestrictions(root, resolveMode())
	assert.NotNil(t, resolveCommandPath(root, "hub.delegation.revoke-all"), "human mode keeps hub delegation")
}

// TestHubDelegationCreateHelp_RepeatsAttachConsequences: create --help
// states the attach consequences of §11.9.
func TestHubDelegationCreateHelp_RepeatsAttachConsequences(t *testing.T) {
	for _, phrase := range []string{
		"agent:attach gives the agent shell access",
		"agent token (~/.scion/scion-token",
		"sciontool delegation socket",
		"which this grant does not bound",
		"can outlive revocation and the credential's expiry",
		"cannot retract",
		"not the\ncomputation that follows it",
	} {
		assert.Contains(t, hubDelegationCreateCmd.Long, phrase)
	}
}

// TestHubDelegationCreate_RequestAndAttachNotice: create resolves the agent,
// bounds the grant to the agent's project by default, maps every flag into
// the request, and prints the attach notice only when agent:attach is
// selected.
func TestHubDelegationCreate_RequestAndAttachNotice(t *testing.T) {
	resetDelegationFlags(t)
	requests := delegationTestHub(t)
	delegationPermissions = []string{"agent:read", "agent:attach"}
	delegationName, delegationPurpose = "nightly", "triage"
	delegationExpires, delegationLabels, delegationMaxCredTTL = "7d", []string{"team=infra"}, 30*time.Minute

	out, errOut, err := runDelegationCmd(t, runHubDelegationCreate, "agent-1")
	require.NoError(t, err)
	assert.Contains(t, out, "Created grant g-1 for agent agent-1")
	assert.Contains(t, errOut, "agent:attach gives the agent shell access")

	got := requests()
	require.Len(t, got, 2)
	assert.Equal(t, "/api/v1/agents/agent-1", got[0].path)
	create := got[1]
	assert.Equal(t, http.MethodPost, create.method)
	assert.Equal(t, "/api/v1/agents/agent-1/delegations", create.path)
	assert.Equal(t, map[string]interface{}{"kind": "project", "projectId": "proj-1"}, create.body["boundary"])
	assert.Equal(t, []interface{}{"agent:read", "agent:attach"}, create.body["permissions"])
	assert.Equal(t, "nightly", create.body["name"])
	assert.Equal(t, "triage", create.body["purpose"])
	assert.Equal(t, map[string]interface{}{"team": "infra"}, create.body["labels"])
	assert.Equal(t, float64(1800), create.body["maxCredentialTtlSeconds"])
	exp, err := time.Parse(time.RFC3339, create.body["expiresAt"].(string))
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now().Add(7*24*time.Hour), exp, time.Minute)

	// Without agent:attach there is no notice; --boundary hub bounds to the hub.
	resetDelegationFlags(t)
	delegationPermissions, delegationName, delegationBoundary = []string{"agent:read"}, "n", "hub"
	_, errOut, err = runDelegationCmd(t, runHubDelegationCreate, "agent-1")
	require.NoError(t, err)
	assert.Empty(t, errOut)
	got = requests()
	assert.Equal(t, map[string]interface{}{"kind": "hub"}, got[len(got)-1].body["boundary"])
}

// TestBuildCreateAgentDelegationRequest_Refusals: missing or malformed
// flags are usage errors, refused before any request.
func TestBuildCreateAgentDelegationRequest_Refusals(t *testing.T) {
	cases := map[string]func(){
		"no permission": func() { delegationName = "n" },
		"no name":       func() { delegationPermissions = []string{"agent:read"} },
		"bad expires":   func() { delegationPermissions, delegationName, delegationExpires = []string{"agent:read"}, "n", "soon" },
		"zero days":     func() { delegationPermissions, delegationName, delegationExpires = []string{"agent:read"}, "n", "0d" },
		"bad label": func() {
			delegationPermissions, delegationName, delegationLabels = []string{"agent:read"}, "n", []string{"novalue"}
		},
		"credential ttl": func() {
			delegationPermissions, delegationName, delegationMaxCredTTL = []string{"agent:read"}, "n", 2*time.Hour
		},
		"no project known": func() { delegationPermissions, delegationName = []string{"agent:read"}, "n" },
		"bad boundary": func() {
			delegationPermissions, delegationName, delegationBoundary = []string{"agent:read"}, "n", "world"
		},
		"hub boundary with a project": func() {
			delegationPermissions, delegationName, delegationBoundary, delegationBoundaryPrj = []string{"agent:read"}, "n", "hub", "p-2"
		},
	}
	for name, set := range cases {
		t.Run(name, func(t *testing.T) {
			resetDelegationFlags(t)
			set()
			project := "proj-1"
			if name == "no project known" {
				project = ""
			}
			_, err := buildCreateAgentDelegationRequest(project, time.Now())
			require.Error(t, err)
			var ue *usageError
			assert.ErrorAs(t, err, &ue)
		})
	}
	d, err := parseDelegationExpires("12h")
	require.NoError(t, err)
	assert.Equal(t, 12*time.Hour, d)
	// A day count is accepted up to the largest that fits in a time.Duration.
	d, err = parseDelegationExpires("106751d")
	require.NoError(t, err)
	assert.Equal(t, time.Duration(106751)*24*time.Hour, d)
	for _, s := range []string{"106752d", "100000000d"} {
		_, err = parseDelegationExpires(s)
		require.Error(t, err, s)
	}
}

// TestHubDelegationRevokeAll_ConfirmationAndOutput: revoke-all needs exactly
// one of --mine and <agent>, and a confirmation (the shared --yes or
// --non-interactive, or a Yes on a terminal); without one nothing is sent.
// It reports revoked and skipped grants.
func TestHubDelegationRevokeAll_ConfirmationAndOutput(t *testing.T) {
	resetDelegationFlags(t)
	requests := delegationTestHub(t)
	var prompts []string
	answer := false
	prev := hubDelegationConfirm
	hubDelegationConfirm = func(prompt string) bool {
		prompts = append(prompts, prompt)
		return answer || autoConfirm || nonInteractive
	}
	t.Cleanup(func() { hubDelegationConfirm = prev })

	_, _, err := runDelegationCmd(t, runHubDelegationRevokeAll)
	require.Error(t, err, "neither --mine nor an agent")
	delegationMine = true
	_, _, err = runDelegationCmd(t, runHubDelegationRevokeAll, "agent-1")
	require.Error(t, err, "both --mine and an agent")
	_, _, err = runDelegationCmd(t, runHubDelegationRevokeAll)
	require.ErrorContains(t, err, "cancelled", "not confirmed")
	require.Len(t, prompts, 1)
	assert.Empty(t, requests(), "nothing was sent without confirmation")

	autoConfirm = true
	out, _, err := runDelegationCmd(t, runHubDelegationRevokeAll)
	require.NoError(t, err)
	assert.Contains(t, out, "Revoked 1 grant(s).")
	assert.Contains(t, out, "skipped g-2 (agent agent-1, forbidden)")
	got := requests()
	require.Len(t, got, 1)
	assert.Equal(t, "/api/v1/users/me/delegations/revoke-all", got[0].path)

	autoConfirm, nonInteractive, delegationMine = false, true, false
	_, _, err = runDelegationCmd(t, runHubDelegationRevokeAll, "agent-1")
	require.NoError(t, err, "--non-interactive confirms")
	got = requests()
	assert.Equal(t, "/api/v1/agents/agent-1/delegations/revoke-all", got[len(got)-1].path)
}

// TestHubDelegationConfirm_DefaultRefusesWithoutTerminal: the real
// confirmation refuses without --yes when stdin is not a terminal (as in CI,
// which runs with stdin from /dev/null) and accepts with the shared flag. On
// a terminal the real prompt would wait for an answer, so the test does not
// run there.
func TestHubDelegationConfirm_DefaultRefusesWithoutTerminal(t *testing.T) {
	if util.IsTerminal() {
		t.Skip("stdin is a terminal: the real confirmation would prompt")
	}
	resetDelegationFlags(t)
	assert.False(t, hubDelegationConfirm("revoke?"))
	autoConfirm = true
	assert.True(t, hubDelegationConfirm("revoke?"))
}

// TestHubDelegationFlags_ParseThroughRootTree: the subcommands' flags parse
// through the real command tree, and none shadows a root persistent flag
// with another meaning: --hub and --project stay the root's hub and project
// selection, and revoke-all's --yes/-y and --non-interactive are the shared
// confirmation flags.
func TestHubDelegationFlags_ParseThroughRootTree(t *testing.T) {
	resetDelegationFlags(t)
	prevHub, prevProject := hubEndpoint, projectPath
	var parsed []*cobra.Command
	t.Cleanup(func() {
		// Put the real tree's flags back to their defaults for later tests.
		for _, c := range parsed {
			c.Flags().VisitAll(func(f *pflag.Flag) {
				if f.Changed {
					if sv, ok := f.Value.(pflag.SliceValue); ok {
						_ = sv.Replace(nil)
					} else {
						_ = f.Value.Set(f.DefValue)
					}
					f.Changed = false
				}
			})
		}
		hubEndpoint, projectPath = prevHub, prevProject
	})

	args := []string{"hub", "delegation", "create", "agent-1", "--hub", "https://hub.example", "--project", "proj-x",
		"--boundary", "hub", "--permission", "agent:read", "--name", "n"}
	c, rest, err := rootCmd.Find(args)
	require.NoError(t, err)
	require.Equal(t, "create", c.Name())
	parsed = append(parsed, c)
	require.NoError(t, c.ParseFlags(rest))
	assert.Equal(t, "https://hub.example", hubEndpoint, "--hub is the root hub selection")
	assert.Equal(t, "proj-x", projectPath, "--project is the root project selection")
	assert.Equal(t, "hub", delegationBoundary)
	assert.Equal(t, []string{"agent:read"}, delegationPermissions)
	require.NoError(t, c.ValidateArgs(c.Flags().Args()))

	c, rest, err = rootCmd.Find([]string{"hub", "delegation", "revoke-all", "--mine", "-y"})
	require.NoError(t, err)
	require.Equal(t, "revoke-all", c.Name())
	parsed = append(parsed, c)
	require.NoError(t, c.ParseFlags(rest))
	assert.True(t, autoConfirm, "-y is the shared confirmation flag")
	assert.True(t, delegationMine)

	for _, sub := range hubDelegationCmd.Commands() {
		sub.LocalNonPersistentFlags().VisitAll(func(f *pflag.Flag) {
			root := rootCmd.PersistentFlags().Lookup(f.Name)
			if root == nil {
				return
			}
			assert.True(t, f.Name == "yes" || f.Name == "non-interactive",
				"%s --%s shadows the root flag of the same name", sub.Name(), f.Name)
		})
	}
}

// TestHubDelegationListGetRevoke_Routes: list (per agent and mine), get and
// revoke (grant and one credential) reach their routes.
func TestHubDelegationListGetRevoke_Routes(t *testing.T) {
	resetDelegationFlags(t)
	requests := delegationTestHub(t)

	out, _, err := runDelegationCmd(t, runHubDelegationList)
	require.NoError(t, err)
	assert.Contains(t, out, "g-1")
	_, _, err = runDelegationCmd(t, runHubDelegationList, "agent-1")
	require.NoError(t, err)
	out, _, err = runDelegationCmd(t, runHubDelegationGet, "agent-1", "g-1")
	require.NoError(t, err)
	assert.Contains(t, out, "Grant g-1 for agent agent-1")
	_, _, err = runDelegationCmd(t, runHubDelegationRevoke, "agent-1", "g-1")
	require.NoError(t, err)
	delegationCredential = "c-1"
	_, _, err = runDelegationCmd(t, runHubDelegationRevoke, "agent-1", "g-1")
	require.NoError(t, err)

	var paths []string
	for _, r := range requests() {
		paths = append(paths, r.method+" "+r.path)
	}
	assert.Equal(t, []string{
		"GET /api/v1/users/me/delegations",
		"GET /api/v1/agents/agent-1",
		"GET /api/v1/agents/agent-1/delegations",
		"GET /api/v1/agents/agent-1",
		"GET /api/v1/agents/agent-1/delegations/g-1",
		"GET /api/v1/agents/agent-1",
		"DELETE /api/v1/agents/agent-1/delegations/g-1",
		"GET /api/v1/agents/agent-1",
		"DELETE /api/v1/agents/agent-1/delegations/g-1/credentials/c-1",
	}, paths)
}
