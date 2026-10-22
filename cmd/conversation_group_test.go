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
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Thread group CLI tests (ptone/scion#4424, design section 4 and phase 3).
// They run each subcommand against fakeThreadGroupHub, an in-memory hub that
// serves /api/v1/thread-groups with the phase 2 contract, plus the
// conversation routes resolveConversationRef and conversation create use.

const (
	tgProject = "11111111-0000-0000-0000-000000000001"
	tgRelease = "22222222-0000-0000-0000-000000000001"
	tgDesign  = "22222222-0000-0000-0000-000000000002"
	tgConvA   = "33333333-0000-0000-0000-000000000001"
	tgConvB   = "33333333-0000-0000-0000-000000000002"
)

// tgCall is one request the fake hub saw.
type tgCall struct {
	Method, Path, Query string
	Body                map[string]any
}

// fakeThreadGroupHub is an in-memory hub for the thread group commands.
type fakeThreadGroupHub struct {
	mu sync.Mutex
	// enabled false makes the fake an older hub with its
	// thread groups experiment off: every thread group route is 404.
	enabled bool
	// rejectGroupOnCreate makes conversation create answer a
	// threadGroupId with such a hub's 400 while the thread group routes
	// still answer, as during a flag flip between two calls.
	rejectGroupOnCreate bool
	groups              []hubclient.ThreadGroup
	convs               []map[string]any // group conversations for #name resolution
	calls               []tgCall
}

func newFakeThreadGroupHub() *fakeThreadGroupHub {
	return &fakeThreadGroupHub{
		enabled: true,
		groups: []hubclient.ThreadGroup{
			{ID: tgDesign, ProjectID: tgProject, Name: "Design", CreatedBy: "agent-1", CreatedByKind: "agent",
				Threads: []hubclient.ThreadGroupThread{}},
			{ID: tgRelease, ProjectID: tgProject, Name: "Release", CreatedBy: "user-1", CreatedByKind: "user",
				Threads: []hubclient.ThreadGroupThread{
					{ConversationID: tgConvA, ThreadID: "topic-a", Name: "release-notes", AddedBy: "user-1"},
				}},
		},
		convs: []map[string]any{
			{"id": tgConvA, "kind": "group", "surface": "native", "driftState": "active", "displayName": "release-notes"},
			{"id": tgConvB, "kind": "group", "surface": "native", "driftState": "active", "displayName": "standup"},
		},
	}
}

func (h *fakeThreadGroupHub) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (h *fakeThreadGroupHub) writeError(w http.ResponseWriter, status int, code, msg string) {
	h.writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

func (h *fakeThreadGroupHub) find(id string) int {
	for i, g := range h.groups {
		if g.ID == id {
			return i
		}
	}
	return -1
}

func (h *fakeThreadGroupHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var body map[string]any
	if b, _ := io.ReadAll(r.Body); len(b) > 0 {
		_ = json.Unmarshal(b, &body)
	}
	h.calls = append(h.calls, tgCall{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Body: body})

	if r.URL.Path == "/api/v1/conversations" {
		switch r.Method {
		case http.MethodGet:
			h.writeJSON(w, http.StatusOK, map[string]any{"conversations": h.convs})
		case http.MethodPost:
			if body["threadGroupId"] != nil && (!h.enabled || h.rejectGroupOnCreate) {
				h.writeError(w, http.StatusBadRequest, "invalid_request", "thread groups are not enabled")
				return
			}
			h.writeJSON(w, http.StatusCreated, map[string]any{
				"id": "new-conv", "kind": "group", "surface": "native", "driftState": "active",
				"displayName": body["displayName"],
			})
		}
		return
	}

	if !strings.HasPrefix(r.URL.Path, "/api/v1/thread-groups") {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if !h.enabled {
		h.writeError(w, http.StatusNotFound, "not_found", "route not found")
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/thread-groups"), "/")
	if rest == "" {
		switch r.Method {
		case http.MethodGet:
			if r.URL.Query().Get("projectId") == "" {
				h.writeError(w, http.StatusBadRequest, "invalid_request", "projectId is required")
				return
			}
			groups := []hubclient.ThreadGroup{}
			for _, g := range h.groups {
				if g.ProjectID == r.URL.Query().Get("projectId") {
					groups = append(groups, g)
				}
			}
			h.writeJSON(w, http.StatusOK, map[string]any{"threadGroups": groups})
		case http.MethodPost:
			g := hubclient.ThreadGroup{ID: "22222222-0000-0000-0000-0000000000ff", ProjectID: tgProject,
				Name: body["name"].(string), CreatedByKind: "user", Threads: []hubclient.ThreadGroupThread{}}
			h.groups = append(h.groups, g)
			h.writeJSON(w, http.StatusCreated, g)
		}
		return
	}
	parts := strings.Split(rest, "/")
	i := h.find(parts[0])
	if i < 0 {
		h.writeError(w, http.StatusNotFound, "not_found", "Thread group not found")
		return
	}
	switch {
	case len(parts) == 1 && r.Method == http.MethodGet:
		h.writeJSON(w, http.StatusOK, h.groups[i])
	case len(parts) == 1 && r.Method == http.MethodPatch:
		h.groups[i].Name = body["name"].(string)
		h.writeJSON(w, http.StatusOK, h.groups[i])
	case len(parts) == 1 && r.Method == http.MethodDelete:
		h.groups = append(h.groups[:i], h.groups[i+1:]...)
		w.WriteHeader(http.StatusNoContent)
	case len(parts) == 2 && r.Method == http.MethodPost:
		h.groups[i].Threads = append(h.groups[i].Threads,
			hubclient.ThreadGroupThread{ConversationID: body["conversationId"].(string)})
		h.writeJSON(w, http.StatusOK, h.groups[i])
	case len(parts) == 3 && r.Method == http.MethodDelete:
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// mutations returns the non-GET calls to the thread group routes and to
// conversation create.
func (h *fakeThreadGroupHub) mutations() []tgCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []tgCall
	for _, c := range h.calls {
		if c.Method != http.MethodGet {
			out = append(out, c)
		}
	}
	return out
}

// threadGroupTestState snapshots the package-level state the thread group
// commands read.
type threadGroupTestState struct {
	home                                                 string
	projectPath, convProject, convGroupProject           string
	convCreateGroup, outputFormat                        string
	convGroupListJSON, convGroupGetJSON, convGroupCreate bool
	convCreateJSON, autoConfirm                          bool
}

func saveThreadGroupTestState() threadGroupTestState {
	return threadGroupTestState{
		home: os.Getenv("HOME"), projectPath: projectPath, convProject: convProject,
		convGroupProject: convGroupProject, convCreateGroup: convCreateGroup, outputFormat: outputFormat,
		convGroupListJSON: convGroupListJSON, convGroupGetJSON: convGroupGetJSON,
		convGroupCreate: convGroupCreateJSON, convCreateJSON: convCreateJSON, autoConfirm: autoConfirm,
	}
}

func (s threadGroupTestState) restore() {
	_ = os.Setenv("HOME", s.home)
	projectPath, convProject, convGroupProject = s.projectPath, s.convProject, s.convGroupProject
	convCreateGroup, outputFormat = s.convCreateGroup, s.outputFormat
	convGroupListJSON, convGroupGetJSON, convGroupCreateJSON = s.convGroupListJSON, s.convGroupGetJSON, s.convGroupCreate
	convCreateJSON, autoConfirm = s.convCreateJSON, s.autoConfirm
}

// setupThreadGroupTest starts a fake hub and points the CLI at it, with
// hubProjectID as the hub-linked project ("" for none). It resets every
// thread group flag.
func setupThreadGroupTest(t *testing.T, hubProjectID string) *fakeThreadGroupHub {
	t.Helper()
	orig := saveThreadGroupTestState()
	t.Cleanup(orig.restore)
	hub := newFakeThreadGroupHub()
	srv := httptest.NewServer(hub)
	t.Cleanup(srv.Close)

	isolateHubEnvForTest(t, srv.URL, "")
	tmpHome := t.TempDir()
	_ = os.Setenv("HOME", tmpHome)
	projectPath = setupConversationCreateProject(t, tmpHome, srv.URL, hubProjectID)
	convProject, convGroupProject, convCreateGroup, outputFormat = "", "", "", ""
	convGroupListJSON, convGroupGetJSON, convGroupCreateJSON, convCreateJSON, autoConfirm = false, false, false, false, false
	return hub
}

// runTG runs fn as a command and returns its stdout and error.
func runTG(t *testing.T, fn func(*cobra.Command, []string) error, args ...string) (string, error) {
	t.Helper()
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	var err error
	out := captureStdout(t, func() { err = fn(cmd, args) })
	return out, err
}

func TestConversationGroupCommandTree(t *testing.T) {
	require.Same(t, conversationGroupCmd, resolveCommandPath(rootCmd, "conversation.group"))
	var names []string
	for _, c := range conversationGroupCmd.Commands() {
		names = append(names, c.Name())
	}
	sort.Strings(names)
	assert.Equal(t, []string{"add", "create", "delete", "get", "list", "remove", "rename"}, names)

	flagsOf := map[*cobra.Command][]string{
		conversationGroupListCmd:   {"json"},
		conversationGroupGetCmd:    {"json"},
		conversationGroupCreateCmd: {"json"},
		conversationGroupDeleteCmd: {"yes"},
		conversationCreateCmd:      {"group"},
	}
	for c, flags := range flagsOf {
		for _, f := range flags {
			assert.NotNil(t, c.Flags().Lookup(f), "%s should have --%s", c.CommandPath(), f)
		}
	}

	// Every group subcommand has its own local --project <id> bound to
	// convGroupProject. LocalFlags leaves out the inherited root -g/--project,
	// and the root flag has the shorthand g, so this cannot pass on the
	// root flag.
	t.Cleanup(func() { convGroupProject = "" })
	for _, c := range []*cobra.Command{conversationGroupListCmd, conversationGroupGetCmd, conversationGroupCreateCmd,
		conversationGroupRenameCmd, conversationGroupDeleteCmd, conversationGroupAddCmd, conversationGroupRemoveCmd} {
		f := c.LocalFlags().Lookup("project")
		require.NotNil(t, f, "%s needs a local --project", c.Name())
		assert.Empty(t, f.Shorthand, "%s --project must be the local ID flag, not the root -g", c.Name())
		require.NoError(t, c.Flags().Set("project", "pid-"+c.Name()))
		assert.Equal(t, "pid-"+c.Name(), convGroupProject, "%s --project must bind convGroupProject", c.Name())
		require.NoError(t, f.Value.Set(""))
		f.Changed = false
	}
	for _, c := range []*cobra.Command{conversationGroupRenameCmd, conversationGroupAddCmd, conversationGroupRemoveCmd} {
		assert.Error(t, c.Args(c, []string{"one"}), "%s needs two args", c.Name())
		assert.NoError(t, c.Args(c, []string{"one", "two"}))
	}
	for _, c := range []*cobra.Command{conversationGroupGetCmd, conversationGroupCreateCmd, conversationGroupDeleteCmd} {
		assert.Error(t, c.Args(c, nil), "%s needs one arg", c.Name())
	}
	assert.Error(t, conversationGroupListCmd.Args(conversationGroupListCmd, []string{"x"}))
}

func TestConversationGroupList_TextAndProject(t *testing.T) {
	hub := setupThreadGroupTest(t, tgProject)
	out, err := runTG(t, runConversationGroupList)
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	require.Len(t, lines, 3)
	assert.Equal(t, []string{"NAME", "THREADS", "ID"}, strings.Fields(lines[0]))
	assert.Equal(t, []string{"Design", "0", tgDesign}, strings.Fields(lines[1]))
	assert.Equal(t, []string{"Release", "1", tgRelease}, strings.Fields(lines[2]))
	assert.Equal(t, "projectId="+tgProject, hub.calls[0].Query, "--project defaults to the hub-linked project")

	// An explicit --project wins.
	convGroupProject = "explicit-project"
	_, err = runTG(t, runConversationGroupList)
	require.NoError(t, err)
	assert.Equal(t, "projectId=explicit-project", hub.calls[1].Query)
}

func TestConversationGroupList_Empty(t *testing.T) {
	hub := setupThreadGroupTest(t, tgProject)
	hub.groups = nil
	out, err := runTG(t, runConversationGroupList)
	require.NoError(t, err)
	assert.Equal(t, "No thread groups found.\n", out)

	convGroupListJSON = true
	out, err = runTG(t, runConversationGroupList)
	require.NoError(t, err)
	assert.JSONEq(t, `{"threadGroups":[]}`, out, "an empty list is [], never null")
}

// The --json shapes are stable: list is the hub's {"threadGroups":[...]}
// body, get and create are one ThreadGroup, each with exactly these keys.
func TestConversationGroup_JSONShapes(t *testing.T) {
	groupKeys := []string{"createdAt", "createdBy", "createdByKind", "id", "name", "projectId", "threads", "updatedAt"}
	threadKeys := []string{"addedAt", "addedBy", "conversationId", "name", "threadId"}
	keysOf := func(m map[string]any) []string {
		var ks []string
		for k := range m {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		return ks
	}

	setupThreadGroupTest(t, tgProject)
	convGroupListJSON = true
	out, err := runTG(t, runConversationGroupList)
	require.NoError(t, err)
	var list map[string][]map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &list))
	assert.Equal(t, []string{"threadGroups"}, func() []string {
		var ks []string
		for k := range list {
			ks = append(ks, k)
		}
		return ks
	}())
	require.Len(t, list["threadGroups"], 2)
	assert.Equal(t, groupKeys, keysOf(list["threadGroups"][1]))
	thread := list["threadGroups"][1]["threads"].([]any)[0].(map[string]any)
	assert.Equal(t, threadKeys, keysOf(thread))
	assert.Equal(t, "topic-a", thread["threadId"])

	outputFormat = ""
	convGroupGetJSON = true
	out, err = runTG(t, runConversationGroupGet, "release")
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	assert.Equal(t, groupKeys, keysOf(got))
	assert.Equal(t, tgRelease, got["id"])

	outputFormat = ""
	convGroupCreateJSON = true
	out, err = runTG(t, runConversationGroupCreate, "Ops")
	require.NoError(t, err)
	got = nil
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	assert.Equal(t, groupKeys, keysOf(got))
	assert.Equal(t, []any{}, got["threads"], "a new group's threads is [], never null")
}

func TestConversationGroupGet_ResolvesNameAndID(t *testing.T) {
	setupThreadGroupTest(t, tgProject)
	for _, ref := range []string{"Release", "release", "  RELEASE ", tgRelease} {
		out, err := runTG(t, runConversationGroupGet, ref)
		require.NoError(t, err, ref)
		assert.Contains(t, out, "ID:          "+tgRelease, ref)
		assert.Contains(t, out, "Name:        Release", ref)
		assert.Contains(t, out, "Threads:", ref)
		assert.Contains(t, out, "release-notes", ref)
		assert.Contains(t, out, tgConvA, ref)
	}

	out, err := runTG(t, runConversationGroupGet, "Design")
	require.NoError(t, err)
	assert.Contains(t, out, "No threads in this group.")
	assert.Contains(t, out, "Created by:  agent-1 (agent)")
}

func TestConversationGroup_UnknownName(t *testing.T) {
	hub := setupThreadGroupTest(t, tgProject)
	_, err := runTG(t, runConversationGroupGet, "nope")
	require.Error(t, err)
	assert.Equal(t, `no thread group named "nope" in project `+tgProject, err.Error())
	assert.Empty(t, hub.mutations())

	_, err = runTG(t, runConversationGroupRename, "nope", "x")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `no thread group named "nope"`)
	assert.Empty(t, hub.mutations(), "an unknown name must not reach a mutating route")
}

func TestConversationGroup_Ambiguous(t *testing.T) {
	t.Run("two groups with the same name key", func(t *testing.T) {
		hub := setupThreadGroupTest(t, tgProject)
		hub.groups = append(hub.groups, hubclient.ThreadGroup{
			ID: "22222222-0000-0000-0000-000000000003", ProjectID: tgProject, Name: "RELEASE"})
		_, err := runTG(t, runConversationGroupDelete, "release")
		require.Error(t, err)
		assert.Contains(t, err.Error(), `thread group "release" is ambiguous`)
		assert.Contains(t, err.Error(), tgRelease)
		assert.Contains(t, err.Error(), "22222222-0000-0000-0000-000000000003")
		assert.Contains(t, err.Error(), "use the group ID")
		assert.Empty(t, hub.mutations())
	})
	t.Run("an ID that is also another group's name", func(t *testing.T) {
		hub := setupThreadGroupTest(t, tgProject)
		hub.groups = append(hub.groups, hubclient.ThreadGroup{
			ID: "22222222-0000-0000-0000-000000000004", ProjectID: tgProject, Name: tgRelease})
		_, err := runTG(t, runConversationGroupGet, tgRelease)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "is ambiguous")
	})
}

// A UUID of a group outside the listed project is fetched by ID, so the
// hub decides; with no project resolved the list is skipped.
func TestConversationGroupGet_IDOutsideProject(t *testing.T) {
	hub := setupThreadGroupTest(t, tgProject)
	other := hubclient.ThreadGroup{ID: "44444444-0000-0000-0000-000000000001", ProjectID: "other", Name: "Elsewhere",
		Threads: []hubclient.ThreadGroupThread{}}
	hub.groups = append(hub.groups, other)
	out, err := runTG(t, runConversationGroupGet, other.ID)
	require.NoError(t, err)
	assert.Contains(t, out, "Elsewhere")
	require.Len(t, hub.calls, 2)
	assert.Equal(t, "projectId="+tgProject, hub.calls[0].Query, "the project's groups are listed first")
	assert.Equal(t, "/api/v1/thread-groups/"+other.ID, hub.calls[1].Path, "then the ID is fetched directly")

	hub2 := setupThreadGroupTest(t, "")
	out, err = runTG(t, runConversationGroupGet, tgDesign)
	require.NoError(t, err)
	assert.Contains(t, out, "Name:        Design")
	require.Len(t, hub2.calls, 1, "a UUID with no project skips the list")
	assert.Equal(t, "/api/v1/thread-groups/"+tgDesign, hub2.calls[0].Path)
}

func TestConversationGroupCreate_Request(t *testing.T) {
	hub := setupThreadGroupTest(t, tgProject)
	out, err := runTG(t, runConversationGroupCreate, "Ops")
	require.NoError(t, err)
	assert.Contains(t, out, "Thread group created: ")
	assert.Contains(t, out, "Name:    Ops")
	m := hub.mutations()
	require.Len(t, m, 1)
	assert.Equal(t, map[string]any{"projectId": tgProject, "name": "Ops"}, m[0].Body)
}

// With no project resolved the request omits projectId, so the hub
// defaults it for an agent; a user's "projectId is required" gets the local
// resolve hint.
func TestConversationGroupCreate_NoProject(t *testing.T) {
	hub := setupThreadGroupTest(t, "")
	_, err := runTG(t, runConversationGroupList)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "projectId is required")
	assert.Contains(t, err.Error(), "Use --project flag")

	_, err = runTG(t, runConversationGroupCreate, "Ops")
	require.NoError(t, err)
	m := hub.mutations()
	require.Len(t, m, 1)
	assert.Equal(t, map[string]any{"name": "Ops"}, m[0].Body, "an unresolved project is omitted, not fabricated")
}

// The "projectId is required" answer to the name-resolving list call gets
// the local resolve hint too, for the item subcommands and for
// conversation create --group <name>, and nothing is mutated.
func TestConversationGroup_ResolveProjectHint(t *testing.T) {
	for _, tc := range []struct {
		name string
		fn   func(*cobra.Command, []string) error
		args []string
	}{
		{"get", runConversationGroupGet, []string{"Release"}},
		{"add", runConversationGroupAdd, []string{"Release", "#standup"}},
		{"conversation create --group", runConversationCreate, []string{"new-thread"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hub := setupThreadGroupTest(t, "")
			convCreateGroup = "Release"
			_, err := runTG(t, tc.fn, tc.args...)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "projectId is required")
			assert.Contains(t, err.Error(), "Use --project flag")
			assert.Empty(t, hub.mutations())
		})
	}
}

