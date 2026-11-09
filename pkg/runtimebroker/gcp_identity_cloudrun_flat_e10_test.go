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

package runtimebroker

import (
	"context"
	"fmt"
	"io/fs"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/brokeridentity"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// The per-agent GCP identity refusal on a flat cloudrun instance (amendment
// P5-CR, CR3 credentials, E-10): every mode, on create, start and restart,
// with the hub.cloudrun_native_identity experiment on and off, is refused
// before any side effect. Each refusal row runs with broker ownership on,
// the project ID set and a fresh hub-managed project, and asserts no
// ownership record or run left (none Provisioning), no new or changed
// project file, no runtime create call and no identity resolution. The
// control rows (no request, no default) show each of those observations
// moving when the dispatch is not refused.

const (
	e10Agent       = "agent-cr-e10"
	e10AgentID     = "3b6d9f1a-2c4e-4a8b-9d0f-7e1c5a3b2d4f"
	e10ProjectID   = "8e2a4c6b-0d1f-4e3a-b5c7-9f1d3b5a7c9e"
	e10ProjectSlug = "e10-fresh"
	e10SeedRun     = "e10-seed-run"
)

// e10Mode is one per-agent GCP identity request, in its create form (the
// request's GCPIdentity) and its start/restart form (the hub's resolved env).
type e10Mode struct {
	name string
	gcp  *GCPIdentityConfig
	env  map[string]string
}

func e10Modes() []e10Mode {
	return []e10Mode{
		{
			name: "assign",
			gcp:  &GCPIdentityConfig{MetadataMode: "assign", NativeCloudRun: true, SAEmail: crAssignedSA, ProjectID: crProjectID},
			env: map[string]string{
				"SCION_METADATA_MODE":        "assign",
				"SCION_METADATA_MODE_SOURCE": "hub",
				"SCION_METADATA_SA_EMAIL":    crAssignedSA,
				"SCION_METADATA_PROJECT_ID":  crProjectID,
				cloudRunNativeMarkerEnv:      "1",
			},
		},
		{
			name: "block",
			gcp:  &GCPIdentityConfig{MetadataMode: "block"},
			env:  map[string]string{"SCION_METADATA_MODE": "block", "SCION_METADATA_MODE_SOURCE": "hub"},
		},
		{
			name: "passthrough",
			gcp:  &GCPIdentityConfig{MetadataMode: "passthrough"},
			env:  map[string]string{"SCION_METADATA_MODE": "passthrough", "SCION_METADATA_MODE_SOURCE": "hub"},
		},
		{
			name: "unset resolving to the hub-default passthrough",
			gcp:  &GCPIdentityConfig{MetadataMode: "passthrough", RequireLocalRuntime: true},
			env: map[string]string{
				"SCION_METADATA_MODE":                  "passthrough",
				"SCION_METADATA_MODE_SOURCE":           "hub",
				"SCION_METADATA_REQUIRE_LOCAL_RUNTIME": "true",
			},
		},
	}
}

var e10Ops = []startOperation{opCreate, opHTTPStart, opHTTPRestart}

// e10Server is a flat cloudrun instance with broker ownership on. runs
// counts runtime create (Run) calls.
type e10Server struct {
	srv       *Server
	runs      *int
	home      string
	globalDir string
}

func newE10Server(t *testing.T) e10Server {
	t.Helper()
	srv := newFlatCloudRunStartServer(t)
	srv.ownership = NewOwnershipStore(t.TempDir(), "flat-cr-broker")
	mr, ok := srv.runtime.(*runtime.MockRuntime)
	if !ok {
		t.Fatalf("runtime is %T, want *runtime.MockRuntime", srv.runtime)
	}
	runs := 0
	mr.RunFunc = func(context.Context, runtime.RunConfig) (string, error) {
		runs++
		return "", nil
	}
	newTestGlobalSettings(t, testCloudRunAllowListGlobalSettingsYAML)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		t.Fatal(err)
	}
	return e10Server{srv: srv, runs: &runs, home: home, globalDir: globalDir}
}

