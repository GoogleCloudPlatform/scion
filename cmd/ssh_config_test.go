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
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/credentials"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
	"github.com/GoogleCloudPlatform/scion/pkg/tunnelclient"
)

// fakeSSHHub is an httptest hub serving the endpoints ssh and ssh-config
// use: the project and agent lists (cursor paginated), GET project, GET
// agent (with sshHostKey) and the project-scoped GET agent by slug.
type fakeSSHHub struct {
	mu       sync.Mutex
	token    string
	projects []fakeSSHProject
	agents   []*fakeSSHAgent
	pageSize int
	// truncateAgents reports a totalCount larger than the agents served.
	truncateAgents bool
	// agentListStatus, when set, answers every agent list with it.
	agentListStatus int
	// laterAgentPageStatus, when set, answers agent list pages after the
	// first with it.
	laterAgentPageStatus int
	srv                  *httptest.Server
}

type fakeSSHProject struct {
	ID, Slug string
}

type fakeSSHAgent struct {
	ID, Slug, ProjectID, HostKey string
}

func newFakeSSHHub(t *testing.T, token string) *fakeSSHHub {
	t.Helper()
	h := &fakeSSHHub{token: token, pageSize: 2}
	h.srv = httptest.NewServer(http.HandlerFunc(h.serve))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *fakeSSHHub) addProject(id, slug string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.projects = append(h.projects, fakeSSHProject{ID: id, Slug: slug})
}

func (h *fakeSSHHub) addAgent(t *testing.T, id, slug, projectID string) *fakeSSHAgent {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	a := &fakeSSHAgent{ID: id, Slug: slug, ProjectID: projectID, HostKey: testSSHPublicKey(t)}
	h.agents = append(h.agents, a)
	return a
}

func (h *fakeSSHHub) deleteAgent(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i, a := range h.agents {
		if a.ID == id {
			h.agents = append(h.agents[:i], h.agents[i+1:]...)
			return
		}
	}
}

func (h *fakeSSHHub) rotateKey(t *testing.T, id string) string {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, a := range h.agents {
		if a.ID == id {
			a.HostKey = testSSHPublicKey(t)
			return a.HostKey
		}
	}
	t.Fatalf("no agent %s", id)
	return ""
}

func writeFakeHubError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": code, "message": code}})
}

func writeFakeHubJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func fakeAgentJSON(a *fakeSSHAgent) map[string]any {
	return map[string]any{
		"id": a.ID, "slug": a.Slug, "name": a.Slug, "projectId": a.ProjectID,
		"status": "running", "sshHostKey": a.HostKey,
	}
}

// fakePage returns the [start, end) window for a cursor and the next cursor.
func fakePage(cursor string, n, size int) (start, end int, next string) {
	start, _ = strconv.Atoi(cursor)
	if start > n {
		start = n
	}
	end = start + size
	if end >= n {
		return start, n, ""
	}
	return start, end, strconv.Itoa(end)
}