// --project <id> on an item subcommand selects the project its group name
// resolves in, the same as on list and create.
func TestConversationGroup_ProjectFlagOnItemCommands(t *testing.T) {
	const otherProject = "11111111-0000-0000-0000-000000000009"
	const otherGroup = "22222222-0000-0000-0000-000000000009"
	hub := setupThreadGroupTest(t, tgProject)
	hub.groups = append(hub.groups, hubclient.ThreadGroup{ID: otherGroup, ProjectID: otherProject, Name: "Release",
		Threads: []hubclient.ThreadGroupThread{}})

	convGroupProject = otherProject
	out, err := runTG(t, runConversationGroupGet, "release")
	require.NoError(t, err)
	assert.Contains(t, out, "ID:          "+otherGroup, "the name resolves in the --project project, not the hub-linked one")
	assert.Equal(t, "projectId="+otherProject, hub.calls[0].Query)

	_, err = runTG(t, runConversationGroupRename, "Release", "Shipped")
	require.NoError(t, err)
	m := hub.mutations()
	require.Len(t, m, 1)
	assert.Equal(t, "/api/v1/thread-groups/"+otherGroup, m[0].Path)

	convGroupProject = "11111111-0000-0000-0000-00000000000a"
	_, err = runTG(t, runConversationGroupGet, "Release")
	require.Error(t, err)
	assert.Equal(t, `no thread group named "Release" in project 11111111-0000-0000-0000-00000000000a`, err.Error())
}