// seedLiveRecord records the agent as owned and live, as its earlier create
// would have, so a start or restart reaches the run step.
func (e e10Server) seedLiveRecord(t *testing.T) {
	t.Helper()
	if err := e.srv.ownership.BeginRun(e10ProjectID, e10AgentID, e10Agent, e10SeedRun); err != nil {
		t.Fatal(err)
	}
	if err := e.srv.ownership.SetRunState(e10ProjectID, e10AgentID, e10SeedRun, OwnershipStateCreated); err != nil {
		t.Fatal(err)
	}
}

func (e e10Server) records(t *testing.T) []OwnershipRecord {
	t.Helper()
	recs, err := e.srv.ownership.List()
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

// projectPath is where a dispatch of the fresh project would put its files.
func (e e10Server) projectPath() string {
	return config.BrokerProjectPath(e.globalDir, "", e10ProjectSlug)
}

// e10TreeSnapshot maps every path under root to its content (a directory to
// "", a link to its target), so two snapshots differ exactly when a file,
// directory or link was added, removed or changed.
func e10TreeSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	snap := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			snap[rel] = "link:" + target
		case d.IsDir():
			snap[rel+"/"] = ""
		default:
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			snap[rel] = "file:" + string(b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

// e10Inputs is a dispatch of the fresh project: a create carries mode's
// GCPIdentity (nil: no request), a start or restart the resolved env (nil:
// no request). gateOn adds the experiment to a create's config.
func e10Inputs(op startOperation, gcp *GCPIdentityConfig, env map[string]string, gateOn bool) startContextInputs {
	path := "/api/v1/agents"
	switch op {
	case opHTTPStart:
		path += "/" + e10Agent + "/start"
	case opHTTPRestart:
		path += "/" + e10Agent + "/restart"
	}
	in := startContextInputs{
		Name:        e10Agent,
		Slug:        e10Agent,
		AgentID:     e10AgentID,
		ProjectID:   e10ProjectID,
		ProjectSlug: e10ProjectSlug,
		HTTPRequest: httptest.NewRequest(http.MethodPost, path, nil),
		Operation:   op,
	}
	if op == opCreate {
		in.Config = &CreateAgentConfig{GCPIdentity: gcp}
		if gateOn {
			in.Config.HubAgentDefaults = crGateDefaults()
		}
		return in
	}
	if env != nil {
		in.ResolvedEnv = maps.Clone(env)
	}
	return in
}

func e10Ctx(gateOn bool) context.Context {
	if gateOn {
		return crGateCtx()
	}
	return context.Background()
}

func TestFlatCloudRunGCPIdentityRefusal_Text(t *testing.T) {
	if flatCloudRunGCPIdentityRefusalText != "flat Cloud Run instances do not support per-agent GCP identities" {
		t.Errorf("refusal text = %q", flatCloudRunGCPIdentityRefusalText)
	}
}

// Every mode, on create, start and restart, gate on and off: refused first,
// with no side effect of any kind.
func TestBuildStartContext_FlatCloudRunGCPIdentityRefusedFirst(t *testing.T) {
	want := fmt.Sprintf("agent %q: %s", e10Agent, flatCloudRunGCPIdentityRefusalText)
	for _, gateOn := range []bool{false, true} {
		for _, op := range e10Ops {
			for _, mode := range e10Modes() {
				t.Run(fmt.Sprintf("gate=%t/%s/%s", gateOn, op, mode.name), func(t *testing.T) {
					e := newE10Server(t)
					if op != opCreate {
						// A start or restart of a live agent: a run left
						// behind would show as a new (Provisioning) run on
						// this record.
						e.seedLiveRecord(t)
					}
					recordsBefore := e.records(t)
					homeBefore := e10TreeSnapshot(t, e.home)

					sc, err := e.srv.buildStartContext(e10Ctx(gateOn), e10Inputs(op, mode.gcp, mode.env, gateOn))

					if sc != nil {
						t.Fatal("expected a refusal, got a start context")
					}
					sce := requireStartContextError(t, err)
					if sce.Status != http.StatusBadRequest {
						t.Errorf("status = %d, want %d", sce.Status, http.StatusBadRequest)
					}
					if sce.Message != want {
						t.Errorf("message = %q, want %q", sce.Message, want)
					}
					if got := e.records(t); !reflect.DeepEqual(got, recordsBefore) {
						t.Errorf("ownership records changed:\nbefore %+v\nafter  %+v", recordsBefore, got)
					}
					if got := e10TreeSnapshot(t, e.home); !maps.Equal(got, homeBefore) {
						t.Errorf("a project file or directory was written (%d paths before, %d after)", len(homeBefore), len(got))
					}
					if _, err := os.Lstat(e.projectPath()); !os.IsNotExist(err) {
						t.Errorf("the fresh project path exists after the refusal (err %v)", err)
					}
					// Not a deciding check: buildStartContext never calls
					// Run, so this count has no positive at this level. The
					// refusal itself (no start context, a 400) and the three
					// side-effect zeros (the records, the HOME tree, the
					// identity resolution), each with its control row,
					// decide that no run was started.
					if *e.runs != 0 {
						t.Errorf("runtime create calls = %d, want 0", *e.runs)
					}
					if n := e.srv.cloudRunIdentityResolves.count(); n != 0 {
						t.Errorf("resolveCloudRunIdentity calls = %d, want 0", n)
					}
				})
			}
		}
	}
}

// The control rows: no GCP identity request and no hub default, gate off.
// The dispatch is not refused, and the observations the refusal rows hold at
// zero move: the identity resolution runs once, the run is recorded, and
// the fresh project's files are written.
func TestBuildStartContext_FlatCloudRunNoGCPIdentityRequestNotRefused(t *testing.T) {
	for _, op := range e10Ops {
		t.Run(string(op), func(t *testing.T) {
			e := newE10Server(t)
			if op != opCreate {
				e.seedLiveRecord(t)
			}
			recordsBefore := e.records(t)
			homeBefore := e10TreeSnapshot(t, e.home)

			// Whatever a later step returns, the steps checked here have
			// run by then.
			_, err := e.srv.buildStartContext(context.Background(), e10Inputs(op, nil, nil, false))

			if err != nil && strings.Contains(err.Error(), flatCloudRunGCPIdentityRefusalText) {
				t.Fatalf("a dispatch with no request was refused: %v", err)
			}
			if n := e.srv.cloudRunIdentityResolves.count(); n != 1 {
				t.Errorf("resolveCloudRunIdentity calls = %d, want 1", n)
			}
			if got := e.records(t); reflect.DeepEqual(got, recordsBefore) {
				t.Errorf("the run was not recorded: records %+v", got)
			}
			if got := e10TreeSnapshot(t, e.home); maps.Equal(got, homeBefore) {
				t.Error("no project file was written")
			}
			if _, err := os.Lstat(e.projectPath()); err != nil {
				t.Errorf("the fresh project path was not created: %v", err)
			}
		})
	}
}

// accountCount counts the request's service account in s.
func accountCount(s string) int { return strings.Count(s, crAssignedSA) }

// The refusal names the agent only: the requested account appears neither
// in the error nor in the log, on create (the request's GCPIdentity), start
// and restart (the resolved env). Positive, per operation: the same count
// over the row's own error and log with the account forced in once is 1.
func TestBuildStartContext_FlatCloudRunGCPIdentityRefusalNamesAgentOnly(t *testing.T) {
	assign := e10Modes()[0]
	for _, op := range e10Ops {
		t.Run(string(op), func(t *testing.T) {
			e := newE10Server(t)
			if op != opCreate {
				e.seedLiveRecord(t)
			}
			l := probeFlatTokenStep(e.srv)
			_, err := e.srv.buildStartContext(crGateCtx(), e10Inputs(op, assign.gcp, assign.env, true))
			sce := requireStartContextError(t, err)
			logged := l.String()
			if !strings.Contains(sce.Message, e10Agent) {
				t.Errorf("message %q does not name the agent", sce.Message)
			}
			if n := accountCount(sce.Message); n != 0 {
				t.Errorf("the error names the account %d time(s): %q", n, sce.Message)
			}
			if strings.Contains(sce.Message, crProjectID) {
				t.Errorf("the error names the project: %q", sce.Message)
			}
			if n := accountCount(logged); n != 0 {
				t.Errorf("the log names the account %d time(s)", n)
			}
			// Planted positives: the same search over this row's error and
			// log with the account forced in once finds it exactly once.
			if n := accountCount(sce.Message + " " + crAssignedSA); n != 1 {
				t.Errorf("positive control on the error: accountCount = %d, want 1", n)
			}
			if n := accountCount(logged + " " + crAssignedSA); n != 1 {
				t.Errorf("positive control on the log: accountCount = %d, want 1", n)
			}
		})
	}
}

// The refusal's two predicates, from request fields and instance
// configuration only.
func TestFlatCloudRunGCPIdentityRefusal_Predicates(t *testing.T) {
	cloudrun := config.V1RuntimeTargetConfig{Type: config.RuntimeTargetTypeCloudRun}
	docker := config.V1RuntimeTargetConfig{Type: config.RuntimeTargetTypeDocker}
	targets := []struct {
		name string
		fi   *FlatInstanceConfig
		want bool
	}{
		{name: "legacy Runtime Broker", fi: nil, want: false},
		{name: "flat docker", fi: &FlatInstanceConfig{Instance: config.V1RuntimeBrokerInstanceConfig{RuntimeTarget: &docker},
			Identity: &brokeridentity.Identity{RuntimeTarget: api.RuntimeTargetDescriptor{Type: brokeridentity.TargetTypeDocker}}}, want: false},
		{name: "flat cloudrun", fi: &FlatInstanceConfig{Instance: config.V1RuntimeBrokerInstanceConfig{RuntimeTarget: &cloudrun},
			Identity: &brokeridentity.Identity{RuntimeTarget: api.RuntimeTargetDescriptor{Type: brokeridentity.TargetTypeCloudRun}}}, want: true},
		{name: "flat cloudrun by configured target only", fi: &FlatInstanceConfig{Instance: config.V1RuntimeBrokerInstanceConfig{RuntimeTarget: &cloudrun}}, want: true},
		{name: "flat cloudrun by persisted identity only", fi: &FlatInstanceConfig{
			Identity: &brokeridentity.Identity{RuntimeTarget: api.RuntimeTargetDescriptor{Type: brokeridentity.TargetTypeCloudRun}}}, want: true},
	}
	for _, tc := range targets {
		t.Run("target/"+tc.name, func(t *testing.T) {
			s := &Server{}
			s.config.FlatInstance = tc.fi
			if got := s.flatCloudRunTarget(); got != tc.want {
				t.Errorf("flatCloudRunTarget() = %t, want %t", got, tc.want)
			}
		})
	}

	requests := []struct {
		name string
		in   startContextInputs
		want bool
	}{
		{name: "create, no request", in: startContextInputs{Operation: opCreate, Config: &CreateAgentConfig{}}, want: false},
		{name: "create, nil config", in: startContextInputs{Operation: opCreate}, want: false},
		{name: "start, no request", in: startContextInputs{Operation: opHTTPStart, ResolvedEnv: map[string]string{"SCION_AGENT_ID": e10AgentID}}, want: false},
		{name: "create, GCPIdentity block", in: startContextInputs{Operation: opCreate, Config: &CreateAgentConfig{GCPIdentity: &GCPIdentityConfig{MetadataMode: "block"}}}, want: true},
		{name: "create, mode in config env", in: startContextInputs{Operation: opCreate, Config: &CreateAgentConfig{Env: []string{"SCION_METADATA_MODE=assign"}}}, want: true},
		{name: "create, hub default in config env", in: startContextInputs{Operation: opCreate, Config: &CreateAgentConfig{Env: []string{"SCION_METADATA_REQUIRE_LOCAL_RUNTIME=true"}}}, want: true},
		{name: "start, mode in resolved env", in: startContextInputs{Operation: opHTTPStart, ResolvedEnv: map[string]string{"SCION_METADATA_MODE": "block"}}, want: true},
		{name: "restart, mode in resolved env", in: startContextInputs{Operation: opHTTPRestart, ResolvedEnv: map[string]string{"SCION_METADATA_MODE": "block"}}, want: true},
		{name: "start, hub default in resolved env", in: startContextInputs{Operation: opHTTPStart, ResolvedEnv: map[string]string{"SCION_METADATA_REQUIRE_LOCAL_RUNTIME": "true"}}, want: true},
	}
	for _, tc := range requests {
		t.Run("request/"+tc.name, func(t *testing.T) {
			if got := perAgentGCPIdentityRequested(tc.in); got != tc.want {
				t.Errorf("perAgentGCPIdentityRequested() = %t, want %t", got, tc.want)
			}
		})
	}
}