func (h *fakeSSHHub) serve(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+h.token {
		writeFakeHubError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	q := r.URL.Query()
	switch {
	case r.URL.Path == "/api/v1/auth/me":
		writeFakeHubJSON(w, map[string]any{"id": "user-1", "email": "user@example.test"})
	case r.URL.Path == "/api/v1/projects":
		var list []fakeSSHProject
		for _, p := range h.projects {
			if s := q.Get("slug"); s == "" || s == p.Slug {
				list = append(list, p)
			}
		}
		start, end, next := fakePage(q.Get("cursor"), len(list), h.pageSize)
		items := []map[string]any{}
		for _, p := range list[start:end] {
			items = append(items, map[string]any{"id": p.ID, "slug": p.Slug, "name": p.Slug})
		}
		writeFakeHubJSON(w, map[string]any{"projects": items, "nextCursor": next, "totalCount": len(list)})
	case r.URL.Path == "/api/v1/agents":
		if h.agentListStatus != 0 {
			writeFakeHubError(w, h.agentListStatus, "fake")
			return
		}
		if h.laterAgentPageStatus != 0 && q.Get("cursor") != "" {
			writeFakeHubError(w, h.laterAgentPageStatus, "fake")
			return
		}
		start, end, next := fakePage(q.Get("cursor"), len(h.agents), h.pageSize)
		total := len(h.agents)
		if h.truncateAgents {
			next = ""
			end = start + min(h.pageSize, len(h.agents)-start)
			total = len(h.agents) + 5
		}
		items := []map[string]any{}
		for _, a := range h.agents[start:end] {
			items = append(items, fakeAgentJSON(a))
		}
		writeFakeHubJSON(w, map[string]any{"agents": items, "nextCursor": next, "totalCount": total})
	case len(parts) == 4 && parts[2] == "projects":
		for _, p := range h.projects {
			if p.ID == parts[3] {
				writeFakeHubJSON(w, map[string]any{"id": p.ID, "slug": p.Slug, "name": p.Slug})
				return
			}
		}
		writeFakeHubError(w, http.StatusNotFound, "not_found")
	case len(parts) == 4 && parts[2] == "agents":
		for _, a := range h.agents {
			if a.ID == parts[3] {
				writeFakeHubJSON(w, fakeAgentJSON(a))
				return
			}
		}
		writeFakeHubError(w, http.StatusNotFound, "agent_not_found")
	case len(parts) == 6 && parts[2] == "projects" && parts[4] == "agents":
		for _, a := range h.agents {
			if a.ProjectID == parts[3] && (a.Slug == parts[5] || a.ID == parts[5]) {
				writeFakeHubJSON(w, fakeAgentJSON(a))
				return
			}
		}
		writeFakeHubError(w, http.StatusNotFound, "agent_not_found")
	default:
		writeFakeHubError(w, http.StatusNotFound, "not_found")
	}
}

// label is the hub label ssh-config derives for this fake hub when no
// include file exists yet.
func (h *fakeSSHHub) label(t *testing.T) string {
	t.Helper()
	u, err := sshEndpointURL(h.endpoint(t))
	require.NoError(t, err)
	preferred, _ := sshHubLabelCandidates(u)
	return preferred
}

func (h *fakeSSHHub) endpoint(t *testing.T) string {
	t.Helper()
	return tunnelclient.NormalizeEndpoint(h.srv.URL)
}

func mustNormKey(t *testing.T, key string) string {
	t.Helper()
	n, err := normalizeSSHHostKey(key)
	require.NoError(t, err)
	return n
}

// stubSSHTransport turns off transport-layer auth for the test.
func stubSSHTransport(t *testing.T) {
	t.Helper()
	old := sshTransportResolver
	sshTransportResolver = func() (transportauth.TokenSource, transportauth.HeaderMode, error) {
		return nil, 0, nil
	}
	t.Cleanup(func() { sshTransportResolver = old })
}

const sshTestExe = "/opt/scion/bin/scion"

func newSSHTestEnv(t *testing.T, home string, hub *fakeSSHHub) (*sshCommandEnv, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	stubSSHTransport(t)
	var out, errb bytes.Buffer
	env := &sshCommandEnv{
		home:     home,
		exe:      sshTestExe,
		endpoint: hub.srv.URL,
		token:    hub.token,
		stdin:    strings.NewReader(""),
		stdout:   &out,
		stderr:   &errb,
		probe:    func(context.Context, string) error { return nil },
	}
	return env, &out, &errb
}

// twoProjectHub returns a hub with projects a-b (agent c) and a (agent b-c),
// the pair whose names would collide without a separator.
func twoProjectHub(t *testing.T, token string) *fakeSSHHub {
	t.Helper()
	h := newFakeSSHHub(t, token)
	h.addProject("p-ab", "a-b")
	h.addProject("p-a", "a")
	h.addAgent(t, "ag-c", "c", "p-ab")
	h.addAgent(t, "ag-bc", "b-c", "p-a")
	return h
}

// Two hubs and two projects give one include file and one
// known-hosts file per hub, a single Include line, and no name collision.
func TestSSHConfig_TwoHubsTwoProjects(t *testing.T) {
	home := t.TempDir()
	hub1 := twoProjectHub(t, "tok-1")
	hub2 := newFakeSSHHub(t, "tok-2")
	hub2.addProject("p-x", "x")
	hub2.addProject("p-y", "y")
	hub2.addAgent(t, "ag-1", "one", "p-x")
	hub2.addAgent(t, "ag-2", "two", "p-y")
	hub2.addAgent(t, "ag-3", "three", "p-y")

	env1, _, errb1 := newSSHTestEnv(t, home, hub1)
	require.Equal(t, tunnelclient.ExitOK, runSSHConfig(context.Background(), env1, nil, true), errb1.String())
	env2, _, errb2 := newSSHTestEnv(t, home, hub2)
	require.Equal(t, tunnelclient.ExitOK, runSSHConfig(context.Background(), env2, nil, true), errb2.String())

	p := sshPaths{home: home}
	includes, err := filepath.Glob(filepath.Join(p.includeDir(), "scion-*"))
	require.NoError(t, err)
	assert.Len(t, includes, 2)
	knowns, err := filepath.Glob(filepath.Join(p.knownHostsDir(), "known_hosts-*"))
	require.NoError(t, err)
	assert.Len(t, knowns, 2)

	l1 := hub1.label(t)
	inc1 := readTestFile(t, p.includeFile(l1))
	assert.Contains(t, inc1, "Host scion."+l1+".a-b.c\n")
	assert.Contains(t, inc1, "Host scion."+l1+".a.b-c\n")
	kh1 := parseKnownHosts(readTestFile(t, p.knownHostsFile(l1)))
	assert.Len(t, kh1, 2)
	assert.NotEqual(t, kh1["scion."+l1+".a-b.c"], kh1["scion."+l1+".a.b-c"])

	l2 := hub2.label(t)
	require.NotEqual(t, l1, l2)
	inc2 := readTestFile(t, p.includeFile(l2))
	for _, h := range []string{"x.one", "y.two", "y.three"} {
		assert.Contains(t, inc2, "Host scion."+l2+"."+h+"\n")
	}
	assert.NotContains(t, inc2, ".a-b.c")

	cfg := readTestFile(t, p.userConfig())
	assert.Equal(t, 1, strings.Count(cfg, sshIncludeLine))
	assert.True(t, strings.HasPrefix(cfg, sshIncludeLine+"\n"))
}

// The pagination is followed to the end (page size 2, five agents).
func TestSSHConfig_FollowsPagination(t *testing.T) {
	home := t.TempDir()
	hub := newFakeSSHHub(t, "tok")
	hub.addProject("p1", "proj")
	for i := 0; i < 5; i++ {
		hub.addAgent(t, "id"+strconv.Itoa(i), "agent"+strconv.Itoa(i), "p1")
	}
	env, _, errb := newSSHTestEnv(t, home, hub)
	require.Equal(t, tunnelclient.ExitOK, runSSHConfig(context.Background(), env, nil, true), errb.String())
	kh := parseKnownHosts(readTestFile(t, sshPaths{home: home}.knownHostsFile(hub.label(t))))
	assert.Len(t, kh, 5)
}

// Regenerating after an agent is deleted removes its block
// and known-hosts line.
func TestSSHConfig_RegeneratePrunesDeletedAgent(t *testing.T) {
	home := t.TempDir()
	hub := twoProjectHub(t, "tok")
	env, _, errb := newSSHTestEnv(t, home, hub)
	require.Equal(t, tunnelclient.ExitOK, runSSHConfig(context.Background(), env, nil, true), errb.String())

	hub.deleteAgent("ag-bc")
	require.Equal(t, tunnelclient.ExitOK, runSSHConfig(context.Background(), env, nil, true), errb.String())

	l := hub.label(t)
	p := sshPaths{home: home}
	inc := readTestFile(t, p.includeFile(l))
	assert.Contains(t, inc, "Host scion."+l+".a-b.c\n")
	assert.NotContains(t, inc, "scion."+l+".a.b-c")
	kh := parseKnownHosts(readTestFile(t, p.knownHostsFile(l)))
	assert.Contains(t, kh, "scion."+l+".a-b.c")
	assert.NotContains(t, kh, "scion."+l+".a.b-c")
}

// With the hub unreachable, nothing changes and the exit
// code is the "hub unreachable or unavailable" row (6).
func TestSSHConfig_HubUnreachable_LeavesFilesUnchanged_ExitHubUnavailable(t *testing.T) {
	home := t.TempDir()
	hub := twoProjectHub(t, "tok")
	env, _, errb := newSSHTestEnv(t, home, hub)
	require.Equal(t, tunnelclient.ExitOK, runSSHConfig(context.Background(), env, nil, true), errb.String())
	before := sshFilesSnapshot(t, home)

	hub.srv.Close()
	errb.Reset()
	code := runSSHConfig(context.Background(), env, nil, true)
	assert.Equal(t, tunnelclient.ExitHubUnavailable, code)
	assert.Equal(t, before, sshFilesSnapshot(t, home))
	assert.Equal(t, 1, strings.Count(errb.String(), "\n"), errb.String())
	assert.Contains(t, errb.String(), "hub unreachable or unavailable")
}

// A truncated listing (totalCount above what was served,
// no cursor) writes nothing and exits with the "any other error" row (1).
func TestSSHConfig_TruncatedList_LeavesFilesUnchanged_ExitError(t *testing.T) {
	home := t.TempDir()
	hub := twoProjectHub(t, "tok")
	env, _, errb := newSSHTestEnv(t, home, hub)
	require.Equal(t, tunnelclient.ExitOK, runSSHConfig(context.Background(), env, nil, true), errb.String())
	before := sshFilesSnapshot(t, home)

	hub.addAgent(t, "ag-new", "new", "p-a")
	hub.truncateAgents = true
	errb.Reset()
	assert.Equal(t, tunnelclient.ExitError, runSSHConfig(context.Background(), env, nil, true))
	assert.Equal(t, before, sshFilesSnapshot(t, home))
	assert.Contains(t, errb.String(), "incomplete")
}

// A later page failing with 503 writes nothing and exits with the "hub
// unreachable or unavailable" row (6).
func TestSSHConfig_LaterPageUnavailable_LeavesFilesUnchanged_ExitHubUnavailable(t *testing.T) {
	home := t.TempDir()
	hub := twoProjectHub(t, "tok")
	hub.addAgent(t, "ag-3", "third", "p-a")
	env, _, errb := newSSHTestEnv(t, home, hub)
	require.Equal(t, tunnelclient.ExitOK, runSSHConfig(context.Background(), env, nil, true), errb.String())
	before := sshFilesSnapshot(t, home)

	hub.laterAgentPageStatus = http.StatusServiceUnavailable
	assert.Equal(t, tunnelclient.ExitHubUnavailable, runSSHConfig(context.Background(), env, nil, true))
	assert.Equal(t, before, sshFilesSnapshot(t, home))
}

// Credential rows: no credential and a refused credential both exit with
// the credential row (3) and write nothing.
func TestSSHConfig_CredentialMissingOrRefused_ExitCredential(t *testing.T) {
	hub := twoProjectHub(t, "tok")

	t.Run("no credential", func(t *testing.T) {
		home := t.TempDir()
		env, _, errb := newSSHTestEnv(t, home, hub)
		env.token = ""
		assert.Equal(t, tunnelclient.ExitCredential, runSSHConfig(context.Background(), env, nil, true))
		assert.Contains(t, errb.String(), "no usable credential")
		assert.Empty(t, sshFilesSnapshot(t, home)[filepath.Join(home, ".ssh")])
	})
	t.Run("refused credential", func(t *testing.T) {
		home := t.TempDir()
		env, _, errb := newSSHTestEnv(t, home, hub)
		env.token = "wrong"
		assert.Equal(t, tunnelclient.ExitCredential, runSSHConfig(context.Background(), env, nil, true))
		assert.Contains(t, errb.String(), "credential refused")
		assert.Empty(t, sshFilesSnapshot(t, home)[filepath.Join(home, ".ssh")])
	})
}

// A 403 on the listing exits with the "not permitted" row (5).
func TestSSHConfig_ListForbidden_ExitForbidden(t *testing.T) {
	home := t.TempDir()
	hub := twoProjectHub(t, "tok")
	hub.agentListStatus = http.StatusForbidden
	env, _, _ := newSSHTestEnv(t, home, hub)
	assert.Equal(t, tunnelclient.ExitForbidden, runSSHConfig(context.Background(), env, nil, true))
}

// Host-key rotation: a recreated agent gets its known-hosts line rewritten
// and the command says so.
func TestSSHConfig_HostKeyRotation_RewritesLineAndSaysSo(t *testing.T) {
	home := t.TempDir()
	hub := twoProjectHub(t, "tok")
	env, out, errb := newSSHTestEnv(t, home, hub)
	require.Equal(t, tunnelclient.ExitOK, runSSHConfig(context.Background(), env, nil, true), errb.String())
	l := hub.label(t)
	host := "scion." + l + ".a-b.c"
	p := sshPaths{home: home}
	oldKey := parseKnownHosts(readTestFile(t, p.knownHostsFile(l)))[host]

	newKey := hub.rotateKey(t, "ag-c")
	out.Reset()
	require.Equal(t, tunnelclient.ExitOK, runSSHConfig(context.Background(), env, nil, true), errb.String())

	kh := parseKnownHosts(readTestFile(t, p.knownHostsFile(l)))
	assert.Equal(t, mustNormKey(t, newKey), kh[host])
	assert.NotEqual(t, oldKey, kh[host])
	assert.Contains(t, out.String(), "host key for "+host+" changed (agent was recreated); known_hosts line rewritten")
	assert.NotContains(t, out.String(), "scion."+l+".a.b-c changed")
}

// The Host block carries every line the design lists, with the absolute
// scion path and the pinned hub and project.
func TestSSHConfig_HostBlockLines(t *testing.T) {
	home := t.TempDir()
	hub := twoProjectHub(t, "tok")
	env, _, errb := newSSHTestEnv(t, home, hub)
	require.Equal(t, tunnelclient.ExitOK, runSSHConfig(context.Background(), env, nil, true), errb.String())
	l := hub.label(t)
	blocks := parseSSHHostBlocks(readTestFile(t, sshPaths{home: home}.includeFile(l)))
	host := "scion." + l + ".a-b.c"
	want := "Host " + host + "\n" +
		"  ProxyCommand " + sshTestExe + " tunnel --stdio --ssh --hub " + hub.endpoint(t) + " --project a-b c\n" +
		"  HostKeyAlias " + host + "\n" +
		"  UserKnownHostsFile ~/.scion/ssh/known_hosts-" + l + "\n" +
		"  StrictHostKeyChecking yes\n"
	assert.Equal(t, want, blocks[host])
	kh := readTestFile(t, sshPaths{home: home}.knownHostsFile(l))
	assert.Contains(t, kh, host+" ssh-ed25519 ")
	assert.NotContains(t, kh, "host@agent", "the comment is stripped from the pinned key")
}

// The ProxyCommand line runs with a minimal PATH and an
// unrelated working directory. A stand-in scion records its arguments.
func TestSSHConfig_ProxyCommandRunsWithMinimalPATHAndUnrelatedDir(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("needs /bin/sh")
	}
	home := t.TempDir()
	bin := filepath.Join(t.TempDir(), "dir with space%")
	require.NoError(t, os.MkdirAll(bin, 0o755))
	argsOut := filepath.Join(t.TempDir(), "args")
	exe := filepath.Join(bin, "scion")
	script := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done > '" + argsOut + "'\npwd >> '" + argsOut + "'\n"
	require.NoError(t, os.WriteFile(exe, []byte(script), 0o755))

	hub := twoProjectHub(t, "tok")
	env, _, errb := newSSHTestEnv(t, home, hub)
	env.exe = exe
	require.Equal(t, tunnelclient.ExitOK, runSSHConfig(context.Background(), env, nil, true), errb.String())

	l := hub.label(t)
	block := parseSSHHostBlocks(readTestFile(t, sshPaths{home: home}.includeFile(l)))["scion."+l+".a-b.c"]
	var proxy string
	for _, line := range strings.Split(block, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "ProxyCommand "); ok {
			proxy = v
		}
	}
	require.NotEmpty(t, proxy)
	// ssh turns %% back into % before running the command.
	proxy = strings.ReplaceAll(proxy, "%%", "%")

	c := exec.Command("/bin/sh", "-c", "exec "+proxy)
	c.Env = []string{"PATH=/nonexistent"}
	c.Dir = "/"
	out, err := c.CombinedOutput()
	require.NoError(t, err, string(out))
	got := strings.Split(strings.TrimSpace(readTestFile(t, argsOut)), "\n")
	assert.Equal(t, []string{"tunnel", "--stdio", "--ssh", "--hub", hub.endpoint(t), "--project", "a-b", "c", "/"}, got)
}

