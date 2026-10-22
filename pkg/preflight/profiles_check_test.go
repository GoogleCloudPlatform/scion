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

package preflight

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/knadh/koanf/v2"
)

const testBroker = "broker-colocated"

// fakeDB is an in-memory ReadOnlyQueries.
type fakeDB struct {
	settings    []HubSettingRow
	agents      []agent.ProfileRefAgentRow
	unparseable int
	projects    []agent.ProfileRefProject
	providers   []agent.ProfileRefProvider
	noBroker    bool
	flatBroker  bool
	// errs fails the named read ("broker", "hub_settings", "agents",
	// "projects", "providers") with its error.
	errs map[string]error
}

func (d *fakeDB) HubSettings(context.Context, []string) ([]HubSettingRow, error) {
	return d.settings, d.errs["hub_settings"]
}

func (d *fakeDB) Agents(context.Context) ([]agent.ProfileRefAgentRow, int, error) {
	return d.agents, d.unparseable, d.errs["agents"]
}

func (d *fakeDB) Projects(context.Context) ([]agent.ProfileRefProject, error) {
	return d.projects, d.errs["projects"]
}

func (d *fakeDB) Providers(_ context.Context, brokerID string) ([]agent.ProfileRefProvider, error) {
	if brokerID != testBroker {
		return nil, d.errs["providers"]
	}
	return d.providers, d.errs["providers"]
}

func (d *fakeDB) Broker(_ context.Context, id string) (bool, bool, error) {
	return !d.noBroker && id == testBroker, d.flatBroker, d.errs["broker"]
}

// profilesHome isolates HOME, the working directory, the global settings
// overlay and the SCION_SEED_ and SCION_SERVER_ environment, and writes a
// versioned global settings file with active_profile active and profiles
// (each on the docker runtime), which is also the bootstrap material. It
// returns the global directory.
func profilesHome(t *testing.T, active string, profiles ...string) string {
	t.Helper()
	old := config.GetGlobalSettingsOverlay()
	t.Cleanup(func() { config.SetGlobalSettingsOverlay(old) })
	config.SetGlobalSettingsOverlay(nil)
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "SCION_SEED_") || strings.HasPrefix(k, "SCION_SERVER_") {
			t.Setenv(k, v) // restored at cleanup
			if err := os.Unsetenv(k); err != nil {
				t.Fatal(err)
			}
		}
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(home); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	globalDir := filepath.Join(home, ".scion")
	if err := os.MkdirAll(globalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString("schema_version: \"1\"\nactive_profile: " + active + "\n")
	if len(profiles) > 0 {
		b.WriteString("profiles:\n")
		for _, p := range profiles {
			b.WriteString("  " + p + ":\n    runtime: docker\n")
		}
	}
	if err := os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return globalDir
}

// profilesRow is a hub_settings profiles row holding the named profiles,
// each on the docker runtime.
func profilesRow(origin string, updatedBy *string, names ...string) HubSettingRow {
	doc := map[string]config.V1ProfileConfig{}
	for _, n := range names {
		doc[n] = config.V1ProfileConfig{Runtime: "docker"}
	}
	data, _ := json.Marshal(doc)
	return HubSettingRow{Section: "profiles", Origin: origin, UpdatedBy: updatedBy, Value: data}
}

func strPtr(s string) *string { return &s }

// runProfiles runs the settings.profiles check through RunAll.
func runProfiles(t *testing.T, globalDir string, db ReadOnlyQueries, brokerID string) Report {
	t.Helper()
	r := NewRegistry()
	r.Register(ProfilesCheck())
	return r.RunAll(context.Background(), Env{GlobalDir: globalDir, ColocatedBrokerID: brokerID, DB: db})
}

// codes lists the report's finding codes, in report order.
func codes(rep Report) []string {
	out := []string{}
	for _, f := range rep.Findings {
		out = append(out, f.Code)
	}
	return out
}

func finding(t *testing.T, rep Report, code string) Finding {
	t.Helper()
	for _, f := range rep.Findings {
		if f.Code == code {
			return f
		}
	}
	t.Fatalf("no finding %s in %v", code, codes(rep))
	return Finding{}
}

func wantCodes(t *testing.T, rep Report, want ...string) {
	t.Helper()
	if want == nil {
		want = []string{}
	}
	if got := codes(rep); !reflect.DeepEqual(got, want) {
		t.Fatalf("codes = %v, want %v\nfindings: %+v", got, want, rep.Findings)
	}
}

