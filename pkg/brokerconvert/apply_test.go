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

//go:build !windows

package brokerconvert

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	yamlv3 "gopkg.in/yaml.v3"

	"github.com/GoogleCloudPlatform/scion/internal/writesonread"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

const (
	applyLegacyID   = "b-legacy"
	applyInstanceID = "b-flat-1"
	plantedApplyEnv = "PLANTED-APPLY-ENV-c51"
)

// applyHub is a stateful fake Hub for apply, status and rollback: the
// legacy row, the instance row once registered, projects with providers
// and defaults, and agents per Runtime Broker. Every write is recorded.
type applyHub struct {
	*httptest.Server
	t *testing.T

	mu          sync.Mutex
	writes      []string
	legacy      map[string]any
	instance    map[string]any // nil until registered
	maxAgents   any            // the legacy row's maxAgents setting
	canUpdate   bool           // the caller's settings capability
	settingsErr int            // a status for the legacy settings read
	instMax     *int64         // the instance row's maxAgents setting
	instRev     int64
	puts        int
	agents      map[string][]map[string]any
	projects    map[string]*applyProject
	brokers     []map[string]any // other rows, for the name check
	failNext    map[string]int   // "METHOD path" -> status, once
	order       []string
}

type applyProject struct {
	name          string
	defaultID     string
	activeProfile string
	providers     map[string]string // broker ID -> local path
}

func newApplyHub(t *testing.T) *applyHub {
	t.Helper()
	h := &applyHub{
		t:         t,
		legacy:    map[string]any{"id": applyLegacyID, "name": "build-host", "status": "offline", "profiles": []map[string]any{{"name": "build", "type": "docker"}}},
		maxAgents: 0,
		canUpdate: true,
		agents:    map[string][]map[string]any{},
		projects: map[string]*applyProject{
			"p-1": {name: "web", defaultID: applyLegacyID, activeProfile: "build", providers: map[string]string{applyLegacyID: ""}},
			"p-2": {name: "api", defaultID: "b-other", providers: map[string]string{applyLegacyID: "", "b-other": ""}},
		},
		failNext: map[string]int{},
	}
	h.Server = httptest.NewServer(http.HandlerFunc(h.serve))
	t.Cleanup(h.Close)
	return h
}

