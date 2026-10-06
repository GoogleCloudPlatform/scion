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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hubStartStub is an httptest hub that answers the calls startAgentViaHub
// makes on the non-attach path: project GET, existing-agent GET, and the
// agent create POST (whose raw JSON body it captures).
type hubStartStub struct {
	server        *httptest.Server
	createBody    map[string]interface{}
	createCalls   int
	existingPhase string // "" → existing-agent GET returns 404
	project       map[string]interface{}
	createStatus  int // non-zero → the create POST fails with this status
}

func newHubStartStub(t *testing.T, projectID, agentName, existingPhase string) *hubStartStub {
	t.Helper()
	stub := &hubStartStub{existingPhase: existingPhase}
	stub.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/"+projectID:
			if stub.project != nil {
				_ = json.NewEncoder(w).Encode(stub.project)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": projectID, "name": "p"})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/"+projectID+"/agents/"+agentName:
			if stub.existingPhase == "" {
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"error": map[string]interface{}{"code": "not_found", "message": "agent not found"},
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"id": "agent-id", "slug": agentName, "name": agentName,
				"phase": stub.existingPhase, "status": stub.existingPhase,
			})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects/"+projectID+"/agents":
			stub.createCalls++
			if stub.createStatus != 0 {
				w.WriteHeader(stub.createStatus)
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"error": map[string]interface{}{"code": "conflict", "message": "refused by hub"},
				})
				return
			}
			raw, err := io.ReadAll(r.Body)
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(raw, &stub.createBody))
			_ = json.NewEncoder(w).Encode(&hubclient.CreateAgentResponse{
				Agent: &hubclient.Agent{
					ID: "agent-id", Slug: agentName, Name: agentName,
					Status: "running", Phase: "running", Created: time.Now().UTC(),
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(stub.server.Close)
	return stub
}

func (s *hubStartStub) hubCtx(t *testing.T, projectID string) *HubContext {
	t.Helper()
	client, err := hubclient.New(s.server.URL)
	require.NoError(t, err)
	return &HubContext{Client: client, Endpoint: s.server.URL, ProjectID: projectID}
}

// resetHubStartGlobals zeroes the package-level flag vars startAgentViaHub
// reads and restores them after the test.
func resetHubStartGlobals(t *testing.T) {
	t.Helper()
	origNoAuth, origAttach, origForce := noAuth, attach, forceResume
	origTemplate, origImage, origBroker := templateName, agentImage, runtimeBrokerID
	origHC, origHA, origLabels := harnessConfigFlag, harnessAuthFlag, labelFlags
	origRole, origMM, origFormat := agentRoleFlag, messageModeFlag, outputFormat
	t.Cleanup(func() {
		noAuth, attach, forceResume = origNoAuth, origAttach, origForce
		templateName, agentImage, runtimeBrokerID = origTemplate, origImage, origBroker
		harnessConfigFlag, harnessAuthFlag, labelFlags = origHC, origHA, origLabels
		agentRoleFlag, messageModeFlag, outputFormat = origRole, origMM, origFormat
	})
	noAuth, attach, forceResume = false, false, false
	templateName, agentImage, runtimeBrokerID = "", "", ""
	harnessConfigFlag, harnessAuthFlag, labelFlags = "", "", nil
	agentRoleFlag, messageModeFlag, outputFormat = "", "", ""
}

// newFlagCmd builds a throwaway command carrying the start flags the warning
// inspects, and marks the given ones as explicitly set.
func newFlagCmd(t *testing.T, set map[string]string) *cobra.Command {
	t.Helper()
	c := &cobra.Command{Use: "start"}
	var s string
	var b bool
	c.Flags().StringVar(&s, "image", "", "")
	c.Flags().StringVar(&s, "type", "", "")
	c.Flags().StringVar(&s, "broker", "", "")
	c.Flags().BoolVar(&b, "no-auth", false, "")
	c.Flags().BoolVar(&b, "attach", false, "")
	for name, val := range set {
		require.NoError(t, c.Flags().Set(name, val))
	}
	return c
}

// ptone/scion#1912: --no-auth must reach the hub on the wire.
func TestStartAgentViaHub_SendsNoAuthOnWire(t *testing.T) {
	resetHubStartGlobals(t)
	const projectID, agentName = "proj-noauth", "noauth-agent"

	for _, tc := range []struct {
		name   string
		noAuth bool
	}{{"set", true}, {"unset", false}} {
		t.Run(tc.name, func(t *testing.T) {
			stub := newHubStartStub(t, projectID, agentName, "")
			noAuth = tc.noAuth
			var err error
			captureStdIO(t, func() {
				err = startAgentViaHub(nil, stub.hubCtx(t, projectID), agentName, "", false, nil)
			})
			require.NoError(t, err)
			require.Equal(t, 1, stub.createCalls)
			v, present := stub.createBody["noAuth"]
			if tc.noAuth {
				assert.Equal(t, true, v, "create body must carry noAuth=true")
			} else {
				assert.False(t, present, "noAuth must be omitted when not set")
			}
		})
	}
}

// ptone/scion#1911: the existing-agent pre-check converts start on a stopped
// or suspended agent into a resume request, and leaves other phases alone.
// The success line uses the same verb as the action line (a stopped agent is
// "Restarting" and then "restarted", not "resumed").
func TestStartAgentViaHub_ExistingAgentPreCheck(t *testing.T) {
	const projectID, agentName = "proj-precheck", "precheck-agent"

	for _, tc := range []struct {
		phase      string
		wantResume bool
		wantAction string
		wantResult string
	}{
		{"", false, "Starting agent", "started"},
		{"stopped", true, "Restarting agent", "restarted"},
		{"suspended", true, "Resuming agent", "resumed"},
		{"running", false, "Starting agent", "started"},
		// The hub only restarts error-phase agents in place with --force,
		// so start leaves them as-is (and the hub still reports 409).
		{"error", false, "Starting agent", "started"},
	} {
		t.Run("phase="+tc.phase, func(t *testing.T) {
			resetHubStartGlobals(t)
			stub := newHubStartStub(t, projectID, agentName, tc.phase)
			var err error
			stdout, stderr := captureStdIO(t, func() {
				err = startAgentViaHub(nil, stub.hubCtx(t, projectID), agentName, "", false, nil)
			})
			require.NoError(t, err)
			require.Equal(t, 1, stub.createCalls)
			if tc.wantResume {
				assert.Equal(t, true, stub.createBody["resume"])
			} else {
				_, present := stub.createBody["resume"]
				assert.False(t, present, "resume must not be set for phase %q", tc.phase)
			}
			assert.Contains(t, stdout, tc.wantAction+" '"+agentName+"'")
			assert.Contains(t, stderr, "Agent '"+agentName+"' "+tc.wantResult+" via Hub.")
		})
	}
}

// ptone/scion#1911 / #1912: explicitly set config flags that the hub will not
// apply to an existing agent produce one stderr warning naming them, and
// --no-auth cites ptone/scion#1855.
func TestStartAgentViaHub_WarnsIgnoredFlagsForExistingAgent(t *testing.T) {
	const projectID, agentName = "proj-warn", "warn-agent"

	t.Run("stopped agent with config flags", func(t *testing.T) {
		resetHubStartGlobals(t)
		stub := newHubStartStub(t, projectID, agentName, "stopped")
		noAuth, agentImage = true, "img:1"
		cmd := newFlagCmd(t, map[string]string{"image": "img:1", "no-auth": "true", "attach": "true"})
		attach = false // --attach is applied by the hub; keep the test off the attach path
		var err error
		_, stderr := captureStdIO(t, func() {
			err = startAgentViaHub(cmd, stub.hubCtx(t, projectID), agentName, "", false, nil)
		})
		require.NoError(t, err)
		assert.Contains(t, stderr, "these flags are not applied to an existing agent: --image, --no-auth\n")
		assert.Contains(t, stderr, "ptone/scion#1855")
		assert.NotContains(t, stderr, "--attach")
		assert.NotContains(t, stderr, "--type")
	})

	t.Run("resume on stopped agent with --no-auth", func(t *testing.T) {
		resetHubStartGlobals(t)
		stub := newHubStartStub(t, projectID, agentName, "stopped")
		noAuth = true
		cmd := newFlagCmd(t, map[string]string{"no-auth": "true"})
		var err error
		stdout, stderr := captureStdIO(t, func() {
			err = startAgentViaHub(cmd, stub.hubCtx(t, projectID), agentName, "", true, nil)
		})
		require.NoError(t, err)
		assert.Contains(t, stderr, "--no-auth")
		assert.Contains(t, stderr, "ptone/scion#1855")
		assert.Contains(t, stdout, "Restarting agent")
	})

	t.Run("new agent gets no warning", func(t *testing.T) {
		resetHubStartGlobals(t)
		stub := newHubStartStub(t, projectID, agentName, "")
		noAuth = true
		cmd := newFlagCmd(t, map[string]string{"image": "img:1", "no-auth": "true"})
		var err error
		_, stderr := captureStdIO(t, func() {
			err = startAgentViaHub(cmd, stub.hubCtx(t, projectID), agentName, "", false, nil)
		})
		require.NoError(t, err)
		assert.NotContains(t, stderr, "not applied to an existing agent")
		assert.Equal(t, true, stub.createBody["noAuth"])
	})

	t.Run("running agent gets no warning", func(t *testing.T) {
		resetHubStartGlobals(t)
		stub := newHubStartStub(t, projectID, agentName, "running")
		cmd := newFlagCmd(t, map[string]string{"image": "img:1"})
		var err error
		_, stderr := captureStdIO(t, func() {
			err = startAgentViaHub(cmd, stub.hubCtx(t, projectID), agentName, "", false, nil)
		})
		require.NoError(t, err)
		assert.NotContains(t, stderr, "not applied to an existing agent")
	})

	t.Run("refused create gets no warning", func(t *testing.T) {
		resetHubStartGlobals(t)
		stub := newHubStartStub(t, projectID, agentName, "stopped")
		stub.createStatus = http.StatusConflict
		noAuth = true
		cmd := newFlagCmd(t, map[string]string{"image": "img:1", "no-auth": "true"})
		var err error
		_, stderr := captureStdIO(t, func() {
			err = startAgentViaHub(cmd, stub.hubCtx(t, projectID), agentName, "", false, nil)
		})
		require.Error(t, err)
		assert.Equal(t, 1, stub.createCalls)
		assert.NotContains(t, stderr, "not applied to an existing agent")
		assert.NotContains(t, stderr, "ptone/scion#1855")
	})

	t.Run("real start command flags", func(t *testing.T) {
		resetHubStartGlobals(t)
		stub := newHubStartStub(t, projectID, agentName, "stopped")
		imageFlag := startCmd.Flags().Lookup("image")
		profileFlag := startCmd.Flag("profile") // inherited persistent flag
		require.NotNil(t, imageFlag)
		require.NotNil(t, profileFlag)
		origImageValue, origImageChanged, origProfileChanged := imageFlag.Value.String(), imageFlag.Changed, profileFlag.Changed
		t.Cleanup(func() {
			_ = imageFlag.Value.Set(origImageValue)
			imageFlag.Changed, profileFlag.Changed = origImageChanged, origProfileChanged
		})
		require.NoError(t, startCmd.Flags().Set("image", "img:2"))
		profileFlag.Changed = true // mark explicit without changing the active profile

		var err error
		_, stderr := captureStdIO(t, func() {
			err = startAgentViaHub(startCmd, stub.hubCtx(t, projectID), agentName, "", false, nil)
		})
		require.NoError(t, err)
		assert.Contains(t, stderr, "these flags are not applied to an existing agent: --image, --profile\n")
	})

	t.Run("no explicit flags gets no warning", func(t *testing.T) {
		resetHubStartGlobals(t)
		stub := newHubStartStub(t, projectID, agentName, "stopped")
		var err error
		_, stderr := captureStdIO(t, func() {
			err = startAgentViaHub(newFlagCmd(t, nil), stub.hubCtx(t, projectID), agentName, "", false, nil)
		})
		require.NoError(t, err)
		assert.NotContains(t, stderr, "not applied to an existing agent")
	})
}

// Every flag the warning inspects must exist on start or resume (directly or
// inherited), so a renamed or removed flag cannot silently drop out of it.
func TestConfigFlagsNotAppliedToExistingAgent_AreDefined(t *testing.T) {
	for _, name := range configFlagsNotAppliedToExistingAgent {
		assert.True(t, startCmd.Flag(name) != nil || resumeCmd.Flag(name) != nil,
			"--%s is not defined on start or resume", name)
	}
}

func TestHubCreateReusesExistingAgent(t *testing.T) {
	for _, tc := range []struct {
		phase         string
		resume, force bool
		want          bool
	}{
		{"", true, true, false},
		{"suspended", false, false, true},
		{"created", false, false, true},
		{"stopped", false, false, false},
		{"stopped", true, false, true},
		{"error", true, false, false},
		{"error", true, true, true},
		{"running", true, true, false},
		{"provisioning", true, false, false},
	} {
		assert.Equal(t, tc.want, hubCreateReusesExistingAgent(tc.phase, tc.resume, tc.force),
			"phase=%q resume=%v force=%v", tc.phase, tc.resume, tc.force)
	}
}

func TestHubStartActionWord(t *testing.T) {
	assert.Equal(t, "Starting", hubStartActionWord("", false, false))
	assert.Equal(t, "Restarting", hubStartActionWord("stopped", true, false))
	assert.Equal(t, "Resuming", hubStartActionWord("suspended", true, false))
	assert.Equal(t, "Force-resuming", hubStartActionWord("error", true, true))
	assert.Equal(t, "Resuming", hubStartActionWord("", true, false))
}

// ptone/scion#1915: the clone log line prints the URL the hub will use (no
// doubled scheme) and describes the transport accurately.
func TestStartAgentViaHub_CloneLogLine(t *testing.T) {
	const projectID, agentName = "proj-clone", "clone-agent"
	for _, tc := range []struct {
		name, remote, label, wantURL, wantNote string
	}{
		{"https remote", "https://github.com/org/repo", "", "https://github.com/org/repo", "HTTPS clone with GITHUB_TOKEN"},
		{"schemeless remote", "github.com/org/repo", "", "https://github.com/org/repo.git", "HTTPS clone with GITHUB_TOKEN"},
		{"git label", "github.com/org/repo", "git://172.17.0.1:9418/org/repo", "git://172.17.0.1:9418/org/repo", "unauthenticated git:// clone"},
		{"ssh label", "github.com/org/repo", "git@github.com:org/repo.git", "git@github.com:org/repo.git", "SSH clone; GITHUB_TOKEN is not used"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetHubStartGlobals(t)
			stub := newHubStartStub(t, projectID, agentName, "")
			stub.project = map[string]interface{}{"id": projectID, "name": "p", "gitRemote": tc.remote}
			if tc.label != "" {
				stub.project["labels"] = map[string]string{"scion.dev/clone-url": tc.label}
			}
			var err error
			_, stderr := captureStdIO(t, func() {
				err = startAgentViaHub(nil, stub.hubCtx(t, projectID), agentName, "", false, nil)
			})
			require.NoError(t, err)
			assert.Contains(t, stderr, "Using hub, cloning repo "+tc.wantURL+"\n")
			assert.Contains(t, stderr, tc.wantNote)
			assert.NotContains(t, stderr, "https://https://")
		})
	}
}

// Shared-workspace projects are not cloned by the hub (agents mount the
// shared workspace), so the CLI must not claim a clone.
func TestStartAgentViaHub_SharedWorkspaceLogLine(t *testing.T) {
	const projectID, agentName = "proj-shared", "shared-agent"
	resetHubStartGlobals(t)
	stub := newHubStartStub(t, projectID, agentName, "")
	stub.project = map[string]interface{}{
		"id": projectID, "name": "p", "gitRemote": "github.com/org/repo",
		"labels": map[string]string{"scion.dev/workspace-mode": "shared"},
	}
	var err error
	_, stderr := captureStdIO(t, func() {
		err = startAgentViaHub(nil, stub.hubCtx(t, projectID), agentName, "", false, nil)
	})
	require.NoError(t, err)
	assert.Contains(t, stderr, "Using hub, shared workspace for repo github.com/org/repo\n")
	assert.NotContains(t, stderr, "cloning repo")
	assert.NotContains(t, stderr, "GITHUB_TOKEN")
}

func TestHubCloneTransportNote(t *testing.T) {
	assert.Contains(t, hubCloneTransportNote("https://h/r.git"), "HTTPS clone with GITHUB_TOKEN")
	assert.Contains(t, hubCloneTransportNote("http://h/r.git"), "HTTP clone")
	assert.Contains(t, hubCloneTransportNote("ssh://git@h/r.git"), "SSH clone")
	assert.Contains(t, hubCloneTransportNote("git@h:r.git"), "SSH clone")
	assert.Contains(t, hubCloneTransportNote("git://h/r"), "git:// clone")
	assert.Contains(t, hubCloneTransportNote("/srv/repo"), "path")
}

// The hub lists only brokers the caller may use in a 422 no_runtime_broker.
// When that list is empty, the CLI surfaces the hub's message instead of
// prompting; with a non-empty list and autoConfirm set (no picker), it keeps
// the hub's reason and asks for --broker, naming the broker when there is one.
func TestCreateAgentWithBrokerResolution_NoRuntimeBroker(t *testing.T) {
	const projectID = "proj-nrb"
	// Hub reasons as pkg/hub resolveRuntimeBroker sends them: the
	// permission message comes with an empty usable list, the
	// default-unavailable one with alternatives.
	const (
		noneMsg    = "No runtime brokers available for this project that you have permission to use"
		defaultMsg = "Default runtime broker is unavailable; specify an alternative"
	)
	for _, tc := range []struct {
		name      string
		hubMsg    string
		brokers   []map[string]interface{}
		wantInErr string
	}{
		{"empty list", noneMsg, []map[string]interface{}{}, noneMsg},
		{"missing list", noneMsg, nil, noneMsg},
		{"single usable broker, autoConfirm", defaultMsg, []map[string]interface{}{{"id": "b1", "name": "one", "status": "online"}}, `Default runtime broker is unavailable: runtime broker "one" is available, retry with --broker "one"`},
		{"several usable brokers, autoConfirm", defaultMsg, []map[string]interface{}{{"id": "b1", "name": "one", "status": "online"}, {"id": "b2", "name": "two", "status": "online"}}, `Default runtime broker is unavailable: multiple runtime brokers available ("one", "two"), specify a broker with --broker <name>`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Never prompt, whatever stdin is: autoConfirm skips the
			// interactive picker even on a TTY.
			origAutoConfirm := autoConfirm
			autoConfirm = true
			t.Cleanup(func() { autoConfirm = origAutoConfirm })
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				details := map[string]interface{}{}
				if tc.brokers != nil {
					details["availableBrokers"] = tc.brokers
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnprocessableEntity)
				_ = json.NewEncoder(w).Encode(map[string]interface{}{"error": map[string]interface{}{
					"code":    "no_runtime_broker",
					"message": tc.hubMsg,
					"details": details,
				}})
			}))
			t.Cleanup(srv.Close)
			client, err := hubclient.New(srv.URL)
			require.NoError(t, err)
			hubCtx := &HubContext{Client: client, Endpoint: srv.URL, ProjectID: projectID}

			var resp *hubclient.CreateAgentResponse
			stdout, _ := captureStdIO(t, func() {
				resp, err = createAgentWithBrokerResolution(context.Background(), hubCtx, projectID,
					&hubclient.CreateAgentRequest{Name: "a"})
			})
			require.Error(t, err)
			assert.Nil(t, resp)
			assert.Contains(t, err.Error(), tc.wantInErr)
			assert.True(t, isHubFailure(err), "a no_runtime_broker rejection is a hub failure (no Usage block)")
			assert.Equal(t, 1, calls, "must not retry")
			assert.NotContains(t, stdout, "Select a broker")
			assert.NotContains(t, stdout, "Use runtime broker")
		})
	}
}