func TestConversationGroupRename(t *testing.T) {
	hub := setupThreadGroupTest(t, tgProject)
	out, err := runTG(t, runConversationGroupRename, "release", "Release 2.0")
	require.NoError(t, err)
	assert.Equal(t, `Renamed thread group `+tgRelease+` from "Release" to "Release 2.0"`+"\n", out)
	m := hub.mutations()
	require.Len(t, m, 1)
	assert.Equal(t, http.MethodPatch, m[0].Method)
	assert.Equal(t, "/api/v1/thread-groups/"+tgRelease, m[0].Path)
	assert.Equal(t, map[string]any{"name": "Release 2.0"}, m[0].Body)
}

func TestConversationGroupDelete(t *testing.T) {
	t.Run("not confirmed without a terminal or --yes", func(t *testing.T) {
		hub := setupThreadGroupTest(t, tgProject)
		_, err := runTG(t, runConversationGroupDelete, "Release")
		require.Error(t, err)
		assert.Equal(t, "deletion cancelled", err.Error())
		assert.Empty(t, hub.mutations())
	})
	t.Run("--yes deletes", func(t *testing.T) {
		hub := setupThreadGroupTest(t, tgProject)
		autoConfirm = true
		out, err := runTG(t, runConversationGroupDelete, "Release")
		require.NoError(t, err)
		assert.Equal(t, `Deleted thread group "Release" (`+tgRelease+`)`+"\n", out)
		m := hub.mutations()
		require.Len(t, m, 1)
		assert.Equal(t, http.MethodDelete, m[0].Method)
		assert.Equal(t, "/api/v1/thread-groups/"+tgRelease, m[0].Path)
	})
}