// A hub with no legacy co-located broker gets exactly the not-applicable
// information, never a block.
func TestProfilesCheck_HubOnly(t *testing.T) {
	globalDir := profilesHome(t, "a", "a")
	rep := runProfiles(t, globalDir, nil, "")
	wantCodes(t, rep, "settings.profiles.not_applicable")
	if rep.Findings[0].Severity != SeverityInfo || rep.Blocking != 0 {
		t.Fatalf("finding %+v (blocking %d), want info and no block", rep.Findings[0], rep.Blocking)
	}
}

// A flat co-located broker row is a legitimate deployment: the check does
// not apply (information, not a block), and none of that broker's agents
// is evaluated, even one on a missing profile.
func TestProfilesCheck_FlatColocatedNotApplicable(t *testing.T) {
	globalDir := profilesHome(t, "a", "a")
	db := &fakeDB{flatBroker: true, agents: []agent.ProfileRefAgentRow{{Slug: "w", ProjectID: "p1", RuntimeBrokerID: testBroker, AppliedProfile: "gone"}}}
	rep := runProfiles(t, globalDir, db, testBroker)
	wantCodes(t, rep, "settings.profiles.not_applicable")
	if f := rep.Findings[0]; f.Severity != SeverityInfo || rep.Blocking != 0 || len(f.Subjects) != 0 || !strings.Contains(f.Message, "is a flat instance") {
		t.Fatalf("finding %+v (blocking %d), want info naming the flat instance, no block, no subjects", f, rep.Blocking)
	}
}

// A broker ID with no broker row, a row with no origin, a stored document
// that does not decode, and a missing reader cannot be evaluated:
// could_not_run.
func TestProfilesCheck_CouldNotRun(t *testing.T) {
	globalDir := profilesHome(t, "a", "a")
	for _, c := range []struct {
		name string
		db   ReadOnlyQueries
		want string
	}{
		{"no broker row", &fakeDB{noBroker: true}, "has no broker row"},
		{"no origin", &fakeDB{settings: []HubSettingRow{profilesRow("", nil, "a")}}, "hub_settings row profiles has no origin"},
		{"undecodable profiles", &fakeDB{settings: []HubSettingRow{{Section: "profiles", Origin: "managed", Value: json.RawMessage(`{not json`)}}}, "stored profiles section does not decode"},
		{"no reader", nil, "no database reader"},
	} {
		t.Run(c.name, func(t *testing.T) {
			rep := runProfiles(t, globalDir, c.db, testBroker)
			wantCodes(t, rep, "settings.profiles.could_not_run")
			if !strings.Contains(rep.Findings[0].Message, c.want) {
				t.Fatalf("message %q, want it to mention %q", rep.Findings[0].Message, c.want)
			}
		})
	}
}

// The boot-time re-sync decides the after set (R3-B1): a seeded row is
// replaced by the current bootstrap document, a managed row is kept, a
// seeded row last written by someone else counts as managed, and a NULL
// updated_by stays seeded (N4-a).
func TestProfilesCheck_PostSyncRows(t *testing.T) {
	unplacedOnB := []agent.ProfileRefAgentRow{{Slug: "w", ProjectID: "p1", AppliedProfile: "b"}}
	for _, c := range []struct {
		name string
		row  HubSettingRow
		want []string
	}{
		{"seeded row re-synced to the bootstrap", profilesRow("seeded", strPtr("seed"), "a", "b"), []string{"settings.profiles.agent_saved_profile"}},
		{"seeded row with a NULL updated_by stays seeded", profilesRow("seeded", nil, "a", "b"), []string{"settings.profiles.agent_saved_profile"}},
		{"managed row kept", profilesRow("managed", strPtr("admin"), "a", "b"), nil},
		{"pre-origin row counts as managed", profilesRow("seeded", strPtr("admin"), "a", "b"), nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			globalDir := profilesHome(t, "a", "a")
			rep := runProfiles(t, globalDir, &fakeDB{settings: []HubSettingRow{c.row}, agents: unplacedOnB}, testBroker)
			wantCodes(t, rep, c.want...)
			if c.want != nil {
				f := finding(t, rep, "settings.profiles.agent_saved_profile")
				if f.Severity != SeverityBlock || f.Data["profile"] != "b" || f.Data["agents"] != 1 {
					t.Fatalf("finding %+v, want a block on profile b with 1 agent", f)
				}
				if !reflect.DeepEqual(f.Subjects, []string{"p1/w"}) {
					t.Fatalf("subjects %v, want [p1/w] (no project row: its ID)", f.Subjects)
				}
			}
		})
	}
}