// File modes: new directories 0700, new files 0600.
func TestSSHConfig_NewFileAndDirModes(t *testing.T) {
	home := t.TempDir()
	hub := twoProjectHub(t, "tok")
	env, _, errb := newSSHTestEnv(t, home, hub)
	require.Equal(t, tunnelclient.ExitOK, runSSHConfig(context.Background(), env, nil, true), errb.String())
	p := sshPaths{home: home}
	l := hub.label(t)
	for _, d := range []string{p.sshDir(), p.includeDir(), filepath.Join(home, ".scion"), p.knownHostsDir()} {
		assert.Equal(t, fs.FileMode(0o700), fileMode(t, d), d)
	}
	for _, f := range []string{p.userConfig(), p.includeFile(l), p.knownHostsFile(l)} {
		assert.Equal(t, fs.FileMode(0o600), fileMode(t, f), f)
	}
}

// An existing ~/.ssh/config keeps its mode and content, with the Include
// line added at the top once.
func TestSSHConfig_ExistingUserConfigKeepsModeAndContent(t *testing.T) {
	home := t.TempDir()
	p := sshPaths{home: home}
	require.NoError(t, os.MkdirAll(p.sshDir(), 0o755))
	original := "Host example\n  User someone\n"
	require.NoError(t, os.WriteFile(p.userConfig(), []byte(original), 0o644))
	require.NoError(t, os.Chmod(p.userConfig(), 0o644))

	hub := twoProjectHub(t, "tok")
	env, _, errb := newSSHTestEnv(t, home, hub)
	require.Equal(t, tunnelclient.ExitOK, runSSHConfig(context.Background(), env, nil, true), errb.String())

	assert.Equal(t, fs.FileMode(0o644), fileMode(t, p.userConfig()))
	assert.Equal(t, fs.FileMode(0o755), fileMode(t, p.sshDir()), "an existing directory keeps its mode")
	assert.Equal(t, sshIncludeLine+"\n\n"+original, readTestFile(t, p.userConfig()))
}