func TestConversationGroupAddRemove(t *testing.T) {
	hub := setupThreadGroupTest(t, tgProject)
	out, err := runTG(t, runConversationGroupAdd, "Design", "#standup")
	require.NoError(t, err)
	assert.Equal(t, "Added conversation "+tgConvB+` to thread group "Design" (`+tgDesign+`)`+"\n", out)

	_, err = runTG(t, runConversationGroupAdd, "Design", "conv:"+tgConvA)
	require.NoError(t, err)

	out, err = runTG(t, runConversationGroupRemove, "release", "#release-notes")
	require.NoError(t, err)
	assert.Equal(t, "Removed conversation "+tgConvA+` from thread group "Release" (`+tgRelease+`)`+"\n", out)

	m := hub.mutations()
	require.Len(t, m, 3)
	assert.Equal(t, tgCall{Method: http.MethodPost, Path: "/api/v1/thread-groups/" + tgDesign + "/threads",
		Body: map[string]any{"conversationId": tgConvB}}, m[0])
	assert.Equal(t, map[string]any{"conversationId": tgConvA}, m[1].Body)
	assert.Equal(t, tgCall{Method: http.MethodDelete, Path: "/api/v1/thread-groups/" + tgRelease + "/threads/" + tgConvA}, m[2])

	// An unknown conversation reference fails before any thread group call.
	before := len(hub.mutations())
	_, err = runTG(t, runConversationGroupAdd, "Design", "#missing")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no conversation found for #missing")
	assert.Len(t, hub.mutations(), before)
}

