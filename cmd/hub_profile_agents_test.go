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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/credentials"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubprofile"
	"github.com/GoogleCloudPlatform/scion/pkg/hubprofile/hubprofiletest"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
)

// Tests for agent commands and web-code under a target-hub profile
// (ptone/scion#4240, X3b). The target is hubprofiletest's fake hub, which
// verifies the issuer and fixture credentials; x3bAgents adds the project
// and agent routes. Every run happens in agent mode with the agent's own
// hub (which fails the test when contacted), its agent token file, OAuth
// credentials for the target, SCION_AUTH_TOKEN, SCION_HUB_TOKEN and
// SCION_DEV_TOKEN present.

const (
	x3bProjectID   = "6b1d6c3e-2f4a-4c8e-9d5b-7a0e1f2c3d4e"
	x3bProjectName = "tp1"
	x3bTerminal    = "x3b terminal output"
	x3bLogs        = "x3b agent log line"
)

// x3bAgents serves the project and agent routes of the fake target for
// one project, x3bProjectName.
type x3bAgents struct {
	mu        sync.Mutex
	agents    map[string]*hubclient.Agent
	creates   []map[string]any
	stops     []string
	deletes   []string
	execs     []string
	logs      []string
	envSubmit int
	// envGather, when set, makes a create answer 202 with these needed
	// keys.
	envGather []string
}

func newX3bAgents() *x3bAgents { return &x3bAgents{agents: map[string]*hubclient.Agent{}} }

func (a *x3bAgents) project() hubclient.Project {
	return hubclient.Project{ID: x3bProjectID, Name: x3bProjectName, Slug: x3bProjectName}
}

func x3bJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (a *x3bAgents) handle(w http.ResponseWriter, r *http.Request, identityID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	p := r.URL.Path
	switch {
	case p == "/api/v1/projects" && r.Method == http.MethodGet:
		var out []hubclient.Project
		if n := r.URL.Query().Get("name"); n == x3bProjectName {
			out = append(out, a.project())
		}
		x3bJSON(w, http.StatusOK, map[string]any{"projects": out})
		return true
	case p == "/api/v1/projects/"+x3bProjectID && r.Method == http.MethodGet:
		x3bJSON(w, http.StatusOK, a.project())
		return true
	case strings.HasPrefix(p, "/api/v1/templates"):
		x3bJSON(w, http.StatusOK, map[string]any{"templates": []any{}})
		return true
	}
	base := "/api/v1/projects/" + x3bProjectID + "/agents"
	if !strings.HasPrefix(p, base) {
		return false
	}
	rest := strings.TrimPrefix(strings.TrimPrefix(p, base), "/")
	switch {
	case rest == "" && r.Method == http.MethodGet:
		list := []hubclient.Agent{}
		names := make([]string, 0, len(a.agents))
		for n := range a.agents {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			if ph := r.URL.Query().Get("phase"); ph == "" || a.agents[n].Phase == ph {
				list = append(list, *a.agents[n])
			}
		}
		x3bJSON(w, http.StatusOK, map[string]any{"agents": list, "serverTime": time.Now()})
	case rest == "" && r.Method == http.MethodPost:
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		a.creates = append(a.creates, body)
		name, _ := body["name"].(string)
		phase := "running"
		if po, _ := body["provisionOnly"].(bool); po {
			phase = "created"
		}
		ag := &hubclient.Agent{ID: "ag-" + name, Slug: name, Name: name, ProjectID: x3bProjectID, Phase: phase, Status: phase, OwnerID: identityID, Created: time.Now()}
		a.agents[name] = ag
		if len(a.envGather) > 0 {
			x3bJSON(w, http.StatusAccepted, map[string]any{"agent": ag, "envGather": map[string]any{"agentId": ag.ID, "required": a.envGather, "needs": a.envGather}})
			return true
		}
		x3bJSON(w, http.StatusCreated, map[string]any{"agent": ag})
	default:
		name, action, _ := strings.Cut(rest, "/")
		ag := a.agents[name]
		for n, cand := range a.agents {
			if ag == nil && cand.ID == name {
				ag, name = cand, n
			}
		}
		if ag == nil {
			x3bJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"code": "not_found", "message": "agent not found"}})
			return true
		}
		switch {
		case action == "" && r.Method == http.MethodGet:
			x3bJSON(w, http.StatusOK, ag)
		case action == "" && r.Method == http.MethodDelete:
			a.deletes = append(a.deletes, name)
			delete(a.agents, name)
			w.WriteHeader(http.StatusNoContent)
		case action == "stop" && r.Method == http.MethodPost:
			a.stops = append(a.stops, name)
			ag.Phase, ag.Status = "stopped", "stopped"
			x3bJSON(w, http.StatusOK, ag)
		case action == "exec" && r.Method == http.MethodPost:
			a.execs = append(a.execs, name)
			x3bJSON(w, http.StatusOK, map[string]any{"output": x3bTerminal + "\n", "exitCode": 0})
		case action == "logs" && r.Method == http.MethodGet:
			a.logs = append(a.logs, name)
			x3bJSON(w, http.StatusOK, map[string]any{"logs": x3bLogs + "\n"})
		case action == "env" && r.Method == http.MethodPost:
			a.envSubmit++
			x3bJSON(w, http.StatusOK, map[string]any{"agent": ag})
		default:
			x3bJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"code": "not_found", "message": "no route"}})
		}
	}
	return true
}