// A seeded row whose bootstrap profiles were removed becomes {}: the after
// set is unmanaged (files plus built-ins), so the default moves from the
// single stored profile back to active_profile, an INFO and no block.
func TestProfilesCheck_SeededRowBootstrapRemoved(t *testing.T) {
	globalDir := profilesHome(t, "local")
	rep := runProfiles(t, globalDir, &fakeDB{settings: []HubSettingRow{profilesRow("seeded", strPtr("seed"), "a")}}, testBroker)
	wantCodes(t, rep, "settings.profiles.active_profile_changed")
	f := rep.Findings[0]
	if f.Severity != SeverityInfo || f.Data["profile"] != "local" || f.Data["previous"] != "a" {
		t.Fatalf("finding %+v, want info local, previous a", f)
	}
}

// An error extracting the bootstrap material is the blocking
// could_not_evaluate (N4-b), not a silent pass.
func TestProfilesCheck_ExtractErrorBlocks(t *testing.T) {
	globalDir := profilesHome(t, "a", "a")
	old := extractSection
	t.Cleanup(func() { extractSection = old })
	extractSection = func(*koanf.Koanf, string) (json.RawMessage, error) { return nil, errors.New("bad section") }
	rep := runProfiles(t, globalDir, &fakeDB{}, testBroker)
	wantCodes(t, rep, "settings.profiles.could_not_evaluate")
	if f := rep.Findings[0]; f.Severity != SeverityBlock || !strings.Contains(f.Message, "the profiles bootstrap material") && !strings.Contains(f.Message, "the runtimes bootstrap material") {
		t.Fatalf("finding %+v, want a block naming the section", f)
	}
	// The loader's error text can quote a value: it goes to the log, never into the finding.
	if strings.Contains(rep.Findings[0].Message, "bad section") {
		t.Fatalf("finding %+v quotes the loader's error", rep.Findings[0])
	}
}

// Agents on another broker are not counted; unplaced ones are (D2); a
// project's activeProfile counts only when the co-located broker provides
// the project; undecodable applied configs are counted under no profile,
// with a warning.
func TestProfilesCheck_Scope(t *testing.T) {
	globalDir := profilesHome(t, "a", "a")
	db := &fakeDB{
		settings: []HubSettingRow{profilesRow("managed", strPtr("admin"), "a", "x")},
		agents: []agent.ProfileRefAgentRow{
			{Slug: "other", ProjectID: "p1", RuntimeBrokerID: "broker-other", AppliedProfile: "gone-other"},
			{Slug: "flat", ProjectID: "p1", RuntimeBrokerID: "broker-flat-instance", AppliedProfile: "gone-flat"},
			{Slug: "unplaced", ProjectID: "p1", AppliedProfile: "gone-unplaced"},
		},
		unparseable: 2,
		projects: []agent.ProfileRefProject{
			{ID: "p1", Slug: "one", ActiveProfile: "gone-provided"},
			{ID: "p2", Slug: "two", ActiveProfile: "gone-unprovided"},
		},
		providers: []agent.ProfileRefProvider{{ProjectID: "p1"}},
	}
	rep := runProfiles(t, globalDir, db, testBroker)
	wantCodes(t, rep, "settings.profiles.agent_saved_profile.pre_existing", "settings.profiles.project_active_profile.pre_existing", "settings.profiles.unparseable_applied_config")
	if f := rep.Findings[0]; f.Data["profile"] != "gone-unplaced" || !reflect.DeepEqual(f.Subjects, []string{"one/unplaced"}) {
		t.Fatalf("agent finding %+v, want gone-unplaced with subject one/unplaced", f)
	}
	if f := rep.Findings[1]; f.Data["project"] != "p1" || !reflect.DeepEqual(f.Subjects, []string{"p1"}) {
		t.Fatalf("project finding %+v, want project p1", f)
	}
	if f := rep.Findings[2]; f.Severity != SeverityWarn || f.Data["agents"] != 2 {
		t.Fatalf("unparseable finding %+v, want a warn counting 2", f)
	}
}