// A symlinked ~/.ssh/config stays a symlink; its target is updated.
func TestSSHConfig_SymlinkedUserConfigKept(t *testing.T) {
	home := t.TempDir()
	p := sshPaths{home: home}
	require.NoError(t, os.MkdirAll(p.sshDir(), 0o700))
	target := filepath.Join(t.TempDir(), "dotfiles-ssh-config")
	require.NoError(t, os.WriteFile(target, []byte("Host x\n"), 0o640))
	require.NoError(t, os.Symlink(target, p.userConfig()))

	hub := twoProjectHub(t, "tok")
	env, _, errb := newSSHTestEnv(t, home, hub)
	require.Equal(t, tunnelclient.ExitOK, runSSHConfig(context.Background(), env, nil, true), errb.String())

	fi, err := os.Lstat(p.userConfig())
	require.NoError(t, err)
	assert.NotZero(t, fi.Mode()&fs.ModeSymlink)
	assert.True(t, strings.HasPrefix(readTestFile(t, target), sshIncludeLine+"\n"))
	assert.Equal(t, fs.FileMode(0o640), fileMode(t, target))
}

// The Include line is added once: rerunning leaves ~/.ssh/config as it is,
// and a line already at the top is recognised. A line inside a Host block
// does not count, because it would apply to that block only.
func TestSSHConfig_IncludeLineIdempotent(t *testing.T) {
	home := t.TempDir()
	hub := twoProjectHub(t, "tok")
	env, _, errb := newSSHTestEnv(t, home, hub)
	p := sshPaths{home: home}
	require.Equal(t, tunnelclient.ExitOK, runSSHConfig(context.Background(), env, nil, true), errb.String())
	first := readTestFile(t, p.userConfig())
	require.Equal(t, tunnelclient.ExitOK, runSSHConfig(context.Background(), env, nil, true), errb.String())
	assert.Equal(t, first, readTestFile(t, p.userConfig()))

	assert.True(t, hasSSHIncludeLine("# comment\n"+sshIncludeLine+"\nHost a\n"))
	assert.True(t, hasSSHIncludeLine("Include   ~/.ssh/config.d/scion-*\n"))
	assert.False(t, hasSSHIncludeLine("Host a\n  "+sshIncludeLine+"\n"))
	assert.False(t, hasSSHIncludeLine(""))
}