// Against an older hub with its thread groups experiment off
// (ptone/scion#5546), every subcommand reports the clear message, and
// --group on conversation create does too.
func TestConversationGroup_OlderHubWithoutThreadGroups(t *testing.T) {
	const want = "this hub does not serve thread groups: upgrade the hub"
	cases := []struct {
		name string
		fn   func(*cobra.Command, []string) error
		args []string
	}{
		{"list", runConversationGroupList, nil},
		{"get by name", runConversationGroupGet, []string{"Release"}},
		{"create", runConversationGroupCreate, []string{"Ops"}},
		{"rename", runConversationGroupRename, []string{"Release", "x"}},
		{"delete", runConversationGroupDelete, []string{"Release"}},
		{"add", runConversationGroupAdd, []string{"Release", "#standup"}},
		{"remove", runConversationGroupRemove, []string{"Release", "#standup"}},
		{"conversation create --group", runConversationCreate, []string{"new-thread"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hub := setupThreadGroupTest(t, tgProject)
			hub.enabled = false
			autoConfirm = true
			convCreateGroup = "Release"
			_, err := runTG(t, tc.fn, tc.args...)
			require.Error(t, err)
			assert.Contains(t, err.Error(), want)
			assert.True(t, hubclient.IsThreadGroupsNotEnabled(err), "err = %v", err)
			for _, c := range hub.mutations() {
				assert.NotEqual(t, "/api/v1/conversations", c.Path, "a bad --group must not mint a conversation")
			}
		})
	}

	t.Run("get by ID with no project (item route plus probe)", func(t *testing.T) {
		hub := setupThreadGroupTest(t, "")
		hub.enabled = false
		_, err := runTG(t, runConversationGroupGet, tgRelease)
		require.Error(t, err)
		assert.Contains(t, err.Error(), want)
	})

	t.Run("conversation create with a group ID and no project", func(t *testing.T) {
		hub := setupThreadGroupTest(t, "")
		hub.enabled = false
		// The ID is fetched directly; the fake answers it 404 and the
		// collection probe 404 too, so the resolve step reports it.
		convCreateGroup = tgRelease
		_, err := runTG(t, runConversationCreate, "new-thread")
		require.Error(t, err)
		assert.Contains(t, err.Error(), want)
	})

	t.Run("conversation create answered with an older hub's 400", func(t *testing.T) {
		hub := setupThreadGroupTest(t, tgProject)
		hub.rejectGroupOnCreate = true
		convCreateGroup = "Release"
		_, err := runTG(t, runConversationCreate, "new-thread")
		require.Error(t, err)
		assert.Contains(t, err.Error(), want)
		assert.True(t, hubclient.IsThreadGroupsNotEnabled(err))
	})
}