// TestNonInteractiveBrokerMessage covers the message selection for a
// no_runtime_broker 422 when the CLI cannot prompt (ptone/scion#2861): the
// "multiple runtime brokers" wording only when there really are several
// candidates, otherwise the hub's own reason plus the single broker's name.
func TestNonInteractiveBrokerMessage(t *testing.T) {
	one := []interface{}{map[string]interface{}{"id": "b1", "name": "laptop", "status": "online"}}
	two := []interface{}{
		map[string]interface{}{"id": "b1", "name": "laptop", "status": "online", "isDefault": true},
		map[string]interface{}{"id": "b2", "name": "server", "status": "online"},
	}
	tests := []struct {
		name       string
		hubMessage string
		brokers    []interface{}
		want       string
		notWant    string
	}{
		{
			name:       "default unavailable, single alternative names it",
			hubMessage: "Default runtime broker is unavailable; specify an alternative",
			brokers:    one,
			want:       `Default runtime broker is unavailable: runtime broker "laptop" is available, retry with --broker "laptop"`,
			notWant:    "specify an alternative",
		},
		{
			name:       "single broker without a name falls back to its id",
			hubMessage: "Default runtime broker is unavailable; specify an alternative",
			brokers:    []interface{}{map[string]interface{}{"id": "b1"}},
			want:       `Default runtime broker is unavailable: runtime broker "b1" is available, retry with --broker "b1"`,
		},
		{
			name:       "single broker entry with no name or id",
			hubMessage: "Default runtime broker is unavailable; specify an alternative",
			brokers:    []interface{}{map[string]interface{}{}},
			want:       "Default runtime broker is unavailable: specify a broker with --broker <name>",
		},
		{
			name:       "broker name with a space is quoted",
			hubMessage: "Default runtime broker is unavailable; specify an alternative",
			brokers:    []interface{}{map[string]interface{}{"id": "b1", "name": "my laptop"}},
			want:       `Default runtime broker is unavailable: runtime broker "my laptop" is available, retry with --broker "my laptop"`,
		},
		{
			name:       "hub multiple-brokers message is replaced by the CLI hint",
			hubMessage: "Multiple runtime brokers available for this project; specify runtimeBrokerId to select one",
			brokers:    two,
			want:       `multiple runtime brokers available ("laptop", "server"), specify a broker with --broker <name>`,
			notWant:    "runtimeBrokerId",
		},
		{
			name:       "default unavailable with several alternatives keeps the hub reason",
			hubMessage: "Default runtime broker is unavailable; specify an alternative",
			brokers:    two,
			want:       `Default runtime broker is unavailable: multiple runtime brokers available ("laptop", "server"), specify a broker with --broker <name>`,
		},
		{
			name:       "several brokers but only one parsable name lists none",
			hubMessage: "Default runtime broker is unavailable; specify an alternative",
			brokers:    []interface{}{map[string]interface{}{"id": "b1", "name": "laptop"}, map[string]interface{}{}},
			want:       "Default runtime broker is unavailable: multiple runtime brokers available, specify a broker with --broker <name>",
		},
		{
			name:       "non-map broker entry is skipped",
			hubMessage: "Default runtime broker is unavailable; specify an alternative",
			brokers:    []interface{}{"not-a-map", map[string]interface{}{"id": "b1", "name": "laptop"}, map[string]interface{}{"id": "b2", "name": "server"}},
			want:       `Default runtime broker is unavailable: multiple runtime brokers available ("laptop", "server"), specify a broker with --broker <name>`,
		},
		{
			name:       "unrecognised hub reason is kept verbatim",
			hubMessage: "Broker pool exhausted",
			brokers:    one,
			want:       `Broker pool exhausted: runtime broker "laptop" is available, retry with --broker "laptop"`,
		},
		{
			name:       "empty hub message gets a generic reason",
			hubMessage: "",
			brokers:    one,
			want:       `no runtime broker selected: runtime broker "laptop" is available, retry with --broker "laptop"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := nonInteractiveBrokerMessage(tt.hubMessage, tt.brokers)
			assert.Equal(t, tt.want, got)
			if len(tt.brokers) == 1 {
				assert.NotContains(t, got, "multiple", "a single candidate must not be described as multiple")
			}
			if tt.notWant != "" {
				assert.NotContains(t, got, tt.notWant)
			}
		})
	}
}