// --write warns, naming the file, when another hub's recorded endpoint is
// unreachable, and never deletes or changes that file.
func TestSSHConfig_WarnsAboutUnreachableOtherHub(t *testing.T) {
	home := t.TempDir()
	p := sshPaths{home: home}
	require.NoError(t, ensureSSHDir(home, p.includeDir()))
	other := p.includeFile("oldhub")
	otherContent := renderSSHIncludeFile("https://oldhub.example.test", "oldhub", map[string]string{})
	require.NoError(t, os.WriteFile(other, []byte(otherContent), 0o600))

	hub := twoProjectHub(t, "tok")
	env, _, errb := newSSHTestEnv(t, home, hub)
	var probed []string
	env.probe = func(_ context.Context, endpoint string) error {
		probed = append(probed, endpoint)
		return errors.New("dial failed")
	}
	require.Equal(t, tunnelclient.ExitOK, runSSHConfig(context.Background(), env, nil, true), errb.String())

	assert.Equal(t, []string{"https://oldhub.example.test"}, probed, "only the other hub is probed")
	assert.Contains(t, errb.String(), "warning: "+other+": recorded hub endpoint https://oldhub.example.test cannot be reached")
	assert.Equal(t, otherContent, readTestFile(t, other))
}

// The reachability probe sends no credential, and an unreachable endpoint
// fails rather than hanging.
func TestProbeSSHHubEndpoint_NoCredential(t *testing.T) {
	var gotAuth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = append(gotAuth, r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusUnauthorized)
	}))
	assert.NoError(t, probeSSHHubEndpoint(context.Background(), srv.URL))
	assert.Equal(t, []string{""}, gotAuth)
	srv.Close()
	assert.Error(t, probeSSHHubEndpoint(context.Background(), srv.URL))
}