// x3bEnv is an agent-mode runner with a profile t1 for the fake target
// and a profile "own" for the agent's own hub.
type x3bEnv struct {
	home, proj string
	target     *hubprofiletest.Target
	gcp        *hubprofiletest.GCP
	agents     *x3bAgents
	ownHits    *atomic.Int32
	outputs    []string
}

func newX3bEnv(t *testing.T) *x3bEnv {
	t.Helper()
	profileGateEnv(t, "")
	clk := hubprofiletest.NewClock()
	e := &x3bEnv{gcp: hubprofiletest.NewGCP(t, clk), target: hubprofiletest.NewTarget(t, clk, true), agents: newX3bAgents(), ownHits: &atomic.Int32{}}
	e.target.Handle = e.agents.handle

	own := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.ownHits.Add(1)
		http.Error(w, "own hub must not be contacted", http.StatusTeapot)
	}))
	t.Cleanup(own.Close)

	e.home = t.TempDir()
	t.Setenv("HOME", e.home)
	require.NoError(t, os.MkdirAll(filepath.Join(e.home, ".scion"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(e.home, ".scion", "scion-token"), []byte("AMBIENT-SCION-TOKEN-FILE\n"), 0o600))
	origCredPath := credentials.ExportCredentialsPath()
	credentials.SetCredentialsPath(func() string { return filepath.Join(e.home, ".scion", "credentials.json") })
	t.Cleanup(func() { credentials.SetCredentialsPath(origCredPath) })
	require.NoError(t, credentials.Store(e.target.Srv.URL, &credentials.TokenResponse{AccessToken: "AMBIENT-OAUTH-TOKEN", ExpiresIn: 3600e9}))
	for k, v := range e.gcp.Env() {
		t.Setenv(k, v)
	}
	t.Setenv("SCION_DEV_TOKEN", "AMBIENT-DEV-TOKEN")
	t.Setenv("SCION_AUTH_TOKEN", "AMBIENT-AUTH-TOKEN")
	t.Setenv("SCION_HUB_TOKEN", "AMBIENT-HUB-TOKEN")
	t.Setenv("SCION_HUB_ENDPOINT", own.URL)
	t.Setenv("SCION_CLI_MODE", "agent")
	t.Setenv("SCION_NETWORK_MODE", "host")

	// A local project whose settings point at the agent's own hub, with a
	// local template named like the one a test asks for.
	e.proj = filepath.Join(t.TempDir(), "proj")
	require.NoError(t, os.MkdirAll(filepath.Join(e.proj, ".scion", "templates", "local-only"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(e.proj, ".scion", "templates", "local-only", "scion-agent.yaml"), []byte("harness: claude\n"), 0o644))
	settings, _ := json.Marshal(map[string]any{"project_id": "local-project", "hub": map[string]any{"enabled": true, "endpoint": own.URL}})
	require.NoError(t, os.WriteFile(filepath.Join(e.proj, ".scion", "settings.json"), settings, 0o644))
	t.Chdir(e.proj)

	origDeps := hubProfileDeps
	hubProfileDeps = func() hubprofile.Deps { return hubprofile.Deps{IAMCredentialsURL: e.gcp.IAM.URL} }
	t.Cleanup(func() { hubProfileDeps = origDeps })

	require.NoError(t, hubprofile.Save(e.home, &hubprofile.Profile{Name: "t1", Endpoint: e.target.Srv.URL, Transport: hubprofile.TransportIAP,
		IAPAudience: hubprofiletest.IAPAudience, UATServiceAccount: hubprofiletest.UATSA, IssuerServiceAccount: hubprofiletest.IssuerSA,
		IssuerDelegations: map[string]string{"member": hubprofiletest.Delegation}}, false))
	require.NoError(t, hubprofile.Save(e.home, &hubprofile.Profile{Name: "own", Endpoint: own.URL, Transport: hubprofile.TransportNone,
		IssuerServiceAccount: hubprofiletest.IssuerSA, IssuerDelegations: map[string]string{"member": hubprofiletest.Delegation}}, false))
	return e
}

func (e *x3bEnv) run(t *testing.T, args ...string) (profileRun, error) {
	t.Helper()
	r, err := runProfileRoot(t, args...)
	e.outputs = append(e.outputs, r.stdout, r.stderr)
	if err != nil {
		e.outputs = append(e.outputs, err.Error())
	}
	return r, err
}

// issue issues a member identity into the run cache under alias and
// returns its id on the target.
func (e *x3bEnv) issue(t *testing.T, alias string) string {
	t.Helper()
	before := map[string]bool{}
	for _, id := range e.target.LiveIdentityIDs() {
		before[id] = true
	}
	r, err := e.run(t, "--hub-profile", "t1", "hub", "test-identity", "issue", "--as", alias, "--run", "r1")
	require.NoError(t, err, r.stderr)
	for _, id := range e.target.LiveIdentityIDs() {
		if !before[id] {
			return id
		}
	}
	t.Fatal("no identity was issued")
	return ""
}

// snapshot records the local project and HOME (outside the profile
// directory, where the run cache lives).
func (e *x3bEnv) snapshot(t *testing.T) (map[string]string, map[string]string) {
	t.Helper()
	return snapshotTree(t, e.proj, nil), snapshotTree(t, e.home, func(rel string) bool {
		return rel == filepath.Join(".scion", "hub-profiles") || strings.HasPrefix(rel, filepath.Join(".scion", "hub-profiles")+string(filepath.Separator))
	})
}

// assertClean checks that the agent's own hub was never contacted, that no
// ambient credential reached the target, and that no token, code or
// ambient value appeared in any output.
func (e *x3bEnv) assertClean(t *testing.T) {
	t.Helper()
	assert.EqualValues(t, 0, e.ownHits.Load(), "the agent's own hub was contacted")
	for _, rq := range e.target.Recorded() {
		for k, vs := range rq.Header {
			for _, v := range vs {
				assert.NotContains(t, v, "AMBIENT", "%s %s header %s", rq.Method, rq.Path, k)
			}
		}
		assert.Empty(t, rq.Header.Get("X-Scion-Agent-Token"), rq.Path)
	}
	secrets := append(e.gcp.MintedTokens(), e.target.FixtureTokens()...)
	for _, wc := range e.target.WebCodes() {
		secrets = append(secrets, wc.Code)
	}
	all := strings.Join(e.outputs, "\n")
	for _, s := range secrets {
		assert.NotContains(t, all, s)
		if parts := strings.Split(s, "."); len(parts) == 3 {
			assert.NotContains(t, all, parts[1])
		}
	}
	assert.NotContains(t, all, "AMBIENT")
}

// agentRequests returns the recorded requests to the project's agent
// routes.
func (e *x3bEnv) agentRequests() []hubprofiletest.Request {
	var out []hubprofiletest.Request
	for _, rq := range e.target.Recorded() {
		if strings.HasPrefix(rq.Path, "/api/v1/projects") {
			out = append(out, rq)
		}
	}
	return out
}

// B2: create, start, list, look, logs, stop and delete under a profile
// work against the target through the profile client, as the --as
// identity, on the project named with --project, and create no local
// project state.
func TestHubProfileAgents_EndToEnd(t *testing.T) {
	e := newX3bEnv(t)
	ownerID := e.issue(t, "owner")
	projBefore, homeBefore := e.snapshot(t)

	pa := func(args ...string) []string {
		return append(append([]string{"--hub-profile", "t1"}, args...), "--project", x3bProjectName, "--as", "owner", "--run", "r1")
	}

	r, err := e.run(t, pa("create", "a1", "first task")...)
	require.NoError(t, err, r.stdout+r.stderr)
	r, err = e.run(t, pa("start", "a2", "second task", "--no-wait")...)
	require.NoError(t, err, r.stdout+r.stderr)
	assert.Contains(t, r.stdout+r.stderr, "Agent 'a2' started via Hub.")

	e.agents.mu.Lock()
	require.Len(t, e.agents.creates, 2)
	c1, c2 := e.agents.creates[0], e.agents.creates[1]
	e.agents.mu.Unlock()
	assert.Equal(t, "a1", c1["name"])
	assert.Equal(t, x3bProjectID, c1["projectId"])
	assert.Equal(t, true, c1["provisionOnly"], "create provisions only")
	assert.Equal(t, "a2", c2["name"])
	assert.Equal(t, x3bProjectID, c2["projectId"])
	assert.NotEqual(t, true, c2["provisionOnly"], "start starts")
	assert.NotEqual(t, true, c2["gatherEnv"], "no env-gather with a profile")
	assert.Nil(t, c2["workspaceFiles"], "no local workspace files are sent")

	r, err = e.run(t, pa("list")...)
	require.NoError(t, err, r.stdout+r.stderr)
	assert.Contains(t, r.stdout, "a1")
	assert.Contains(t, r.stdout, "a2")

	r, err = e.run(t, pa("look", "a2", "--plain")...)
	require.NoError(t, err, r.stdout+r.stderr)
	assert.Contains(t, r.stdout, x3bTerminal)

	r, err = e.run(t, pa("logs", "a2")...)
	require.NoError(t, err, r.stdout+r.stderr)
	assert.Contains(t, r.stdout, x3bLogs)

	r, err = e.run(t, pa("stop", "a2")...)
	require.NoError(t, err, r.stdout+r.stderr)
	assert.Contains(t, r.stdout+r.stderr, "Agent 'a2' stopped via Hub.")

	r, err = e.run(t, pa("delete", "a1", "a2")...)
	require.NoError(t, err, r.stdout+r.stderr)
	assert.Contains(t, r.stdout+r.stderr, "Agent 'a1' deleted via Hub.")
	assert.Contains(t, r.stdout+r.stderr, "Agent 'a2' deleted via Hub.")
	assert.NotContains(t, r.stdout+r.stderr, "Cleaned up locally", "no local cleanup with a profile")

	e.agents.mu.Lock()
	assert.Equal(t, []string{"a2"}, e.agents.stops)
	assert.Equal(t, []string{"a1", "a2"}, e.agents.deletes)
	assert.Equal(t, []string{"a2"}, e.agents.execs)
	assert.Equal(t, []string{"a2"}, e.agents.logs)
	assert.Empty(t, e.agents.agents)
	e.agents.mu.Unlock()

	// Every project and agent request went to the named project as the
	// --as identity (the profile's credential), never as the issuer
	// delegation.
	reqs := e.agentRequests()
	require.NotEmpty(t, reqs)
	for _, rq := range reqs {
		assert.Equal(t, ownerID, rq.Identity, "%s %s ran as the --as identity", rq.Method, rq.Path)
		assert.Empty(t, rq.Header.Get("X-Scion-Issuer-Delegation"), rq.Path)
		c, err := transportauth.DecodeIDTokenClaims(strings.TrimPrefix(rq.Header.Get("Proxy-Authorization"), "Bearer "))
		require.NoError(t, err, rq.Path)
		assert.Equal(t, hubprofiletest.UATSA, c.Email, "the profile's transport credential on %s", rq.Path)
	}

	e.assertClean(t)
	projAfter, homeAfter := e.snapshot(t)
	assert.Equal(t, projBefore, projAfter, "the local project changed (agent directories, worktrees or sync state)")
	assert.Equal(t, homeBefore, homeAfter, "HOME changed outside the profile directory (caches, credentials)")
}

// B2: the agent commands refuse a profile for the agent's own hub before
// any request; without a profile they keep their agent-mode availability
// (the gate does not ask for a profile).
func TestHubProfileAgents_OwnHubRefusedNoProfileUnchanged(t *testing.T) {
	e := newX3bEnv(t)
	cmds := []string{"create", "start", "list", "stop", "delete", "look", "logs"}
	t.Setenv(hubprofile.EnvHubProfile, "own")
	for _, p := range cmds {
		err := checkHubProfileGate(realCommand(t, p), ModeAgent)
		require.ErrorIs(t, err, hubprofile.ErrOwnHub, p)
	}
	_, err := e.run(t, "--hub-profile", "own", "list", "--project", x3bProjectName, "--as", "owner", "--run", "r1")
	require.ErrorIs(t, err, hubprofile.ErrOwnHub)
	_, err = e.run(t, "--hub-profile", "own", "start", "a1", "--project", x3bProjectName, "--as", "owner", "--run", "r1")
	require.ErrorIs(t, err, hubprofile.ErrOwnHub)

	t.Setenv(hubprofile.EnvHubProfile, "")
	for _, p := range cmds {
		assert.True(t, agentAllowed[p], "%s stays agent-allowed", p)
		assert.NoError(t, checkHubProfileGate(realCommand(t, p), ModeAgent), "%s without a profile is unchanged", p)
	}
	assert.Empty(t, e.target.Recorded(), "no request to the target")
	assert.EqualValues(t, 0, e.ownHits.Load())
}

// B2: under a profile the commands fail closed, before any agent request,
// where they would need local project state, another credential or a
// local environment value.
func TestHubProfileAgents_FailClosed(t *testing.T) {
	e := newX3bEnv(t)
	e.issue(t, "owner")
	projBefore, homeBefore := e.snapshot(t)
	id := []string{"--as", "owner", "--run", "r1"}
	withProject := func(args ...string) []string {
		return append(append(append([]string{"--hub-profile", "t1"}, args...), "--project", x3bProjectName), id...)
	}

	for _, tc := range []struct {
		args []string
		want string
	}{
		{append([]string{"--hub-profile", "t1", "list"}, id...), "name the project on the target hub with --project"},
		{append([]string{"--hub-profile", "t1", "--global", "list"}, id...), "--global does not apply with a hub profile"},
		{withProject("list", "--all"), "--all is not available with a hub profile"},
		{withProject("list", "--descendants=a1"), "--descendants is not available with a hub profile"},
		{withProject("start", "a1", "--attach"), "--attach is not available with a hub profile"},
		{withProject("start", "a1", "--workspace", "sub"), "--workspace is not available with a hub profile"},
		{withProject("create", "a1", "--upload-template"), "--upload-template is not available with a hub profile"},
		{withProject("delete", "a1", "--cleanup-only"), "--cleanup-only is not available with a hub profile"},
		{append([]string{"--hub-profile", "t1", "list", "--project", "no-such-project"}, id...), "project 'no-such-project' not found"},
		{[]string{"--hub-profile", "t1", "list", "--project", x3bProjectName}, "acts as a test identity: name it with --as"},
	} {
		_, err := e.run(t, tc.args...)
		require.Error(t, err, "%v", tc.args)
		assert.Contains(t, err.Error(), tc.want, "%v", tc.args)
	}
	e.agents.mu.Lock()
	assert.Empty(t, e.agents.creates)
	e.agents.mu.Unlock()

	// A template only on the local disk is not read or uploaded: the
	// target has none, so the create fails before any agent request.
	_, err := e.run(t, withProject("create", "a1", "--type", "local-only")...)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `template "local-only" was not found on the target hub of profile "t1"`)
	for _, rq := range e.target.Recorded() {
		assert.False(t, strings.HasPrefix(rq.Path, "/api/v1/templates") && rq.Method != http.MethodGet, "no template upload: %s %s", rq.Method, rq.Path)
	}

	// A target that asks for environment values anyway gets none: the
	// start fails, nothing is submitted and the value never leaves.
	t.Setenv("X3B_NEEDED_KEY", "AMBIENT-ENV-VALUE")
	e.agents.mu.Lock()
	e.agents.envGather = []string{"X3B_NEEDED_KEY"}
	e.agents.mu.Unlock()
	_, err = e.run(t, withProject("start", "a3", "--no-wait")...)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no local environment is sent")
	e.agents.mu.Lock()
	assert.Zero(t, e.agents.envSubmit)
	require.Len(t, e.agents.creates, 1)
	assert.NotEqual(t, true, e.agents.creates[0]["gatherEnv"])
	e.agents.mu.Unlock()

	e.assertClean(t)
	projAfter, homeAfter := e.snapshot(t)
	assert.Equal(t, projBefore, projAfter)
	assert.Equal(t, homeBefore, homeAfter)
}

