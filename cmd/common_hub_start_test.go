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
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hubStartStub is an httptest hub that answers the calls startAgentViaHub
// makes on the non-attach path: project GET, existing-agent GET, and the
// agent create POST (whose raw JSON body it captures, even when the create
// fails).
type hubStartStub struct {
	server           *httptest.Server
	createBody       map[string]interface{}
	createCalls      int
	existingPhase    string                 // "" → existing-agent GET returns 404
	existingLaunch   map[string]interface{} // launch object of the existing agent (nil → none)
	project          map[string]interface{}
	createStatus     int                    // non-zero → the create POST fails with this status
	createErrCode    string                 // error code for a failed create ("" → "conflict")
	createErrMsg     string                 // error message for a failed create ("" → "refused by hub")
	createErrDetails map[string]interface{} // error details for a failed create (nil → none)
	afterCreate      []string               // "METHOD path" of every request after the first create
}

func newHubStartStub(t *testing.T, projectID, agentName, existingPhase string) *hubStartStub {
	t.Helper()
	stub := &hubStartStub{existingPhase: existingPhase}
	stub.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if stub.createCalls > 0 {
			stub.afterCreate = append(stub.afterCreate, r.Method+" "+r.URL.Path)
		}
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
			existing := map[string]interface{}{
				"id": "agent-id", "slug": agentName, "name": agentName,
				"phase": stub.existingPhase, "status": stub.existingPhase,
			}
			if stub.existingLaunch != nil {
				existing["launch"] = stub.existingLaunch
			}
			_ = json.NewEncoder(w).Encode(existing)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects/"+projectID+"/agents":
			stub.createCalls++
			// Never FailNow in the handler goroutine: it would exit without a
			// response and leave the client hanging. Report and answer 500.
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("hub stub: reading create body: %v", err)
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if err := json.Unmarshal(raw, &stub.createBody); err != nil {
				t.Errorf("hub stub: decoding create body: %v", err)
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if stub.createStatus != 0 {
				code, msg := stub.createErrCode, stub.createErrMsg
				if code == "" {
					code = "conflict"
				}
				if msg == "" {
					msg = "refused by hub"
				}
				errBody := map[string]interface{}{"code": code, "message": msg}
				if stub.createErrDetails != nil {
					errBody["details"] = stub.createErrDetails
				}
				w.WriteHeader(stub.createStatus)
				_ = json.NewEncoder(w).Encode(map[string]interface{}{"error": errBody})
				return
			}
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

// ptone/scion#8584: `scion start` never implicitly sets resume on the wire and
// always prints "Starting" (never "Restarting" or "Resuming"). Resumable
// existing agents are refused by the Hub with 409 agent_exists_resumable,
// while `created` agents continue to be started.
func TestStartAgentViaHub_DoesNotImplicitlyResume(t *testing.T) {
	const projectID, agentName = "proj-no-implicit", "start-agent"

	for _, phase := range []string{"", "created", "provisioning", "running", "error"} {
		t.Run("start/phase="+phase, func(t *testing.T) {
			resetHubStartGlobals(t)
			stub := newHubStartStub(t, projectID, agentName, phase)
			cmd := newFlagCmd(t, map[string]string{"image": "img:1"})
			var err error
			stdout, stderr := captureStdIO(t, func() {
				err = startAgentViaHub(cmd, stub.hubCtx(t, projectID), agentName, "", false, nil)
			})
			require.NoError(t, err)
			require.Equal(t, 1, stub.createCalls)
			_, present := stub.createBody["resume"]
			assert.False(t, present, "start must never set resume for phase %q", phase)
			assert.Contains(t, stdout, "Starting agent '"+agentName+"'")
			assert.Contains(t, stderr, "Agent '"+agentName+"' started via Hub.")
			assert.NotContains(t, stderr, "not applied to an existing agent")
		})
	}

	for _, phase := range []string{"suspended", "stopped", "halted"} {
		t.Run("start-refused/phase="+phase, func(t *testing.T) {
			resetHubStartGlobals(t)
			stub := newHubStartStub(t, projectID, agentName, phase)
			stub.createStatus = http.StatusConflict
			stub.createErrCode = "agent_exists_resumable"
			stub.createErrMsg = `agent "` + agentName + `" already exists (phase: ` + phase + `); run 'scion resume ` + agentName + `' to resume it, delete it, or use another name`
			cmd := newFlagCmd(t, map[string]string{"image": "img:1", "no-auth": "true"})
			var err error
			stdout, stderr := captureStdIO(t, func() {
				err = startAgentViaHub(cmd, stub.hubCtx(t, projectID), agentName, "", false, nil)
			})
			require.Error(t, err)
			require.Equal(t, 1, stub.createCalls)
			_, present := stub.createBody["resume"]
			assert.False(t, present, "start must not set resume for phase %q", phase)
			assert.Contains(t, stdout, "Starting agent '"+agentName+"'")
			assert.Contains(t, err.Error(), phase)
			assert.Contains(t, err.Error(), "scion resume "+agentName)
			assert.NotContains(t, stderr, "not applied to an existing agent")
		})
	}
}

// ptone/scion#8584: `scion resume` sends resume=true, uses the appropriate
// action/result verb (Restarting/restarted for stopped and halted,
// Resuming/resumed for suspended, Force-resuming for error+--force), and
// keeps the ignored-flags warning.
func TestStartAgentViaHub_ExplicitResumeActionAndResult(t *testing.T) {
	const projectID, agentName = "proj-resume", "resume-agent"

	for _, tc := range []struct {
		phase      string
		force      bool
		wantAction string
		wantResult string
	}{
		{"suspended", false, "Resuming agent", "resumed"},
		{"stopped", false, "Restarting agent", "restarted"},
		{"halted", false, "Restarting agent", "restarted"},
		{"error", true, "Force-resuming agent", "resumed"},
	} {
		t.Run("phase="+tc.phase, func(t *testing.T) {
			resetHubStartGlobals(t)
			forceResume = tc.force
			stub := newHubStartStub(t, projectID, agentName, tc.phase)
			var err error
			stdout, stderr := captureStdIO(t, func() {
				err = startAgentViaHub(nil, stub.hubCtx(t, projectID), agentName, "", true, nil)
			})
			require.NoError(t, err)
			require.Equal(t, 1, stub.createCalls)
			assert.Equal(t, true, stub.createBody["resume"])
			if tc.force {
				assert.Equal(t, true, stub.createBody["forceResume"])
			}
			assert.Contains(t, stdout, tc.wantAction+" '"+agentName+"'")
			assert.Contains(t, stderr, "Agent '"+agentName+"' "+tc.wantResult+" via Hub.")
		})
	}
}

// D-T8 addition (design wave 3 D-c, R3; ptone/scion#4670, updated for #8584):
// an agent left in error by a failed start carries its resume memory in
// launch.resumePhase. `scion start` never implies resume from it; `scion resume`
// uses it for the action line, result word, and ignored-flags warning.
func TestStartAgentViaHub_ExistingAgentResumeMemory(t *testing.T) {
	const projectID, agentName = "proj-memory", "memory-agent"

	t.Run("start never implies resume from resumePhase", func(t *testing.T) {
		for _, resumePhase := range []string{"suspended", "stopped", "halted", "created", "provisioning", ""} {
			t.Run("resumePhase="+resumePhase, func(t *testing.T) {
				resetHubStartGlobals(t)
				stub := newHubStartStub(t, projectID, agentName, "error")
				stub.existingLaunch = map[string]interface{}{
					"id": "l1", "state": "ended", "active": false, "kind": "start", "error": "crash_loop",
				}
				if resumePhase != "" {
					stub.existingLaunch["resumePhase"] = resumePhase
				}
				cmd := newFlagCmd(t, map[string]string{"image": "img:1"})
				var err error
				stdout, stderr := captureStdIO(t, func() {
					err = startAgentViaHub(cmd, stub.hubCtx(t, projectID), agentName, "", false, nil)
				})
				require.NoError(t, err)
				require.Equal(t, 1, stub.createCalls)
				_, present := stub.createBody["resume"]
				assert.False(t, present, "start must not set resume for resumePhase %q", resumePhase)
				assert.Contains(t, stdout, "Starting agent '"+agentName+"'")
				assert.NotContains(t, stderr, "not applied to an existing agent")
			})
		}
	})

	t.Run("resume uses resumePhase for action and warning", func(t *testing.T) {
		for _, tc := range []struct {
			resumePhase string
			wantAction  string
			wantResult  string
			wantWarning string // "" → no flags warning
		}{
			{"suspended", "Resuming agent", "resumed", "(phase: error, resumes as suspended)"},
			{"stopped", "Restarting agent", "restarted", "(phase: error, resumes as stopped)"},
			{"halted", "Restarting agent", "restarted", "(phase: error, resumes as halted)"},
			{"provisioning", "Resuming agent", "resumed", ""},
		} {
			t.Run("resumePhase="+tc.resumePhase, func(t *testing.T) {
				resetHubStartGlobals(t)
				stub := newHubStartStub(t, projectID, agentName, "error")
				stub.existingLaunch = map[string]interface{}{
					"id": "l1", "state": "ended", "active": false, "kind": "start", "error": "crash_loop",
					"resumePhase": tc.resumePhase,
				}
				cmd := newFlagCmd(t, map[string]string{"image": "img:1"})
				var err error
				stdout, stderr := captureStdIO(t, func() {
					err = startAgentViaHub(cmd, stub.hubCtx(t, projectID), agentName, "", true, nil)
				})
				require.NoError(t, err)
				require.Equal(t, 1, stub.createCalls)
				assert.Equal(t, true, stub.createBody["resume"])
				assert.Contains(t, stdout, tc.wantAction+" '"+agentName+"'")
				assert.Contains(t, stderr, "Agent '"+agentName+"' "+tc.wantResult+" via Hub.")
				if tc.wantWarning == "" {
					assert.NotContains(t, stderr, "not applied to an existing agent")
				} else {
					assert.Contains(t, stderr, "Warning: agent '"+agentName+"' already exists "+tc.wantWarning+
						" and is reused in place; these flags are not applied to an existing agent: --image\n")
				}
			})
		}
	})
}

// ptone/scion#1911 / #1912 / #8584: on `scion resume`, explicitly set config
// flags that the hub will not apply to an existing agent produce one stderr
// warning naming them, and --no-auth cites ptone/scion#1855. `scion start`
// does not emit this warning.
func TestStartAgentViaHub_WarnsIgnoredFlagsForExistingAgent(t *testing.T) {
	const projectID, agentName = "proj-warn", "warn-agent"

	t.Run("resume on stopped agent with config flags", func(t *testing.T) {
		resetHubStartGlobals(t)
		stub := newHubStartStub(t, projectID, agentName, "stopped")
		noAuth, agentImage = true, "img:1"
		cmd := newFlagCmd(t, map[string]string{"image": "img:1", "no-auth": "true", "attach": "true"})
		attach = false // --attach is applied by the hub; keep the test off the attach path
		var err error
		stdout, stderr := captureStdIO(t, func() {
			err = startAgentViaHub(cmd, stub.hubCtx(t, projectID), agentName, "", true, nil)
		})
		require.NoError(t, err)
		assert.Contains(t, stdout, "Restarting agent")
		assert.Contains(t, stderr, "these flags are not applied to an existing agent: --image, --no-auth\n")
		assert.Contains(t, stderr, "ptone/scion#1855")
		assert.NotContains(t, stderr, "--attach")
		assert.NotContains(t, stderr, "--type")
	})

	t.Run("resume on suspended agent with --no-auth", func(t *testing.T) {
		resetHubStartGlobals(t)
		stub := newHubStartStub(t, projectID, agentName, "suspended")
		noAuth = true
		cmd := newFlagCmd(t, map[string]string{"no-auth": "true"})
		var err error
		stdout, stderr := captureStdIO(t, func() {
			err = startAgentViaHub(cmd, stub.hubCtx(t, projectID), agentName, "", true, nil)
		})
		require.NoError(t, err)
		assert.Contains(t, stderr, "--no-auth")
		assert.Contains(t, stderr, "ptone/scion#1855")
		assert.Contains(t, stdout, "Resuming agent")
	})

	t.Run("start on created agent gets no warning", func(t *testing.T) {
		resetHubStartGlobals(t)
		stub := newHubStartStub(t, projectID, agentName, "created")
		noAuth = true
		cmd := newFlagCmd(t, map[string]string{"image": "img:1", "no-auth": "true"})
		var err error
		_, stderr := captureStdIO(t, func() {
			err = startAgentViaHub(cmd, stub.hubCtx(t, projectID), agentName, "", false, nil)
		})
		require.NoError(t, err)
		assert.NotContains(t, stderr, "not applied to an existing agent")
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

	t.Run("refused resume gets no warning", func(t *testing.T) {
		resetHubStartGlobals(t)
		stub := newHubStartStub(t, projectID, agentName, "running")
		stub.createStatus = http.StatusConflict
		noAuth = true
		cmd := newFlagCmd(t, map[string]string{"image": "img:1", "no-auth": "true"})
		var err error
		_, stderr := captureStdIO(t, func() {
			err = startAgentViaHub(cmd, stub.hubCtx(t, projectID), agentName, "", true, nil)
		})
		require.Error(t, err)
		assert.Equal(t, 1, stub.createCalls)
		assert.NotContains(t, stderr, "not applied to an existing agent")
		assert.NotContains(t, stderr, "ptone/scion#1855")
	})

	t.Run("real resume command flags", func(t *testing.T) {
		resetHubStartGlobals(t)
		stub := newHubStartStub(t, projectID, agentName, "stopped")
		imageFlag := resumeCmd.Flags().Lookup("image")
		profileFlag := resumeCmd.Flag("profile") // inherited persistent flag
		require.NotNil(t, imageFlag)
		require.NotNil(t, profileFlag)
		origImageValue, origImageChanged, origProfileChanged := imageFlag.Value.String(), imageFlag.Changed, profileFlag.Changed
		t.Cleanup(func() {
			_ = imageFlag.Value.Set(origImageValue)
			imageFlag.Changed, profileFlag.Changed = origImageChanged, origProfileChanged
		})
		require.NoError(t, resumeCmd.Flags().Set("image", "img:2"))
		profileFlag.Changed = true // mark explicit without changing the active profile

		var err error
		_, stderr := captureStdIO(t, func() {
			err = startAgentViaHub(resumeCmd, stub.hubCtx(t, projectID), agentName, "", true, nil)
		})
		require.NoError(t, err)
		assert.Contains(t, stderr, "these flags are not applied to an existing agent: --image, --profile\n")
	})

	t.Run("no explicit flags gets no warning", func(t *testing.T) {
		resetHubStartGlobals(t)
		stub := newHubStartStub(t, projectID, agentName, "stopped")
		var err error
		_, stderr := captureStdIO(t, func() {
			err = startAgentViaHub(newFlagCmd(t, nil), stub.hubCtx(t, projectID), agentName, "", true, nil)
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
	one := []map[string]interface{}{{"id": "b1", "name": "one", "status": "online"}}
	two := []map[string]interface{}{{"id": "b1", "name": "one", "status": "online"}, {"id": "b2", "name": "two", "status": "online"}}
	const (
		oneErr = `Default runtime broker is unavailable: runtime broker "one" is available, retry with --broker "one"`
		twoErr = `Default runtime broker is unavailable: multiple runtime brokers available ("one", "two"), specify a broker with --broker <name>`
	)
	for _, tc := range []struct {
		name        string
		hubMsg      string
		brokers     []map[string]interface{}
		autoConfirm bool
		wantInErr   string
	}{
		{"empty list", noneMsg, []map[string]interface{}{}, true, noneMsg},
		{"missing list", noneMsg, nil, true, noneMsg},
		{"single usable broker, autoConfirm", defaultMsg, one, true, oneErr},
		{"several usable brokers, autoConfirm", defaultMsg, two, true, twoErr},
		// Without a terminal and without --yes the picker must not read
		// stdin (an answer is piped in to prove it is ignored).
		{"single usable broker, no terminal", defaultMsg, one, false, oneErr},
		{"several usable brokers, no terminal", defaultMsg, two, false, twoErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Never prompt, whatever stdin is: autoConfirm skips the
			// interactive picker even on a TTY, and without a terminal
			// stdin is not read.
			origAutoConfirm, origIsTerminal := autoConfirm, isInteractiveTerminal
			autoConfirm = tc.autoConfirm
			// The autoConfirm cases run on a simulated TTY, the others
			// without one.
			onTTY := tc.autoConfirm
			isInteractiveTerminal = func() bool { return onTTY }
			t.Cleanup(func() { autoConfirm, isInteractiveTerminal = origAutoConfirm, origIsTerminal })
			withStdin(t, "1\ny\n")
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
			stdout, stderr := captureStdIO(t, func() {
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
			assert.NotContains(t, stderr, "Select a broker")
			assert.NotContains(t, stderr, "Use runtime broker")
			assert.NotContains(t, err.Error(), "operation cancelled")
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

// A synchronous create that the hub answers after the hub client's usual
// timeout still succeeds: the create request has its own, longer timeout.
// The timeouts are scaled down; the usual one is 50ms here, as
// hubClientTimeout (30s) is in production.
func TestCreateAgentWithBrokerResolution_SlowCreateOutlastsClientTimeout(t *testing.T) {
	require.Greater(t, hubCreateRequestTimeout, hubClientTimeout, "the create timeout must exceed the usual client timeout")
	orig := hubCreateRequestTimeout
	hubCreateRequestTimeout = 10 * time.Second
	t.Cleanup(func() { hubCreateRequestTimeout = orig })

	const projectID = "proj-slow"
	var creates atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		creates.Add(1)
		select {
		case <-time.After(300 * time.Millisecond):
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"agent": map[string]interface{}{"id": "agent-1", "slug": "slow"}})
	}))
	t.Cleanup(srv.Close)
	client, err := hubclient.New(srv.URL, hubclient.WithTimeout(50*time.Millisecond))
	require.NoError(t, err)
	hubCtx := &HubContext{Client: client, Endpoint: srv.URL, ProjectID: projectID}

	resp, err := createAgentWithBrokerResolution(context.Background(), hubCtx, projectID, &hubclient.CreateAgentRequest{Name: "slow"})
	require.NoError(t, err, "a create slower than the client timeout must not be cut off")
	require.NotNil(t, resp.Agent)
	assert.Equal(t, "agent-1", resp.Agent.ID)
	assert.Equal(t, int32(1), creates.Load(), "the create is sent once")

	// Other requests on the same client keep the usual timeout.
	_, err = client.ProjectAgents(projectID).Create(context.Background(), &hubclient.CreateAgentRequest{Name: "slow"})
	require.Error(t, err, "without the create timeout the usual client timeout applies")
}