// A label recorded in an existing header is reused for the same endpoint,
// even when it differs from the label that would be derived now.
func TestSSHConfig_ReusesRecordedLabel(t *testing.T) {
	home := t.TempDir()
	hub := twoProjectHub(t, "tok")
	p := sshPaths{home: home}
	require.NoError(t, ensureSSHDir(home, p.includeDir()))
	require.NoError(t, os.WriteFile(p.includeFile("myhub"),
		[]byte(renderSSHIncludeFile(hub.endpoint(t), "myhub", map[string]string{})), 0o600))

	env, _, errb := newSSHTestEnv(t, home, hub)
	require.Equal(t, tunnelclient.ExitOK, runSSHConfig(context.Background(), env, nil, true), errb.String())

	includes, err := filepath.Glob(filepath.Join(p.includeDir(), "scion-*"))
	require.NoError(t, err)
	assert.Equal(t, []string{p.includeFile("myhub")}, includes)
	assert.Contains(t, readTestFile(t, p.includeFile("myhub")), "Host scion.myhub.a-b.c\n")
	assert.FileExists(t, p.knownHostsFile("myhub"))
}

// With an agent and --write, only that agent's block and pin change, and
// nothing is pruned.
func TestSSHConfig_OneAgentWriteUpdatesOnlyThatAgent(t *testing.T) {
	home := t.TempDir()
	hub := twoProjectHub(t, "tok")
	env, out, errb := newSSHTestEnv(t, home, hub)
	require.Equal(t, tunnelclient.ExitOK, runSSHConfig(context.Background(), env, nil, true), errb.String())
	l := hub.label(t)
	p := sshPaths{home: home}
	before := parseSSHHostBlocks(readTestFile(t, p.includeFile(l)))
	beforeKeys := parseKnownHosts(readTestFile(t, p.knownHostsFile(l)))

	hub.deleteAgent("ag-bc") // would be pruned by a full regeneration
	newKey := hub.rotateKey(t, "ag-c")
	out.Reset()
	require.Equal(t, tunnelclient.ExitOK, runSSHConfig(context.Background(), env, []string{"a-b/c"}, true), errb.String())

	after := parseSSHHostBlocks(readTestFile(t, p.includeFile(l)))
	afterKeys := parseKnownHosts(readTestFile(t, p.knownHostsFile(l)))
	other := "scion." + l + ".a.b-c"
	assert.Equal(t, before[other], after[other], "other agent's block is kept")
	assert.Equal(t, beforeKeys[other], afterKeys[other], "other agent's pin is kept")
	assert.Equal(t, mustNormKey(t, newKey), afterKeys["scion."+l+".a-b.c"])
	assert.Contains(t, out.String(), "scion."+l+".a-b.c changed")
}