// A 404 for a missing group on a hub that serves thread groups stays a
// not-found.
func TestConversationGroupGet_MissingIDStaysNotFound(t *testing.T) {
	setupThreadGroupTest(t, "")
	_, err := runTG(t, runConversationGroupGet, "22222222-0000-0000-0000-0000000000aa")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Thread group not found")
	assert.False(t, hubclient.IsThreadGroupsNotEnabled(err))
}

func TestConversationCreate_GroupFlag(t *testing.T) {
	hub := setupThreadGroupTest(t, tgProject)
	convCreateGroup = "release"
	out, err := runTG(t, runConversationCreate, "new-thread")
	require.NoError(t, err)
	assert.Contains(t, out, "Group:   Release")
	m := hub.mutations()
	require.Len(t, m, 1)
	assert.Equal(t, "/api/v1/conversations", m[0].Path)
	assert.Equal(t, tgRelease, m[0].Body["threadGroupId"])
	assert.Equal(t, tgProject, m[0].Body["projectId"])

	// An unknown group creates nothing.
	hub2 := setupThreadGroupTest(t, tgProject)
	convCreateGroup = "nope"
	_, err = runTG(t, runConversationCreate, "new-thread")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `no thread group named "nope"`)
	assert.Empty(t, hub2.mutations())

	// Without --group no threadGroupId is sent.
	hub3 := setupThreadGroupTest(t, tgProject)
	_, err = runTG(t, runConversationCreate, "plain")
	require.NoError(t, err)
	m = hub3.mutations()
	require.Len(t, m, 1)
	assert.NotContains(t, m[0].Body, "threadGroupId")
}