// B1: web-code in agent mode with a target-hub profile mints the code on
// the target through the issuer delegation and writes it only to --out;
// without a profile and with an own-hub profile it is refused before any
// request and writes nothing; --as does not apply.
func TestHubTestIdentityWebCode_Profile(t *testing.T) {
	e := newX3bEnv(t)
	id := e.issue(t, "owner")
	projBefore, homeBefore := e.snapshot(t)
	dir := resolvedTempDir(t)

	out := filepath.Join(dir, "code.txt")
	r, err := e.run(t, "--hub-profile", "t1", "hub", "test-identity", "web-code", id, "--out", out)
	require.NoError(t, err, r.stdout+r.stderr)
	codes := e.target.WebCodes()
	require.Len(t, codes, 1)
	assert.Equal(t, id, codes[0].IdentityID)
	data, err := os.ReadFile(out)
	require.NoError(t, err)
	assert.Equal(t, codes[0].Code, string(data), "the file holds exactly the code")
	fi, err := os.Lstat(out)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
	assert.Contains(t, r.stdout, "Web-login code for test identity "+id+"\n")
	assert.Contains(t, r.stdout, "  Code file:     "+out+" (mode 0600)\n")

	// The mint carried the issuer delegation of the profile: an ID token
	// for the target's issuer account and issuer audience, the delegation
	// header and the profile's transport credential.
	var mint *hubprofiletest.Request
	recorded := e.target.Recorded()
	for i := range recorded {
		if strings.HasSuffix(recorded[i].Path, "/web-login-code") {
			mint = &recorded[i]
		}
	}
	require.NotNil(t, mint)
	assert.Equal(t, "/api/v1/test-identities/"+id+"/web-login-code", mint.Path)
	assert.Equal(t, hubprofiletest.Delegation, mint.Header.Get("X-Scion-Issuer-Delegation"))
	c, err := transportauth.DecodeIDTokenClaims(strings.TrimPrefix(mint.Header.Get("Authorization"), "Bearer "))
	require.NoError(t, err)
	assert.Equal(t, hubprofiletest.IssuerSA, c.Email)
	assert.True(t, c.HasAudience(hubprofiletest.IssuerAudience))
	pc, err := transportauth.DecodeIDTokenClaims(strings.TrimPrefix(mint.Header.Get("Proxy-Authorization"), "Bearer "))
	require.NoError(t, err)
	assert.Equal(t, hubprofiletest.UATSA, pc.Email)

	before := len(e.target.Recorded())
	for _, tc := range []struct {
		name string
		args []string
		is   error
		want string
	}{
		{"agent mode without a profile", []string{"hub", "test-identity", "web-code", id, "--out", filepath.Join(dir, "no-profile.txt")}, nil, "runs only against a target-hub profile"},
		{"a profile for the agent's own hub", []string{"--hub-profile", "own", "hub", "test-identity", "web-code", id, "--out", filepath.Join(dir, "own.txt")}, hubprofile.ErrOwnHub, ""},
		{"--as with a profile", []string{"--hub-profile", "t1", "hub", "test-identity", "web-code", id, "--out", filepath.Join(dir, "as.txt"), "--as", "owner", "--run", "r1"}, nil, "--as does not apply"},
	} {
		_, err := e.run(t, tc.args...)
		require.Error(t, err, tc.name)
		if tc.is != nil {
			assert.ErrorIs(t, err, tc.is, tc.name)
		} else {
			assert.Contains(t, err.Error(), tc.want, tc.name)
		}
	}
	for _, name := range []string{"no-profile.txt", "own.txt", "as.txt"} {
		_, statErr := os.Lstat(filepath.Join(dir, name))
		assert.True(t, os.IsNotExist(statErr), "%s must not exist", name)
	}
	assert.Len(t, e.target.Recorded(), before, "refusals send no request")
	assert.Len(t, e.target.WebCodes(), 1)

	e.assertClean(t)
	projAfter, homeAfter := e.snapshot(t)
	assert.Equal(t, projBefore, projAfter)
	assert.Equal(t, homeBefore, homeAfter)
}