// A bare agent name uses the project from local settings, or the only
// match across all projects; several matches are a usage error.
func TestSSHConfig_AgentReferenceResolution(t *testing.T) {
	hub := twoProjectHub(t, "tok")
	hub.addAgent(t, "ag-c2", "c", "p-a") // a second "c", in project a

	t.Run("ambiguous", func(t *testing.T) {
		env, _, errb := newSSHTestEnv(t, t.TempDir(), hub)
		assert.Equal(t, tunnelclient.ExitUsage, runSSHConfig(context.Background(), env, []string{"c"}, false))
		assert.Contains(t, errb.String(), "a-b/c, a/c")
	})
	t.Run("settings project", func(t *testing.T) {
		env, out, errb := newSSHTestEnv(t, t.TempDir(), hub)
		env.settingsProjectID = "p-a"
		require.Equal(t, tunnelclient.ExitOK, runSSHConfig(context.Background(), env, []string{"c"}, false), errb.String())
		assert.Contains(t, out.String(), "Host scion."+hub.label(t)+".a.c\n")
	})
	t.Run("unique", func(t *testing.T) {
		env, out, errb := newSSHTestEnv(t, t.TempDir(), hub)
		require.Equal(t, tunnelclient.ExitOK, runSSHConfig(context.Background(), env, []string{"b-c"}, false), errb.String())
		assert.Contains(t, out.String(), "Host scion."+hub.label(t)+".a.b-c\n")
	})
	t.Run("not found", func(t *testing.T) {
		env, _, _ := newSSHTestEnv(t, t.TempDir(), hub)
		assert.Equal(t, tunnelclient.ExitAgentNotFound, runSSHConfig(context.Background(), env, []string{"a/nope"}, false))
	})
}