// Agent mode keeps every thread group subcommand except delete (design
// section 4, decision Q3), checked against the real command tree.
func TestApplyModeRestrictions_AgentConversationGroupTree(t *testing.T) {
	t.Setenv("SCION_CLI_MODE", "agent")
	root := &cobra.Command{Use: "scion"}
	real := resolveCommandPath(rootCmd, "conversation")
	require.NotNil(t, real)
	root.AddCommand(cloneCommandShape(real))
	require.NotNil(t, resolveCommandPath(root, "conversation.group.delete"), "the real tree has group delete")

	applyModeRestrictions(root, resolveMode())

	var kept []string
	for _, name := range collectCommandNames(root) {
		if strings.HasPrefix(name, "conversation.group") {
			kept = append(kept, name)
		}
	}
	assert.Equal(t, []string{
		"conversation.group",
		"conversation.group.add", "conversation.group.create", "conversation.group.get",
		"conversation.group.list", "conversation.group.remove", "conversation.group.rename",
	}, kept)
	assert.Nil(t, resolveCommandPath(root, "conversation.group.delete"), "agent mode must refuse group delete")
	assert.False(t, agentAllowed["conversation.group.delete"])
	assert.NotNil(t, resolveCommandPath(root, "conversation.create"), "conversation create (with --group) stays allowed")
}