func (h *applyHub) serve(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	key := r.Method + " " + r.URL.Path
	if r.Method != http.MethodGet {
		h.writes = append(h.writes, key)
	}
	if st, ok := h.failNext[key]; ok {
		delete(h.failNext, key)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(st)
		_, _ = w.Write([]byte(`{"error":{"code":"x","message":"refused with secret-looking text PLANTED-HUB-MSG"}}`))
		return
	}
	reply := func(body any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}
	notFound := func() {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"not found"}}`))
	}
	p := r.URL.Path
	const rb = "/api/v1/runtime-brokers"
	switch {
	case p == rb && r.Method == http.MethodGet:
		name := r.URL.Query().Get("name")
		var out []map[string]any
		for _, b := range h.brokers {
			if name == "" || strings.EqualFold(b["name"].(string), name) {
				out = append(out, b)
			}
		}
		if h.instance != nil && (name == "" || strings.EqualFold(h.instance["name"].(string), name)) {
			out = append(out, h.instance)
		}
		reply(map[string]any{"brokers": out})
	case p == rb+"/"+applyLegacyID && r.Method == http.MethodGet:
		reply(h.legacy)
	case p == rb+"/"+applyLegacyID+"/settings":
		if h.settingsErr != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(h.settingsErr)
			_, _ = w.Write([]byte(`{"error":{"code":"forbidden","message":"no"}}`))
			return
		}
		reply(map[string]any{
			"brokerId":      applyLegacyID,
			"settings":      map[string]any{"maxAgents": h.maxAgents},
			"effective":     map[string]any{"maxAgents": map[string]any{"value": h.maxAgents, "source": "broker", "count": 0, "inherited": map[string]any{"value": 25, "source": "default"}}},
			"_capabilities": map[string]any{"update": h.canUpdate},
		})
	case p == rb+"/"+applyInstanceID+"/settings" && r.Method == http.MethodGet:
		settings := map[string]any{}
		if h.instMax != nil {
			settings["maxAgents"] = *h.instMax
		}
		reply(map[string]any{"brokerId": applyInstanceID, "settings": settings, "revision": h.instRev, "_capabilities": map[string]any{"update": h.canUpdate}})
	case p == rb+"/"+applyInstanceID+"/settings" && r.Method == http.MethodPut:
		var body struct {
			Settings struct {
				MaxAgents *int64 `json:"maxAgents"`
			} `json:"settings"`
			ExpectedRevision int64 `json:"expectedRevision"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.ExpectedRevision != h.instRev {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"revision_conflict","message":"stale"}`))
			return
		}
		h.puts++
		h.instMax = body.Settings.MaxAgents
		h.instRev++
		h.order = append(h.order, "capacity")
		reply(map[string]any{"brokerId": applyInstanceID, "settings": map[string]any{"maxAgents": h.instMax}, "revision": h.instRev})
	case p == rb+"/"+applyLegacyID+"/projects":
		var ps []map[string]any
		for _, id := range h.projectIDs() {
			if _, ok := h.projects[id].providers[applyLegacyID]; ok {
				ps = append(ps, map[string]any{"projectId": id, "projectName": h.projects[id].name})
			}
		}
		reply(map[string]any{"projects": ps})
	case p == rb+"/"+applyInstanceID && r.Method == http.MethodGet:
		if h.instance == nil {
			notFound()
			return
		}
		reply(h.instance)
	case p == rb+"/"+applyInstanceID && r.Method == http.MethodDelete:
		if h.instance == nil {
			notFound()
			return
		}
		h.instance = nil
		h.instMax, h.instRev = nil, 0
		for _, pr := range h.projects {
			delete(pr.providers, applyInstanceID)
		}
		h.order = append(h.order, "deregister")
		w.WriteHeader(http.StatusNoContent)
	case p == "/api/v1/agents":
		reply(map[string]any{"agents": h.agents[r.URL.Query().Get("runtimeBrokerId")]})
	case strings.HasPrefix(p, "/api/v1/projects/"):
		h.serveProject(w, r, strings.Split(strings.TrimPrefix(p, "/api/v1/projects/"), "/"), reply, notFound)
	default:
		h.t.Errorf("unexpected request %s", key)
		notFound()
	}
}

func (h *applyHub) serveProject(w http.ResponseWriter, r *http.Request, parts []string, reply func(any), notFound func()) {
	pr := h.projects[parts[0]]
	if pr == nil {
		notFound()
		return
	}
	switch {
	case len(parts) == 1 && r.Method == http.MethodGet:
		reply(map[string]any{"id": parts[0], "name": pr.name, "defaultRuntimeBrokerId": pr.defaultID})
	case len(parts) == 1:
		var body struct {
			DefaultRuntimeBrokerID string `json:"defaultRuntimeBrokerId"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.DefaultRuntimeBrokerID != "" {
			pr.defaultID = body.DefaultRuntimeBrokerID
		}
		h.order = append(h.order, "default:"+parts[0])
		reply(map[string]any{"id": parts[0], "defaultRuntimeBrokerId": pr.defaultID})
	case len(parts) == 2 && parts[1] == "settings":
		body := map[string]any{}
		if pr.activeProfile != "" {
			body["activeProfile"] = pr.activeProfile
		}
		reply(body)
	case len(parts) == 2 && parts[1] == "providers" && r.Method == http.MethodGet:
		var out []map[string]any
		ids := make([]string, 0, len(pr.providers))
		for id := range pr.providers {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			out = append(out, map[string]any{"brokerId": id, "localPath": pr.providers[id]})
		}
		reply(map[string]any{"providers": out})
	case len(parts) == 2 && parts[1] == "providers" && r.Method == http.MethodPost:
		var body struct {
			BrokerID string `json:"brokerId"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		pr.providers[body.BrokerID] = ""
		h.order = append(h.order, "link:"+parts[0])
		reply(map[string]any{"provider": map[string]any{"brokerId": body.BrokerID}})
	case len(parts) == 3 && parts[1] == "providers" && r.Method == http.MethodDelete:
		if _, ok := pr.providers[parts[2]]; !ok {
			notFound()
			return
		}
		delete(pr.providers, parts[2])
		h.order = append(h.order, "unlink:"+parts[0])
		w.WriteHeader(http.StatusNoContent)
	default:
		h.t.Errorf("unexpected project request %s %v", r.Method, parts)
		notFound()
	}
}

func (h *applyHub) projectIDs() []string {
	ids := make([]string, 0, len(h.projects))
	for id := range h.projects {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (h *applyHub) client(t *testing.T) hubclient.Client {
	t.Helper()
	c, err := hubclient.New(h.URL)
	require.NoError(t, err)
	return c
}

func (h *applyHub) writeCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.writes)
}

// applyFixture is a temp global dir holding the legacy settings, an env
// wired to it and to the fake Hub, and a fake registration.
type applyFixture struct {
	t        *testing.T
	g        string
	hub      *applyHub
	env      *Env
	regErr   error
	regCalls int
	running  bool
	in       Input
}

func newApplyFixture(t *testing.T) *applyFixture {
	t.Helper()
	g := t.TempDir()
	raw, err := os.ReadFile(filepath.Join("testdata", "apply_one_docker.yaml"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(g, "settings.yaml"), raw, 0o640))
	require.NoError(t, os.Chmod(filepath.Join(g, "settings.yaml"), 0o640))

	f := &applyFixture{t: t, g: g, hub: newApplyHub(t)}
	f.env = &Env{
		GlobalDir: g,
		Client:    f.hub.client(t),
		Register: func(ctx context.Context, inst config.V1RuntimeBrokerInstanceConfig) (string, string, error) {
			f.regCalls++
			if f.regErr != nil {
				return "", "", f.regErr
			}
			// The identity dir LoadOrCreate would mint, owner-only.
			dir := filepath.Join(g, "runtime-brokers", inst.Key)
			require.NoError(t, os.MkdirAll(dir, 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "identity.json"), []byte(`{"runtimeBrokerId":"`+applyInstanceID+`"}`), 0o600))
			f.hub.mu.Lock()
			f.hub.instance = map[string]any{"id": applyInstanceID, "name": inst.Name, "status": "offline"}
			f.hub.mu.Unlock()
			return applyInstanceID, "rt-1", nil
		},
		ServerRunning: func(context.Context) bool { return f.running },
		LoadInstances: func() ([]config.V1RuntimeBrokerInstanceConfig, error) {
			data, err := os.ReadFile(filepath.Join(g, "settings.yaml"))
			if err != nil {
				return nil, err
			}
			var vs config.VersionedSettings
			if err := yamlv3.Unmarshal(data, &vs); err != nil {
				return nil, err
			}
			if vs.Server == nil || vs.Server.Broker == nil {
				return nil, nil
			}
			return vs.Server.Broker.Instances, nil
		},
		ReadSettings:    func() ([]byte, error) { return os.ReadFile(filepath.Join(g, "settings.yaml")) },
		WriteInstances:  func(i []config.V1RuntimeBrokerInstanceConfig) error { return config.WriteRuntimeBrokerInstances(g, i) },
		RemoveInstances: func(e []config.V1RuntimeBrokerInstanceConfig) error { return config.RemoveRuntimeBrokerInstances(g, e) },
		Now:             func() time.Time { return testNow },
	}
	return f
}

// plan builds the plan as the command does, from the current settings.
func (f *applyFixture) plan() (*Plan, ApplyInput) {
	f.t.Helper()
	raw, err := os.ReadFile(filepath.Join(f.g, "settings.yaml"))
	require.NoError(f.t, err)
	vs := &config.VersionedSettings{}
	require.NoError(f.t, yamlv3.Unmarshal(raw, vs))
	f.in = Input{Settings: vs, RawSettings: raw}
	p, err := BuildLocal(f.in)
	require.NoError(f.t, err)
	require.NoError(f.t, CollectHub(context.Background(), f.env.Client, applyLegacyID, p))
	return p, ApplyInput{Plan: p, LegacyDefaultProfile: vs.ActiveProfile}
}

// instanceKey is the fixture's one planned instance key, read from the plan
// the command would build (the converter names it after its target).
func (f *applyFixture) instanceKey() string {
	f.t.Helper()
	p, _ := f.plan()
	insts := p.SettingsInstances()
	require.Len(f.t, insts, 1)
	return insts[0].Key
}

// resumeInput rebuilds the plan of a run whose step 2 wrote the settings,
// as the command does: the settings without the instances key, and the
// evidence copy as the raw bytes.
func (f *applyFixture) resumeInput() ApplyInput {
	f.t.Helper()
	j, err := LoadJournal(f.g)
	require.NoError(f.t, err)
	ev, err := ReadRunCopy(f.g, j.EvidenceFile)
	require.NoError(f.t, err)
	raw, err := os.ReadFile(filepath.Join(f.g, "settings.yaml"))
	require.NoError(f.t, err)
	vs := &config.VersionedSettings{}
	require.NoError(f.t, yamlv3.Unmarshal(raw, vs))
	if vs.Server != nil && vs.Server.Broker != nil {
		vs.Server.Broker.Instances = nil
	}
	p, err := BuildLocal(Input{Settings: vs, RawSettings: ev})
	require.NoError(f.t, err)
	require.NoError(f.t, CollectHub(context.Background(), f.env.Client, applyLegacyID, p))
	return ApplyInput{Plan: p, LegacyDefaultProfile: vs.ActiveProfile}
}

func (f *applyFixture) apply() (*Report, error) {
	f.t.Helper()
	_, in := f.plan()
	return Apply(context.Background(), f.env, in)
}

func settingsBytes(t *testing.T, g string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(g, "settings.yaml"))
	require.NoError(t, err)
	return b
}

func refusalCode(t *testing.T, err error) string {
	t.Helper()
	var ref *Refusal
	require.True(t, errors.As(err, &ref), "a refusal, got %v", err)
	return ref.Code
}

// AC1: a drained legacy host with a clean plan ends with the row, the
// settings, the link and the default; a second run is a no-op.
func TestApply_ConvertsAndSecondRunIsNoop(t *testing.T) {
	f := newApplyFixture(t)
	key := f.instanceKey() // before apply: once the settings are flat the plan is rebuilt differently
	rep, err := f.apply()
	require.NoError(t, err)
	assert.Equal(t, JournalApplied, rep.State)
	assert.Contains(t, rep.Notes, NoteEvidenceKept)
	assert.Contains(t, rep.Notes, NoteSwitch)

	insts, err := f.env.LoadInstances()
	require.NoError(t, err)
	require.Len(t, insts, 1)
	// The one pinned literal: the converter names the docker-target
	// instance after its target (profile_conversion.go), not the profile.
	assert.Equal(t, "docker", insts[0].Key)
	assert.Equal(t, key, insts[0].Key)

	p1, p2 := f.hub.projects["p-1"], f.hub.projects["p-2"]
	assert.Contains(t, p1.providers, applyInstanceID)
	assert.Contains(t, p1.providers, applyLegacyID, "the legacy link is kept")
	assert.Equal(t, applyInstanceID, p1.defaultID, "a default on the legacy row is re-pointed to the active profile's instance")
	assert.Contains(t, p2.providers, applyInstanceID)
	assert.Equal(t, "b-other", p2.defaultID, "a default elsewhere is left alone")

	// The second run rebuilds the plan as the command does (settings
	// without the instances key, plus the evidence copy) and is a no-op.
	writes := f.hub.writeCount()
	rep2, err := Apply(context.Background(), f.env, f.resumeInput())
	require.NoError(t, err)
	assert.Contains(t, rep2.Notes, NoteNoop)
	assert.Equal(t, writes, f.hub.writeCount(), "no Hub write on the second run")
	assert.Equal(t, 1, f.regCalls)
}

// Named mutant target: drop the drained re-check before step 3.
func TestApply_AgentAppearsBeforeStep3_Refuses(t *testing.T) {
	f := newApplyFixture(t)
	f.env.WriteInstances = func(i []config.V1RuntimeBrokerInstanceConfig) error {
		if err := config.WriteRuntimeBrokerInstances(f.g, i); err != nil {
			return err
		}
		// A new agent lands on the legacy row during the window.
		f.hub.mu.Lock()
		f.hub.agents[applyLegacyID] = []map[string]any{{"id": "a-new", "name": "late", "projectId": "p-1", "project": "web", "phase": "created"}}
		f.hub.mu.Unlock()
		return nil
	}
	_, err := f.apply()
	require.Error(t, err)
	assert.Equal(t, RefuseNotDrained, refusalCode(t, err))
	var ref *Refusal
	require.True(t, errors.As(err, &ref))
	require.Len(t, ref.Agents, 1)
	assert.Equal(t, "a-new", ref.Agents[0].ID)
	assert.NotContains(t, f.hub.projects["p-1"].providers, applyInstanceID, "step 3 did not run")

	j, err := LoadJournal(f.g)
	require.NoError(t, err)
	assert.Equal(t, StepDone, j.Settings.State)
	assert.False(t, j.HasStarted(), "the journal is resumable, not mid-step")
}

// AC4: refusals with no write.
func TestApply_RefusesWithNoWrite(t *testing.T) {
	cases := []struct {
		name  string
		setup func(f *applyFixture)
		code  string
	}{
		{"non-terminal agent on the legacy row", func(f *applyFixture) {
			f.hub.agents[applyLegacyID] = []map[string]any{{"id": "a-1", "name": "x", "phase": "running"}}
		}, RefuseNotDrained},
		{"server running", func(f *applyFixture) { f.running = true }, RefuseServerRunning},
		{"legacy row online with a recent heartbeat", func(f *applyFixture) {
			f.hub.legacy["status"] = "online"
			f.hub.legacy["lastHeartbeat"] = testNow.Add(-30 * time.Second).Format(time.RFC3339)
		}, RefuseServerRunning},
		{"instance name collision", func(f *applyFixture) {
			key := f.instanceKey()
			f.hub.brokers = []map[string]any{{"id": "b-x", "name": key, "slug": key}}
		}, RefuseNameConflict},
		{"local path link", func(f *applyFixture) { f.hub.projects["p-2"].providers[applyLegacyID] = "/srv/api" }, RefuseLocalPath},
		{"capacity write not permitted", func(f *applyFixture) { f.hub.canUpdate = false }, RefuseCapacityPermission},
		{"capacity not readable", func(f *applyFixture) { f.hub.settingsErr = http.StatusForbidden }, RefuseCapacityUnknown},
		{"capacity inherited: decision required", func(f *applyFixture) { f.hub.maxAgents = nil }, RefusePlanNotClean},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newApplyFixture(t)
			tc.setup(f)
			before := settingsBytes(t, f.g)
			_, err := f.apply()
			require.Error(t, err)
			assert.Equal(t, tc.code, refusalCode(t, err))
			assert.Zero(t, f.hub.writeCount(), "no Hub write")
			assert.Zero(t, f.regCalls)
			assert.Equal(t, before, settingsBytes(t, f.g), "settings untouched")
			_, err = os.Lstat(filepath.Join(f.g, "runtime-brokers", f.instanceKey()))
			assert.True(t, errors.Is(err, fs.ErrNotExist), "no identity minted")
		})
	}
}

// AC4: with the Hub experiment off, step 1 refuses and settings stay
// byte-identical.
func TestApply_ExperimentOffRefusesAtStep1(t *testing.T) {
	f := newApplyFixture(t)
	before := settingsBytes(t, f.g)
	f.regErr = errors.New("412 experiment_disabled PLANTED-HUB-MSG")
	_, err := f.apply()
	require.Error(t, err)
	assert.Equal(t, RefuseRegister, refusalCode(t, err))
	assert.Equal(t, before, settingsBytes(t, f.g))
	assert.NotContains(t, err.Error(), "PLANTED-HUB-MSG", "the registration error text is not copied")
	j, err := LoadJournal(f.g)
	require.NoError(t, err)
	assert.Equal(t, StepStarted, j.Register[0].State)
	assert.NotContains(t, j.Register[0].Error, "PLANTED")
}

// AC4: a changed plan on resume is refused.
func TestApply_ChangedPlanOnResumeRefuses(t *testing.T) {
	f := newApplyFixture(t)
	f.regErr = errors.New("transient")
	_, err := f.apply()
	require.Error(t, err)

	// The operator edits the profile: a different plan.
	data := strings.Replace(string(settingsBytes(t, f.g)), "API_TOKEN:", "OTHER_KEY:", 1)
	require.NoError(t, os.WriteFile(filepath.Join(f.g, "settings.yaml"), []byte(data), 0o640))
	f.regErr = nil
	_, err = f.apply()
	require.Error(t, err)
	assert.Equal(t, RefusePlanChanged, refusalCode(t, err))
}

// Resume after a step 3 failure completes with no duplicate link.
func TestApply_ResumeAfterStep3Failure(t *testing.T) {
	f := newApplyFixture(t)
	f.hub.failNext["POST /api/v1/projects/p-2/providers"] = http.StatusInternalServerError
	_, err := f.apply()
	require.Error(t, err)
	assert.Equal(t, RefuseHubRequest, refusalCode(t, err))

	// The settings are now flat; the command rebuilds the plan from them
	// without the instances key and the evidence copy.
	rep, err := Apply(context.Background(), f.env, f.resumeInput())
	require.NoError(t, err)
	assert.Equal(t, JournalApplied, rep.State)
	assert.Equal(t, 1, f.regCalls, "registration is not repeated")
	links := 0
	for _, o := range f.hub.order {
		if o == "link:p-1" {
			links++
		}
	}
	assert.Equal(t, 1, links, "no duplicate link")
}

// AC3: rollback before retirement.
func TestRollback_RestoresLegacyHost(t *testing.T) {
	f := newApplyFixture(t)
	key := f.instanceKey()
	before := settingsBytes(t, f.g)
	_, err := f.apply()
	require.NoError(t, err)

	// An unrelated settings change after apply survives the rollback.
	data := strings.Replace(string(settingsBytes(t, f.g)), "active_profile: build", "active_profile: build\ndefault_template: chosen-later", 1)
	require.NoError(t, os.WriteFile(filepath.Join(f.g, "settings.yaml"), []byte(data), 0o640))

	rep, err := Rollback(context.Background(), f.env)
	require.NoError(t, err)
	assert.Equal(t, JournalRolledBack, rep.State)

	after := string(settingsBytes(t, f.g))
	assert.NotContains(t, after, "instances")
	assert.Contains(t, after, "default_template: chosen-later")
	assert.Equal(t, strings.Replace(string(before), "active_profile: build", "active_profile: build\ndefault_template: chosen-later", 1), after,
		"every other byte is as before apply")
	fi, err := os.Lstat(filepath.Join(f.g, "settings.yaml"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o640), fi.Mode().Perm(), "the settings file keeps its mode")

	p1 := f.hub.projects["p-1"]
	assert.Equal(t, applyLegacyID, p1.defaultID, "the legacy default is restored")
	assert.NotContains(t, p1.providers, applyInstanceID)
	assert.Contains(t, p1.providers, applyLegacyID)
	assert.Nil(t, f.hub.instance, "the instance row is deregistered")
	// Order: the row is deregistered before its identity is moved aside.
	moved := filepath.Join(ConvertDir(f.g), RolledBackDirName, rep.RunID, key)
	fi, err = os.Lstat(moved)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), fi.Mode().Perm())
	fi, err = os.Lstat(filepath.Join(moved, "identity.json"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
	_, err = os.Lstat(filepath.Join(f.g, "runtime-brokers", key))
	assert.True(t, errors.Is(err, fs.ErrNotExist))

	// The server start check passes after a rollback.
	s, err := AcquireServerStartLock(f.g)
	require.NoError(t, err)
	s.Release()
}

// Required fixed text (a): rollback refuses when the instances key changed
// since apply, naming the instance key.
func TestRollback_RefusesChangedInstances(t *testing.T) {
	f := newApplyFixture(t)
	key := f.instanceKey()
	_, err := f.apply()
	require.NoError(t, err)
	// Change the env value inside server.broker.instances (after its
	// "instances:" line), not the legacy profile's copy above it.
	data := string(settingsBytes(t, f.g))
	at := strings.Index(data, "instances:")
	require.Greater(t, at, 0, "apply wrote server.broker.instances")
	data = data[:at] + strings.Replace(data[at:], plantedApplyEnv, "PLANTED-CHANGED-VALUE", 1)
	require.NoError(t, os.WriteFile(filepath.Join(f.g, "settings.yaml"), []byte(data), 0o640))
	got, err := f.env.LoadInstances()
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.NotEmpty(t, got[0].AgentDefaults)
	assert.Equal(t, "PLANTED-CHANGED-VALUE", got[0].AgentDefaults.HarnessOverrides["claude"].Env["API_TOKEN"],
		"the edit landed in server.broker.instances")

	_, err = Rollback(context.Background(), f.env)
	require.Error(t, err)
	assert.Equal(t, RefuseSettingsChanged, refusalCode(t, err))
	assert.Contains(t, err.Error(), "changed since apply")
	assert.Contains(t, err.Error(), key)
	assert.NotContains(t, err.Error(), "PLANTED")
}

// registerThenFail is a Register that creates the row and the identity (as
// the real one does before join) and then fails.
func registerThenFail(f *applyFixture) func(context.Context, config.V1RuntimeBrokerInstanceConfig) (string, string, error) {
	ok := f.env.Register
	return func(ctx context.Context, inst config.V1RuntimeBrokerInstanceConfig) (string, string, error) {
		_, _, err := ok(ctx, inst)
		require.NoError(f.t, err)
		return "", "", errors.New("join failed PLANTED-HUB-MSG")
	}
}

// B2: registration failed after the Hub created the row. Apply records the
// row's ID from the identity file; rollback deregisters it before the move,
// and a fresh apply then succeeds.
func TestRollback_RegisterFailedAfterRowCreated_Deregisters(t *testing.T) {
	f := newApplyFixture(t)
	key := f.instanceKey()
	okRegister := f.env.Register
	f.env.Register = registerThenFail(f)
	_, err := f.apply()
	require.Error(t, err)
	assert.Equal(t, RefuseRegister, refusalCode(t, err))
	j, err := LoadJournal(f.g)
	require.NoError(t, err)
	assert.Equal(t, StepStarted, j.Register[0].State)
	assert.Equal(t, applyInstanceID, j.Register[0].BrokerID, "the ID is recorded from the identity file")
	require.NotNil(t, f.hub.instance, "the row exists")

	rep, err := Rollback(context.Background(), f.env)
	require.NoError(t, err)
	assert.Equal(t, JournalRolledBack, rep.State)
	assert.Nil(t, f.hub.instance, "the row is deregistered")
	assert.Contains(t, f.hub.order, "deregister")
	_, err = os.Lstat(filepath.Join(ConvertDir(f.g), RolledBackDirName, rep.RunID, key, "identity.json"))
	require.NoError(t, err, "the identity moved aside after the deregister")

	f.env.Register = okRegister
	rep, err = f.apply()
	require.NoError(t, err, "a fresh apply finds no orphan row")
	assert.Equal(t, JournalApplied, rep.State)
}

// B2 (crash form): a register item left started with no ID (the run was cut
// short after the Hub created the row) is named from the identity file at
// rollback, and the row is deregistered.
func TestRollback_StartedItemWithoutIDIsNamedFromIdentity(t *testing.T) {
	f := newApplyFixture(t)
	f.env.Register = registerThenFail(f)
	_, err := f.apply()
	require.Error(t, err)
	j, err := LoadJournal(f.g)
	require.NoError(t, err)
	j.Register[0].BrokerID = "" // the crash: the ID was never saved
	require.NoError(t, SaveJournal(f.g, j, testNow))
	require.NotNil(t, f.hub.instance)

	_, err = Rollback(context.Background(), f.env)
	require.NoError(t, err)
	assert.Nil(t, f.hub.instance, "no row is left")
}

// B2 (sec): a deregister failure other than 404 stops the rollback before
// the move, so the credentials are never moved aside while the row lives.
func TestRollback_DeregisterFailureKeepsDirectory(t *testing.T) {
	f := newApplyFixture(t)
	key := f.instanceKey()
	f.env.Register = registerThenFail(f)
	_, err := f.apply()
	require.Error(t, err)
	f.hub.failNext["DELETE /api/v1/runtime-brokers/"+applyInstanceID] = http.StatusInternalServerError

	_, err = Rollback(context.Background(), f.env)
	require.Error(t, err)
	assert.Equal(t, RefuseHubRequest, refusalCode(t, err))
	fi, err := os.Lstat(filepath.Join(f.g, "runtime-brokers", key, "identity.json"))
	require.NoError(t, err, "the directory stays in place")
	assert.True(t, fi.Mode().IsRegular())
	j, err := LoadJournal(f.g)
	require.NoError(t, err)
	require.Len(t, j.Rollback.Moved, 1)
	assert.Equal(t, StepPending, j.Rollback.Moved[0].State, "the move did not start")
}

// B2 (sec): a started registration with no ID and no identity file is
// reported (the key only), not skipped silently.
func TestRollback_ReportsStartedItemWithoutIdentity(t *testing.T) {
	f := newApplyFixture(t)
	key := f.instanceKey()
	f.regErr = errors.New("refused before any identity")
	_, err := f.apply()
	require.Error(t, err)

	rep, err := Rollback(context.Background(), f.env)
	require.NoError(t, err)
	assert.Contains(t, rep.Notes, NoteNoIdentity+" "+key)
}

// D2: an identity file that cannot be read (here, not valid JSON) refuses
// the rollback with its own code, naming the instance key only, before
// anything is done.
func TestRollback_UnreadableIdentityRefusesWithKey(t *testing.T) {
	f := newApplyFixture(t)
	key := f.instanceKey()
	f.env.Register = registerThenFail(f)
	_, err := f.apply()
	require.Error(t, err)
	j, err := LoadJournal(f.g)
	require.NoError(t, err)
	j.Register[0].BrokerID = ""
	require.NoError(t, SaveJournal(f.g, j, testNow))
	require.NoError(t, os.WriteFile(filepath.Join(f.g, "runtime-brokers", key, "identity.json"), []byte("not json"), 0o600))

	_, err = Rollback(context.Background(), f.env)
	require.Error(t, err)
	assert.Equal(t, RefuseIdentityUnreadable, refusalCode(t, err))
	var ref *Refusal
	require.ErrorAs(t, err, &ref)
	assert.Equal(t, []string{key}, ref.Keys)
	assert.NotNil(t, f.hub.instance, "nothing was deregistered")
	j, err = LoadJournal(f.g)
	require.NoError(t, err)
	assert.Nil(t, j.Rollback, "the rollback did not start")
}

// D1: a BeforeMove error stops the rollback before that instance's move:
// the row is deregistered, the directory stays in place and the move item
// stays pending. A later rollback, without the error, resumes and finishes.
func TestRollback_BeforeMoveErrorStopsBeforeMove(t *testing.T) {
	f := newApplyFixture(t)
	key := f.instanceKey()
	_, err := f.apply()
	require.NoError(t, err)
	var calls []string
	f.env.BeforeMove = func(globalDir, k string) error {
		calls = append(calls, k)
		assert.Equal(t, f.g, globalDir)
		assert.Nil(t, f.hub.instance, "the row is deregistered before the hook")
		return errors.New("PLANTED-HOOK-ERROR")
	}

	_, err = Rollback(context.Background(), f.env)
	require.Error(t, err)
	assert.Equal(t, RefuseLocalWrite, refusalCode(t, err))
	assert.NotContains(t, err.Error(), "PLANTED-HOOK-ERROR", "the refusal text is fixed")
	assert.Equal(t, []string{key}, calls)
	fi, err := os.Lstat(filepath.Join(f.g, "runtime-brokers", key, "identity.json"))
	require.NoError(t, err, "the directory stays in place")
	assert.True(t, fi.Mode().IsRegular())
	j, err := LoadJournal(f.g)
	require.NoError(t, err)
	require.Len(t, j.Rollback.Moved, 1)
	assert.Equal(t, StepPending, j.Rollback.Moved[0].State, "the move did not start")
	assert.NotEmpty(t, j.Rollback.Moved[0].Error)

	f.env.BeforeMove = nil
	rep, err := Rollback(context.Background(), f.env)
	require.NoError(t, err, "the rollback resumes")
	assert.Equal(t, JournalRolledBack, rep.State)
}

// B3: a resume after a step 1 failure with a rotated env value (the plan
// digest leaves values out) writes the new list, and the instances copy
// follows it, so rollback succeeds.
func TestRollback_AfterResumeWithRotatedEnvValue(t *testing.T) {
	f := newApplyFixture(t)
	f.regErr = errors.New("transient")
	_, err := f.apply()
	require.Error(t, err)
	data := strings.Replace(string(settingsBytes(t, f.g)), plantedApplyEnv, "PLANTED-ROTATED-VALUE", 1)
	require.NoError(t, os.WriteFile(filepath.Join(f.g, "settings.yaml"), []byte(data), 0o640))
	f.regErr = nil
	rep, err := f.apply()
	require.NoError(t, err)
	assert.Equal(t, JournalApplied, rep.State)

	rep, err = Rollback(context.Background(), f.env)
	require.NoError(t, err, "the instances copy equals what step 2 wrote")
	assert.Equal(t, JournalRolledBack, rep.State)
}

func TestRollback_RefusesWhileInstanceHasAgent(t *testing.T) {
	f := newApplyFixture(t)
	_, err := f.apply()
	require.NoError(t, err)
	f.hub.agents[applyInstanceID] = []map[string]any{{"id": "a-9", "name": "flat-agent", "phase": "running"}}
	writes := f.hub.writeCount()
	_, err = Rollback(context.Background(), f.env)
	require.Error(t, err)
	assert.Equal(t, RefuseInstanceDrained, refusalCode(t, err))
	assert.Equal(t, writes, f.hub.writeCount())
}

// Named mutant target (status), and sec R2 / AC9: no planted settings
// value appears in the apply, status or rollback output (human or JSON),
// nor in the journal.
func TestStatus_NoPlantedEnvValue(t *testing.T) {
	f := newApplyFixture(t)
	applyRep, err := f.apply()
	require.NoError(t, err)
	statusRep, err := Status(context.Background(), f.env)
	require.NoError(t, err)
	assert.Equal(t, JournalApplied, statusRep.State)
	require.Len(t, statusRep.Instances, 1)
	require.NotNil(t, statusRep.Instances[0].Online)
	rollbackRep, err := Rollback(context.Background(), f.env)
	require.NoError(t, err)

	for _, rep := range []*Report{applyRep, statusRep, rollbackRep} {
		var h, j bytes.Buffer
		require.NoError(t, WriteReportHuman(&h, rep))
		require.NoError(t, WriteReportJSON(&j, rep))
		assert.NotContains(t, h.String(), plantedApplyEnv, rep.Mode)
		assert.NotContains(t, j.String(), plantedApplyEnv, rep.Mode)
		assert.NotContains(t, h.String(), "API_TOKEN", rep.Mode)
	}
	journal, err := os.ReadFile(filepath.Join(ConvertDir(f.g), JournalFileName))
	require.NoError(t, err)
	assert.NotContains(t, string(journal), plantedApplyEnv)
	assert.NotContains(t, string(journal), "API_TOKEN")
}

func TestStatus_IsReadOnly(t *testing.T) {
	f := newApplyFixture(t)
	_, err := f.apply()
	require.NoError(t, err)
	before := listTree(t, f.g)
	writes := f.hub.writeCount()
	_, err = Status(context.Background(), f.env)
	require.NoError(t, err)
	assert.Equal(t, before, listTree(t, f.g), "status writes nothing locally")
	assert.Equal(t, writes, f.hub.writeCount(), "status sends no Hub write")
}

// The strict two-way path test (erratum E1): apply and rollback write only
// the settings file, the settings lock, the convert directory and the
// identity directory, and change nothing else.
func TestApplyRollback_WritesOnlyExpectedPaths(t *testing.T) {
	f := newApplyFixture(t)
	key := f.instanceKey()
	require.NoError(t, os.WriteFile(filepath.Join(f.g, "unrelated.txt"), []byte("x"), 0o600))
	before := listTree(t, f.g)
	_, err := f.apply()
	require.NoError(t, err)
	_, err = Rollback(context.Background(), f.env)
	require.NoError(t, err)
	after := listTree(t, f.g)

	expected := func(rel string) bool {
		switch {
		case rel == "settings.yaml", rel == ".settings.lock", rel == "runtime-brokers":
			return true
		case strings.HasPrefix(rel, "runtime-brokers/.convert"):
			return true
		case rel == "runtime-brokers/"+key || strings.HasPrefix(rel, "runtime-brokers/"+key+"/"):
			return true
		}
		return false
	}
	for rel, sum := range after {
		if old, ok := before[rel]; !ok || old != sum {
			assert.True(t, expected(rel), "unexpected write: %s", rel)
		}
	}
	for rel := range before {
		if _, ok := after[rel]; !ok {
			assert.True(t, expected(rel), "unexpected removal: %s", rel)
		}
	}
	assert.Equal(t, before["unrelated.txt"], after["unrelated.txt"])
}

// listTree maps each path under root to its mode and content.
func listTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	require.NoError(t, filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if rel == "." {
			return nil
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		v := fi.Mode().String()
		if fi.Mode().IsRegular() {
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			v += ":" + string(b)
		}
		out[filepath.ToSlash(rel)] = v
		return nil
	}))
	return out
}

// Q-E (2): a positive legacy limit is carried over to the instance row and
// read back, before step 3.
func TestApply_CarriesLegacyLimit(t *testing.T) {
	f := newApplyFixture(t)
	f.hub.maxAgents = 10
	_, err := f.apply()
	require.NoError(t, err)
	require.NotNil(t, f.hub.instMax)
	assert.Equal(t, int64(10), *f.hub.instMax)
	assert.Equal(t, 1, f.hub.puts)
	// The limit is written before any project is re-pointed.
	require.NotEmpty(t, f.hub.order)
	assert.Equal(t, "capacity", f.hub.order[0])
}

// Q-E (2): a no_limit legacy host ends with an explicit 0 (unlimited) on
// the instance row, never unset.
func TestApply_NoLimitWritesExplicitZero(t *testing.T) {
	f := newApplyFixture(t)
	f.hub.maxAgents = 0
	_, err := f.apply()
	require.NoError(t, err)
	require.NotNil(t, f.hub.instMax, "never left unset (which would inherit the Hub default)")
	assert.Equal(t, int64(0), *f.hub.instMax)
}

// A failure at the capacity write leaves the item started; a re-run
// completes it with no duplicate write.
func TestApply_CapacityWriteFailureResumes(t *testing.T) {
	f := newApplyFixture(t)
	f.hub.maxAgents = 7
	f.hub.failNext["PUT /api/v1/runtime-brokers/"+applyInstanceID+"/settings"] = http.StatusInternalServerError
	_, err := f.apply()
	require.Error(t, err)
	assert.Equal(t, RefuseHubRequest, refusalCode(t, err))
	j, err := LoadJournal(f.g)
	require.NoError(t, err)
	assert.Equal(t, StepStarted, j.Register[0].Capacity.State)
	assert.Equal(t, StepPending, j.Settings.State, "settings untouched")

	_, err = f.apply()
	require.NoError(t, err)
	assert.Equal(t, 1, f.hub.puts, "one successful write")
	assert.Equal(t, int64(7), *f.hub.instMax)
	assert.Equal(t, 1, f.regCalls)
}

// The drift rule: a different value set on the instance row since step 1
// refuses, naming the key, rather than being overwritten.
func TestApply_CapacityDriftRefuses(t *testing.T) {
	f := newApplyFixture(t)
	f.hub.maxAgents = 7
	key := f.instanceKey() // before any apply, as in every other test
	f.hub.failNext["PUT /api/v1/runtime-brokers/"+applyInstanceID+"/settings"] = http.StatusInternalServerError
	_, err := f.apply()
	require.Error(t, err)
	other := int64(3)
	f.hub.instMax = &other
	_, err = f.apply()
	require.Error(t, err)
	assert.Equal(t, RefuseCapacityDrift, refusalCode(t, err))
	assert.Contains(t, err.Error(), key)
	assert.Equal(t, int64(3), *f.hub.instMax, "not overwritten")
}

// A stale revision (409) is re-read, not retried blind: still unset means
// one more write with the fresh revision.
func TestApply_CapacityConflictRereads(t *testing.T) {
	f := newApplyFixture(t)
	f.hub.maxAgents = 5
	f.hub.failNext["PUT /api/v1/runtime-brokers/"+applyInstanceID+"/settings"] = http.StatusConflict
	_, err := f.apply()
	require.NoError(t, err)
	assert.Equal(t, int64(5), *f.hub.instMax)
	assert.Equal(t, 1, f.hub.puts)
}

// Design erratum E1: with the no-write guard on (as every convert mode runs),
// a settings load does not rewrite the settings file, apply's explicit
// writers still work, and apply writes only the expected paths.
func TestApply_UnderNoWriteGuard_LoaderDoesNotRewrite(t *testing.T) {
	writesonread.IsolateForTest(t)
	writesonread.Set()
	f := newApplyFixture(t)
	key := f.instanceKey()
	before := settingsBytes(t, f.g)
	_, err := config.LoadSettings(f.g)
	require.NoError(t, err)
	assert.Equal(t, before, settingsBytes(t, f.g), "a loader under the guard does not rewrite the settings")

	tree := listTree(t, f.g)
	_, err = f.apply()
	require.NoError(t, err, "the explicit writers work under the guard")
	for rel, v := range listTree(t, f.g) {
		if old, ok := tree[rel]; !ok || old != v {
			ok := rel == "settings.yaml" || rel == ".settings.lock" || rel == "runtime-brokers" ||
				strings.HasPrefix(rel, "runtime-brokers/.convert") || strings.HasPrefix(rel, "runtime-brokers/"+key)
			assert.True(t, ok, "unexpected write under the guard: %s", rel)
		}
	}
}