// Without --write the files are printed to stdout and nothing is written.
func TestSSHConfig_WithoutWritePrintsOnly(t *testing.T) {
	home := t.TempDir()
	hub := twoProjectHub(t, "tok")
	env, out, errb := newSSHTestEnv(t, home, hub)
	require.Equal(t, tunnelclient.ExitOK, runSSHConfig(context.Background(), env, nil, false), errb.String())
	l := hub.label(t)
	assert.Contains(t, out.String(), "Host scion."+l+".a-b.c\n")
	assert.Contains(t, out.String(), "scion."+l+".a.b-c ssh-ed25519 ")
	assert.Contains(t, out.String(), sshHeaderEndpointKey+hub.endpoint(t))
	assert.Equal(t, map[string]string{home: "dir " + fileMode(t, home).String()}, sshFilesSnapshot(t, home))
}

// The ProxyCommand's --hub and scion ssh keep the endpoint exactly as
// resolved, so a login stored under that spelling (upper case, a default
// port, a trailing slash) is found. The hub label, the header and the
// include file name are those of the canonical spelling.
func TestSSHConfig_HubArgKeepsResolvedSpelling(t *testing.T) {
	stubSSHTransport(t)
	for _, k := range []string{"SCION_HUB_TOKEN", "SCION_DEV_TOKEN", "SCION_DEV_TOKEN_FILE"} {
		t.Setenv(k, "")
	}
	restore := credentials.ExportCredentialsPath()
	credPath := filepath.Join(t.TempDir(), "credentials.json")
	credentials.SetCredentialsPath(func() string { return credPath })
	t.Cleanup(func() { credentials.SetCredentialsPath(restore) })

	home := t.TempDir()
	p := sshPaths{home: home}
	listing := sshAgentListing{complete: true, agents: []sshListedAgent{{"p", "a", testSSHPublicKey(t)}}}
	canonical, err := regenerateSSHHubFiles(p, "https://hub.example.com", sshTestExe, listing)
	require.NoError(t, err)

	for _, spelled := range []string{"https://HUB.example.com:443/", "https://hub.example.com/", "https://Hub.Example.com"} {
		t.Run(spelled, func(t *testing.T) {
			require.NoError(t, credentials.Store(spelled, &credentials.TokenResponse{AccessToken: "stored-" + spelled, ExpiresIn: time.Hour}))
			assert.Equal(t, "stored-"+spelled, tunnelCredential(spelled).Token, "the login is found under the pinned spelling")

			res, err := regenerateSSHHubFiles(p, spelled, sshTestExe, listing)
			require.NoError(t, err)
			assert.Equal(t, canonical.label, res.label)
			assert.Equal(t, canonical.includeFile, res.includeFile)
			assert.Equal(t, canonical.knownHostsFile, res.knownHostsFile)
			block := parseSSHHostBlocks(res.includeContent)["scion.hub.p.a"]
			assert.Contains(t, block, " --hub "+spelled+" --project p a\n")
			endpoint, label := parseSSHIncludeHeader(res.includeContent)
			assert.Equal(t, "https://hub.example.com", endpoint)
			assert.Equal(t, "hub", label)

			hub, err := resolveSSHHub(&sshCommandEnv{home: home, endpoint: spelled, token: "stored-" + spelled})
			require.NoError(t, err)
			assert.Equal(t, spelled, hub.hubArg, "scion ssh pins the resolved spelling")
			assert.Equal(t, "https://hub.example.com", hub.endpoint)
			assert.Equal(t, "hub", hub.label)
		})
	}
	includes, err := filepath.Glob(filepath.Join(p.includeDir(), "scion-*"))
	require.NoError(t, err)
	assert.Equal(t, []string{p.includeFile("hub")}, includes)
}

// The resolved endpoint must be a plain origin even before normalisation:
// user info, a query or a fragment is a usage error (exit 2).
func TestSSHConfig_ResolvedEndpointMustBePlainOrigin(t *testing.T) {
	stubSSHTransport(t)
	for _, bad := range []string{"https://user:pw@hub.example.com", "https://hub.example.com/?x=1", "https://hub.example.com/#frag"} {
		_, err := resolveSSHHub(&sshCommandEnv{home: t.TempDir(), endpoint: bad, token: "tok"})
		assert.Equal(t, tunnelclient.ExitUsage, tunnelclient.CodeOf(err), bad)
		_, err = planSSHHubFiles(sshPaths{home: t.TempDir()}, bad, sshTestExe, sshAgentListing{complete: true})
		assert.Error(t, err, bad)
	}
}