// The after view keeps a referenced built-in profile (ptone/scion#5113),
// read from the agent's agent-info.json, which wins over the hub's applied
// profile: an INFO retained_builtin_profile and no block. A co-located
// agent with no agent-info.json raises the agent_info_missing INFO.
func TestProfilesCheck_RetainedBuiltin(t *testing.T) {
	globalDir := profilesHome(t, "gke", "gke")
	linked := filepath.Join(t.TempDir(), "linked", config.DotScion)
	if err := os.MkdirAll(linked, 0o755); err != nil {
		t.Fatal(err)
	}
	projectDir, err := config.GetResolvedProjectDir(linked)
	if err != nil {
		t.Fatal(err)
	}
	home := config.GetAgentHomePath(projectDir, "disk")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "agent-info.json"), []byte(`{"profile":"local"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	db := &fakeDB{
		settings: []HubSettingRow{profilesRow("managed", strPtr("admin"), "gke")},
		agents: []agent.ProfileRefAgentRow{
			{Slug: "disk", ProjectID: "p1", RuntimeBrokerID: testBroker, AppliedProfile: "remote"},
			{Slug: "nofile", ProjectID: "p1", RuntimeBrokerID: testBroker, AppliedProfile: "gke"},
		},
		projects:  []agent.ProfileRefProject{{ID: "p1", Slug: "one"}},
		providers: []agent.ProfileRefProvider{{ProjectID: "p1", LocalPath: linked}},
	}
	rep := runProfiles(t, globalDir, db, testBroker)
	wantCodes(t, rep, "settings.profiles.agent_info_missing", "settings.profiles.retained_builtin_profile")
	if f := finding(t, rep, "settings.profiles.agent_info_missing"); f.Data["missing"] != 1 || f.Data["unreadable"] != 0 {
		t.Fatalf("agent_info_missing %+v, want missing 1, unreadable 0", f)
	}
	f := finding(t, rep, "settings.profiles.retained_builtin_profile")
	if f.Severity != SeverityInfo || f.Data["profile"] != "local" || !reflect.DeepEqual(f.Subjects, []string{"one/disk"}) {
		t.Fatalf("retained finding %+v, want info local for one/disk", f)
	}
}

// A built-in active_profile with no reference is kept when two or more
// profiles are stored: the default resolves (no active_profile finding),
// and the INFO names the active_profile form.
func TestProfilesCheck_RetainedActiveProfile(t *testing.T) {
	globalDir := profilesHome(t, "local", "gke", "x")
	db := &fakeDB{
		settings: []HubSettingRow{profilesRow("managed", strPtr("admin"), "gke", "x")},
		agents:   []agent.ProfileRefAgentRow{{Slug: "w1", ProjectID: "p1"}, {Slug: "w2", ProjectID: "p1"}},
	}
	rep := runProfiles(t, globalDir, db, testBroker)
	wantCodes(t, rep, "settings.profiles.retained_builtin_profile")
	f := rep.Findings[0]
	if f.Data["agents"] != 2 || !strings.Contains(f.Message, "because it is the hub's active_profile") {
		t.Fatalf("finding %+v, want 2 agents and the active_profile wording", f)
	}
}

// With exactly one profile stored, the default stays on it even when the
// after view retains the built-in active_profile for an agent that uses
// it: the INFO fallback (no Previous: the default does not change), and
// the retained profile's count holds only its own agent.
func TestProfilesCheck_OneStoredDefaultStaysWithRetainedBuiltin(t *testing.T) {
	globalDir := profilesHome(t, "local", "gke")
	db := &fakeDB{
		settings: []HubSettingRow{profilesRow("managed", strPtr("admin"), "gke")},
		agents: []agent.ProfileRefAgentRow{
			{Slug: "w", ProjectID: "p1", AppliedProfile: "local"},
			{Slug: "nop", ProjectID: "p1"},
		},
	}
	rep := runProfiles(t, globalDir, db, testBroker)
	wantCodes(t, rep, "settings.profiles.active_profile_fallback", "settings.profiles.retained_builtin_profile")
	if f := rep.Findings[0]; f.Data["profile"] != "local" || f.Data["agents"] != 1 || f.Data["previous"] != nil {
		t.Fatalf("fallback finding %+v, want local, 1 agent without a profile, no previous", f)
	}
	if f := rep.Findings[1]; f.Data["profile"] != "local" || f.Data["agents"] != 1 {
		t.Fatalf("retained finding %+v, want local with 1 agent", f)
	}
}

// The before view is built without references (the running hub may predate
// ptone/scion#5113), even when the before rows store two or more profiles,
// where retention would keep a built-in active_profile: before, the default
// did not resolve, so the after view's single-profile fallback carries no
// Previous.
func TestProfilesCheck_BeforeViewWithoutRetention(t *testing.T) {
	globalDir := profilesHome(t, "local", "x")
	db := &fakeDB{settings: []HubSettingRow{profilesRow("seeded", strPtr("seed"), "x", "y")}}
	rep := runProfiles(t, globalDir, db, testBroker)
	wantCodes(t, rep, "settings.profiles.active_profile_fallback", "settings.profiles.dropped_unused_profile")
	if f := rep.Findings[0]; f.Data["profile"] != "local" || f.Data["previous"] != nil {
		t.Fatalf("fallback finding %+v, want local with no previous (before had no default)", f)
	}
	if f := rep.Findings[1]; f.Data["profile"] != "y" {
		t.Fatalf("dropped finding %+v, want y", f)
	}
}

// With stored runtimes, a built-in runtime kept for stored profiles that
// name it counts the agents of every after profile naming it.
func TestProfilesCheck_RetainedRuntimeSumsProfiles(t *testing.T) {
	globalDir := profilesHome(t, "gke")
	profiles, _ := json.Marshal(map[string]config.V1ProfileConfig{"gke": {Runtime: "k8s"}, "a": {Runtime: "docker"}, "b": {Runtime: "docker"}})
	runtimes, _ := json.Marshal(map[string]config.V1RuntimeConfig{"k8s": {Type: "kubernetes"}})
	db := &fakeDB{
		settings: []HubSettingRow{
			{Section: "profiles", Origin: "managed", UpdatedBy: strPtr("admin"), Value: profiles},
			{Section: "runtimes", Origin: "managed", UpdatedBy: strPtr("admin"), Value: runtimes},
		},
		agents: []agent.ProfileRefAgentRow{
			{Slug: "a1", ProjectID: "p1", AppliedProfile: "a"},
			{Slug: "b1", ProjectID: "p1", AppliedProfile: "b"},
			{Slug: "b2", ProjectID: "p1", AppliedProfile: "b"},
		},
	}
	rep := runProfiles(t, globalDir, db, testBroker)
	wantCodes(t, rep, "settings.profiles.retained_builtin_runtime")
	f := rep.Findings[0]
	if f.Data["profile"] != "docker" || f.Data["agents"] != 3 || !strings.Contains(f.Message, "(3 agent(s))") {
		t.Fatalf("finding %+v, want docker kept for 3 agents", f)
	}
}

// A settings loader's error can quote the value it could not convert. The
// finding carries the fixed class and the key path, never the error text:
// the secret-like value appears nowhere in the report.
func TestProfilesCheck_LoaderErrorsNeverQuoteValues(t *testing.T) {
	const secret = "s3cr3t-tok-91"
	mapErr := errors.New("1 error(s) decoding:\n\n* 'profiles.a.runtime' expected type 'string', got unconvertible type 'map[string]interface {}', value: 'map[k:" + secret + "]'")
	yamlErr := errors.New("yaml: unmarshal errors:\n  line 3: cannot unmarshal !!str `" + secret + "` into int")
	for _, c := range []struct {
		name     string
		inject   func(error)
		err      error
		wantCode string
		wantPath bool
	}{
		{"bootstrap extract, key path", func(e error) {
			extractSection = func(*koanf.Koanf, string) (json.RawMessage, error) { return nil, e }
		}, mapErr, "settings.profiles.could_not_evaluate", true},
		{"effective settings, key path", func(e error) {
			catalogView = func(string, map[string]config.V1RuntimeConfig, map[string]config.V1ProfileConfig, *config.ProfileReferences) (*config.VersionedSettings, error) {
				return nil, e
			}
		}, mapErr, "settings.profiles.could_not_run", true},
		{"profile origins, key path", func(e error) {
			profileOrigins = func(string) (map[string]config.ProfileSource, error) { return nil, e }
		}, mapErr, "settings.profiles.could_not_evaluate", true},
		{"effective settings, no key path", func(e error) {
			catalogView = func(string, map[string]config.V1RuntimeConfig, map[string]config.V1ProfileConfig, *config.ProfileReferences) (*config.VersionedSettings, error) {
				return nil, e
			}
		}, yamlErr, "settings.profiles.could_not_run", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			globalDir := profilesHome(t, "a", "a")
			oldE, oldV, oldO := extractSection, catalogView, profileOrigins
			t.Cleanup(func() { extractSection, catalogView, profileOrigins = oldE, oldV, oldO })
			c.inject(c.err)
			rep := runProfiles(t, globalDir, &fakeDB{settings: []HubSettingRow{profilesRow("managed", strPtr("admin"), "a")}}, testBroker)
			doc, err := json.Marshal(rep)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(doc), secret) {
				t.Fatalf("the report quotes the loader's value: %s", doc)
			}
			f := finding(t, rep, c.wantCode)
			if f.Severity != SeverityBlock {
				t.Fatalf("finding %+v, want a block", f)
			}
			if got := strings.Contains(f.Message, "(at profiles.a.runtime)"); got != c.wantPath {
				t.Fatalf("finding message %q: key path shown = %v, want %v", f.Message, got, c.wantPath)
			}
		})
	}
}

// loaderErrorClass names only catalog key paths: mapstructure v2's bare
// line-start form and v1's bullet form count; a quoted token at the start
// of a line inside a multi-line value does not.
func TestLoaderErrorClass_KeyPaths(t *testing.T) {
	for _, c := range []struct {
		name, err, want string
	}{
		{"v2 line-start form", "decoding failed due to the following error(s):\n\n'profiles.a.runtime' expected type 'string', got unconvertible type 'map[string]interface {}', value: 'map[k:v]'", "C (at profiles.a.runtime)"},
		{"v1 bullet form", "1 error(s) decoding:\n\n* 'runtimes.k8s.type' expected type 'string'", "C (at runtimes.k8s.type)"},
		{"quoted token inside a multi-line value", "decoding failed:\n\n'profiles.a.env' expected a map, got 'string', value: 'line one\n'tok-9' rest'", "C (at profiles.a.env)"},
		{"no catalog path", "yaml: unmarshal errors:\n  line 3: cannot unmarshal !!str `x` into int", "C"},
		{"a non-catalog quoted name", "decoding failed:\n\n'server.hub.port' expected type 'int'", "C"},
	} {
		if got := loaderErrorClass("C", errors.New(c.err)); got != c.want {
			t.Errorf("%s: loaderErrorClass = %q, want %q", c.name, got, c.want)
		}
	}
}

// A real settings file whose profile holds a secret-like value where a
// string belongs: whatever stage refuses it, the report never quotes the
// value, and the check blocks.
func TestProfilesCheck_RealBadSettingsFileNeverQuotesValue(t *testing.T) {
	const secret = "s3cr3t-tok-77"
	globalDir := profilesHome(t, "a")
	bad := "schema_version: \"1\"\nactive_profile: a\nprofiles:\n  a:\n    runtime:\n      token: " + secret + "\n"
	if err := os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	rep := runProfiles(t, globalDir, &fakeDB{}, testBroker)
	doc, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(doc), secret) {
		t.Fatalf("the report quotes the settings value: %s", doc)
	}
	if rep.Blocking == 0 {
		t.Fatalf("report %s, want a block: the settings do not decode", doc)
	}
}

// A database read error can carry a stored value or a connection string:
// the could_not_run finding names the table, never the error text.
func TestProfilesCheck_ReadErrorsNeverQuoted(t *testing.T) {
	const secret = "postgres://admin:s3cr3t-pw@db/hub value='tok-55'"
	for _, c := range []struct{ read, table string }{
		{"broker", "runtime_brokers"},
		{"hub_settings", "hub_settings"},
		{"agents", "agents"},
		{"projects", "projects"},
		{"providers", "project_contributors"},
	} {
		t.Run(c.read, func(t *testing.T) {
			globalDir := profilesHome(t, "a", "a")
			db := &fakeDB{settings: []HubSettingRow{profilesRow("managed", strPtr("admin"), "a")}, errs: map[string]error{c.read: errors.New(secret)}}
			rep := runProfiles(t, globalDir, db, testBroker)
			doc, err := json.Marshal(rep)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(doc), "s3cr3t-pw") || strings.Contains(string(doc), "tok-55") {
				t.Fatalf("the report quotes the read error: %s", doc)
			}
			wantCodes(t, rep, "settings.profiles.could_not_run")
			if !strings.Contains(rep.Findings[0].Message, "the "+c.table+" rows could not be read") {
				t.Fatalf("message %q, want it to name the %s table", rep.Findings[0].Message, c.table)
			}
		})
	}
}
