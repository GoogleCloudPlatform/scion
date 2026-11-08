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

//go:build unix

package runtime

import (
	"context"
	"encoding/base64"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/startuplistener"
	"github.com/GoogleCloudPlatform/scion/pkg/stagedsecrets"
)

// crEnvHarness is a test harness with separate GetEnv and GetTelemetryEnv
// maps that records the arguments GetEnv was called with.
type crEnvHarness struct {
	mockHarness
	telemetry                 map[string]string
	gotName, gotHome, gotUser string
}

func newCREnvHarness(env, telemetry map[string]string) *crEnvHarness {
	return &crEnvHarness{mockHarness: mockHarness{command: []string{"echo"}, env: env}, telemetry: telemetry}
}

func (h *crEnvHarness) GetEnv(agentName, agentHome, unixUsername string) map[string]string {
	h.gotName, h.gotHome, h.gotUser = agentName, agentHome, unixUsername
	return maps.Clone(h.env)
}

func (h *crEnvHarness) GetTelemetryEnv() map[string]string { return maps.Clone(h.telemetry) }

// crEnvCfg is runConfigForTest with h as the harness.
func crEnvCfg(h *crEnvHarness) RunConfig {
	cfg := runConfigForTest()
	cfg.UnixUsername = "scion"
	cfg.Harness = h
	return cfg
}

// crEnvRun runs cfg through CloudRunRuntime.Run against a fake Instances
// API and returns the created instance's plain env (crInstanceEnv fails on
// a duplicate name), the staged resolved-auth entries and the runtime log.
func crEnvRun(t *testing.T, cfg RunConfig) (plain, staged map[string]string, logs string) {
	t.Helper()
	fake := &fakeInstancesClient{getErr: notFoundErr()}
	rt := newFakeCloudRunRuntime(t, fake)
	buf := captureRuntimeLog(t)
	if _, err := rt.Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	plain = crInstanceEnv(t, fake)
	return plain, crStagedResolvedAuth(t, plain), buf.String()
}

// crEnvEffective is the value a Cloud Run harness receives for name: the
// plain env, else the staged resolved-auth entries (sciontool fills a
// staged entry only into a name the plain env lacks).
func crEnvEffective(plain, staged map[string]string, name string) (string, bool) {
	if v, ok := plain[name]; ok {
		return v, true
	}
	v, ok := staged[name]
	return v, ok
}

// harnessEnvParityFixture sets every source on cfg, each colliding with
// the harness env on one name.
func harnessEnvParityFixture(cfg RunConfig) RunConfig {
	cfg.Harness = newCREnvHarness(map[string]string{
		"EXAMPLE_HARNESS_ONLY": "from-harness",
		"GOOGLE_CLOUD_PROJECT": "from-harness", // a resolved-auth discovery name
		"EXAMPLE_AUTH_TOKEN":   "from-harness", // a staged resolved-auth name
		"EXAMPLE_TEL":          "from-harness",
		"EXAMPLE_TEL_EMPTY":    "from-harness",
		"EXAMPLE_CFG":          "from-harness",
		"EXAMPLE_SECRET":       "from-harness",
		"EXAMPLE_FB":           "from-harness",
		"EXAMPLE_EMPTY":        "",
	}, map[string]string{
		"EXAMPLE_TEL":       "from-telemetry",
		"EXAMPLE_TEL_EMPTY": "",
	})
	cfg.TelemetryEnabled = true
	cfg.Env = append(cfg.Env, "EXAMPLE_CFG=from-config-env")
	cfg.ResolvedSecrets = []api.ResolvedSecret{{Name: "s", Type: "environment", Target: "EXAMPLE_SECRET", Value: "from-secret"}}
	cfg.ResolvedAuth = &api.ResolvedAuth{Method: "api-key", EnvVars: map[string]string{
		"GOOGLE_CLOUD_PROJECT": "from-auth",
		"EXAMPLE_AUTH_TOKEN":   "from-auth",
		"EXAMPLE_AUTH_ONLY":    "from-auth",
		"CLOUD_ML_REGION":      "from-auth",
	}}
	cfg.FallbackEnv = map[string]string{"EXAMPLE_FB": "from-template", "EXAMPLE_FB_ONLY": "from-template"}
	return cfg
}

// harnessEnvParityWant is the effective env Docker gives
// harnessEnvParityFixture, for the fixture's names.
var harnessEnvParityWant = map[string]string{
	"EXAMPLE_HARNESS_ONLY": "from-harness",
	"GOOGLE_CLOUD_PROJECT": "from-harness",
	"EXAMPLE_AUTH_TOKEN":   "from-harness",
	"EXAMPLE_TEL":          "from-telemetry",
	"EXAMPLE_TEL_EMPTY":    "from-harness",
	"EXAMPLE_CFG":          "from-config-env",
	"EXAMPLE_SECRET":       "from-secret",
	"EXAMPLE_FB":           "from-harness",
	"EXAMPLE_AUTH_ONLY":    "from-auth",
	"CLOUD_ML_REGION":      "from-auth",
	"EXAMPLE_FB_ONLY":      "from-template",
}

// T1: the harness env, telemetry env and every other source give the same
// effective env on Docker and Cloud Run.
func TestHarnessEnv_ParityDockerCloudRun(t *testing.T) {
	dcfg := harnessEnvParityFixture(RunConfig{Name: "test-agent", UnixUsername: "scion", Image: "scion-agent:latest"})
	args, err := buildCommonRunArgs(dcfg)
	if err != nil {
		t.Fatalf("buildCommonRunArgs: %v", err)
	}
	docker := effectiveArgsEnv(args, dcfg.Image)

	plain, staged, _ := crEnvRun(t, harnessEnvParityFixture(crEnvCfg(nil)))

	for name, want := range harnessEnvParityWant {
		if got, ok := docker[name]; !ok || got != want {
			t.Errorf("docker: %s = %q (present %v), want %q", name, got, ok, want)
		}
		if got, ok := crEnvEffective(plain, staged, name); !ok || got != want {
			t.Errorf("cloudrun: %s = %q (present %v), want %q", name, got, ok, want)
		}
	}
	for runtimeName, env := range map[string]map[string]string{"docker": docker, "cloudrun": plain} {
		if _, ok := env["EXAMPLE_EMPTY"]; ok {
			t.Errorf("%s: an empty harness env value reached the container env", runtimeName)
		}
	}
	if _, ok := staged["EXAMPLE_AUTH_TOKEN"]; ok {
		t.Error("cloudrun: a resolved-auth entry the harness env sets is in the staged blob")
	}
	if staged["EXAMPLE_AUTH_ONLY"] != "from-auth" {
		t.Error("cloudrun: control: an ordinary resolved-auth entry is not in the staged blob")
	}
}

// T2: each adjacent pair of the Cloud Run env order, the higher source
// winning. crInstanceEnv also fails on a duplicate name.
func TestCloudRunRun_HarnessEnvPrecedence(t *testing.T) {
	tests := []struct {
		name      string
		env       map[string]string
		telemetry map[string]string
		mutate    func(*RunConfig)
		key       string
		want      string // "" with notWant: only that value is excluded
		notWant   string
		notStaged bool
	}{
		{
			name: "GetEnv beats a resolved-auth discovery name",
			env:  map[string]string{"GOOGLE_CLOUD_PROJECT": "from-harness"},
			mutate: func(c *RunConfig) {
				c.ResolvedAuth = &api.ResolvedAuth{EnvVars: map[string]string{"GOOGLE_CLOUD_PROJECT": "from-auth"}}
			},
			key: "GOOGLE_CLOUD_PROJECT", want: "from-harness",
		},
		{
			name: "GetEnv beats a staged resolved-auth entry",
			env:  map[string]string{"EXAMPLE_AUTH_TOKEN": "from-harness"},
			mutate: func(c *RunConfig) {
				c.ResolvedAuth = &api.ResolvedAuth{EnvVars: map[string]string{"EXAMPLE_AUTH_TOKEN": "from-auth"}}
			},
			key: "EXAMPLE_AUTH_TOKEN", want: "from-harness", notStaged: true,
		},
		{
			name:      "telemetry beats GetEnv",
			env:       map[string]string{"EXAMPLE_TEL": "from-harness"},
			telemetry: map[string]string{"EXAMPLE_TEL": "from-telemetry"},
			mutate:    func(c *RunConfig) { c.TelemetryEnabled = true },
			key:       "EXAMPLE_TEL", want: "from-telemetry",
		},
		{
			name: "runtime key beats GetEnv (host UID)",
			env:  map[string]string{"SCION_HOST_UID": "from-harness"},
			key:  "SCION_HOST_UID", notWant: "from-harness",
		},
		{
			name: "runtime key beats GetEnv (host GID)",
			env:  map[string]string{"SCION_HOST_GID": "from-harness"},
			key:  "SCION_HOST_GID", notWant: "from-harness",
		},
		{
			name: "runtime key beats GetEnv (startup listener port)",
			env:  map[string]string{startuplistener.EnvPort: "from-harness"},
			key:  startuplistener.EnvPort, notWant: "from-harness",
		},
		{
			name:   "config.Env beats GetEnv",
			env:    map[string]string{"EXAMPLE_CFG": "from-harness"},
			mutate: func(c *RunConfig) { c.Env = append(c.Env, "EXAMPLE_CFG=from-config-env") },
			key:    "EXAMPLE_CFG", want: "from-config-env",
		},
		{
			name: "env secret beats GetEnv",
			env:  map[string]string{"EXAMPLE_SECRET": "from-harness"},
			mutate: func(c *RunConfig) {
				c.ResolvedSecrets = []api.ResolvedSecret{{Name: "s", Type: "environment", Target: "EXAMPLE_SECRET", Value: "from-secret"}}
			},
			key: "EXAMPLE_SECRET", want: "from-secret",
		},
		{
			name:   "GetEnv beats the template fallback",
			env:    map[string]string{"EXAMPLE_FB": "from-harness"},
			mutate: func(c *RunConfig) { c.FallbackEnv = map[string]string{"EXAMPLE_FB": "from-template"} },
			key:    "EXAMPLE_FB", want: "from-harness",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := crEnvCfg(newCREnvHarness(tc.env, tc.telemetry))
			if tc.mutate != nil {
				tc.mutate(&cfg)
			}
			plain, staged, _ := crEnvRun(t, cfg)
			got, ok := plain[tc.key]
			if tc.notWant != "" {
				if !ok || got == tc.notWant {
					t.Errorf("%s = %q (present %v), want the runtime's value, not %q", tc.key, got, ok, tc.notWant)
				}
			} else if !ok || got != tc.want {
				t.Errorf("%s = %q (present %v), want %q", tc.key, got, ok, tc.want)
			}
			if _, inBlob := staged[tc.key]; tc.notStaged && inBlob {
				t.Errorf("%s is in the staged resolved-auth entries although the harness env sets it", tc.key)
			}
		})
	}
}

// T3: the telemetry env is applied only when telemetry is enabled.
func TestCloudRunRun_TelemetryEnvOnlyWhenEnabled(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		cfg := crEnvCfg(newCREnvHarness(nil, map[string]string{"EXAMPLE_TEL_ONLY": "from-telemetry"}))
		cfg.TelemetryEnabled = enabled
		plain, _, _ := crEnvRun(t, cfg)
		got, ok := plain["EXAMPLE_TEL_ONLY"]
		if enabled && (!ok || got != "from-telemetry") {
			t.Errorf("telemetry enabled: EXAMPLE_TEL_ONLY = %q (present %v), want %q", got, ok, "from-telemetry")
		}
		if !enabled && ok {
			t.Error("telemetry disabled: the telemetry env reached the instance")
		}
	}
}

// T4: an empty harness or telemetry value is skipped, and an empty
// telemetry value leaves the GetEnv value in place (Docker's addEnv).
func TestCloudRunRun_HarnessEnvEmptyValueSkipped(t *testing.T) {
	cfg := crEnvCfg(newCREnvHarness(
		map[string]string{"EXAMPLE_EMPTY": "", "EXAMPLE_KEPT": "from-harness", "EXAMPLE_CONTROL": "x"},
		map[string]string{"EXAMPLE_TEL_EMPTY": "", "EXAMPLE_KEPT": ""},
	))
	cfg.TelemetryEnabled = true
	plain, _, _ := crEnvRun(t, cfg)
	for _, name := range []string{"EXAMPLE_EMPTY", "EXAMPLE_TEL_EMPTY"} {
		if _, ok := plain[name]; ok {
			t.Errorf("%s: an empty value reached the instance", name)
		}
	}
	if plain["EXAMPLE_KEPT"] != "from-harness" {
		t.Errorf("EXAMPLE_KEPT = %q, want the GetEnv value kept over an empty telemetry value", plain["EXAMPLE_KEPT"])
	}
	if plain["EXAMPLE_CONTROL"] != "x" {
		t.Error("control: a non-empty harness value did not reach the instance")
	}
}

// T5 (names): every name the harness env may not set on Cloud Run, one
// subtest per name and source. A denied name never reaches the instance
// from the harness env; a hub-written GCP metadata value in cfg.Env is
// kept exactly once (also when empty); a staged key in cfg.Env is left to
// the runtime's own handling, and the harness value never replaces it;
// the log names the denied name and never carries its value.
func TestCloudRunRun_HarnessEnvDeniedNames(t *testing.T) {
	const canary = "harness-denied-canary-7295"
	type envCase struct {
		name   string
		cfgEnv *string // nil: cfg.Env does not set the name
		exact  bool    // the instance must hold exactly the cfg.Env value
	}
	fromHub, empty := "from-hub", ""
	// The hub-side fixture must be a decodable staged blob (an empty one).
	emptyBlob := base64.StdEncoding.EncodeToString([]byte("{}"))
	stagedCases := []envCase{{name: "absent-from-cfg-env"}, {name: "set-in-cfg-env", cfgEnv: &fromHub}}
	stagedBlobCases := []envCase{{name: "absent-from-cfg-env"}, {name: "set-in-cfg-env", cfgEnv: &emptyBlob}}
	metadataCases := []envCase{{name: "absent-from-cfg-env"}, {name: "set-in-cfg-env", cfgEnv: &fromHub, exact: true}, {name: "empty-in-cfg-env", cfgEnv: &empty, exact: true}}
	names := []struct {
		key   string
		cases []envCase
	}{
		{stagedsecrets.EnvVar, stagedBlobCases},
		{stagedsecrets.HubTokenStagedEnvVar, stagedCases},
		{stagedsecrets.ResolvedAuthStagedEnvVar, stagedCases},
		{telemetryGCPCredentialsEnvVar, stagedCases},
		{"SCION_METADATA_MODE", metadataCases},
		{"SCION_METADATA_SA_EMAIL", metadataCases},
		{"SCION_METADATA_PROJECT_ID", metadataCases},
	}
	for _, n := range names {
		for _, source := range []string{"getenv", "telemetry"} {
			for _, ec := range n.cases {
				t.Run(n.key+"/"+source+"/"+ec.name, func(t *testing.T) {
					supplied := map[string]string{n.key: canary, "EXAMPLE_CONTROL": "keep"}
					h := newCREnvHarness(supplied, nil)
					if source == "telemetry" {
						h = newCREnvHarness(map[string]string{"EXAMPLE_CONTROL": "keep"}, supplied)
					}
					cfg := crEnvCfg(h)
					cfg.TelemetryEnabled = true
					if ec.cfgEnv != nil {
						cfg.Env = append(cfg.Env, n.key+"="+*ec.cfgEnv)
					}
					plain, staged, logs := crEnvRun(t, cfg)

					got, ok := plain[n.key]
					switch {
					case ok && got == canary:
						t.Errorf("%s: the harness value reached the instance", n.key)
					case ec.cfgEnv == nil && ok && n.key != stagedsecrets.EnvVar:
						t.Errorf("%s reached the instance although no source but the harness set it", n.key)
					case ec.exact && (!ok || got != *ec.cfgEnv):
						t.Errorf("%s = %q (present %v), want the cfg.Env value %q", n.key, got, ok, *ec.cfgEnv)
					}
					if staged[n.key] == canary {
						t.Errorf("%s: the harness value reached the staged entries", n.key)
					}
					if !strings.Contains(logs, "harness env names not applied") || !strings.Contains(logs, n.key) {
						t.Errorf("the log does not name the denied name %s (log length %d)", n.key, len(logs))
					}
					if strings.Contains(logs, canary) {
						t.Error("the log carries the denied value")
					}
					if plain["EXAMPLE_CONTROL"] != "keep" {
						t.Error("control: an ordinary harness env entry did not reach the instance")
					}
				})
			}
		}
	}
}

// T5 (real blob): with a real staged blob (a file-type secret), a harness
// env that supplies SCION_STAGED_SECRETS is denied: the instance carries
// the runtime's blob exactly once, its decoded content equals the blob of
// the same start without that harness entry, and the log names the denied
// name without its value.
func TestCloudRunRun_HarnessEnvStagedBlobUnchanged(t *testing.T) {
	const canary = "harness-blob-canary-7295"
	runWith := func(harnessEnv map[string]string) (map[string]string, *stagedsecrets.Staged, string) {
		cfg := crEnvCfg(newCREnvHarness(harnessEnv, nil))
		cfg.ResolvedSecrets = []api.ResolvedSecret{{Name: "creds", Type: "file", Target: crTestFileTarget, Value: crTestFileSecretValue}}
		plain, _, logs := crEnvRun(t, cfg)
		blob, ok := plain[stagedsecrets.EnvVar]
		if !ok || blob == canary {
			t.Fatalf("%s present %v, harness value %v; want the runtime's staged blob", stagedsecrets.EnvVar, ok, blob == canary)
		}
		staged, err := stagedsecrets.Decode(blob)
		if err != nil {
			t.Fatalf("decode %s: %v", stagedsecrets.EnvVar, err)
		}
		return plain, staged, logs
	}

	controlPlain, control, controlLogs := runWith(map[string]string{"EXAMPLE_CONTROL": "keep"})
	if len(control.FileSecrets) == 0 {
		t.Fatal("control: the file-type secret is not in the staged blob, so the check is vacuous")
	}
	plain, denied, logs := runWith(map[string]string{stagedsecrets.EnvVar: canary, "EXAMPLE_CONTROL": "keep"})
	// The real staged secret never reaches the log either, with or without
	// the denied harness entry: neither its plaintext, nor its standard
	// base64 encoding, nor the emitted (base64) blob that carries it.
	leaks := []string{
		crTestFileSecretValue,
		base64.StdEncoding.EncodeToString([]byte(crTestFileSecretValue)),
		controlPlain[stagedsecrets.EnvVar],
		plain[stagedsecrets.EnvVar],
	}
	for _, leak := range leaks {
		if strings.Contains(controlLogs, leak) || strings.Contains(logs, leak) {
			t.Error("the log carries the staged file secret or the emitted staged blob")
		}
	}

	if !reflect.DeepEqual(control, denied) {
		t.Error("the staged blob differs when the harness env supplies SCION_STAGED_SECRETS")
	}
	if !strings.Contains(logs, "harness env names not applied") || !strings.Contains(logs, stagedsecrets.EnvVar) {
		t.Errorf("the log does not name the denied name %s (log length %d)", stagedsecrets.EnvVar, len(logs))
	}
	if strings.Contains(logs, canary) {
		t.Error("the log carries the denied value")
	}
	if plain["EXAMPLE_CONTROL"] != "keep" {
		t.Error("control: an ordinary harness env entry did not reach the instance")
	}
}

// T6: GetEnv gets the agent name, cfg.HomeDir (the broker-side host path
// per the {{ .AgentHome }} contract, as on Docker and Kubernetes, not the
// instance's /home/<user>) and the unix username.
func TestCloudRunRun_HarnessEnvAgentHomeArg(t *testing.T) {
	rt := newFakeCloudRunRuntime(t, &fakeInstancesClient{getErr: notFoundErr()})
	h := &crEnvHarness{mockHarness: mockHarness{command: []string{"echo"}}}
	h.env = map[string]string{"EXAMPLE_SEEN": "set"}
	cfg := crEnvCfg(h)
	cfg.HomeDir = "/var/lib/scion-broker/agents/agent-1/home"
	inst := rt.buildCloudRunInstance(cfg, 1000, 1000, nil)
	if h.gotName != cfg.Name || h.gotHome != cfg.HomeDir || h.gotUser != cfg.UnixUsername {
		t.Errorf("GetEnv(%q, %q, %q), want (%q, %q, %q)", h.gotName, h.gotHome, h.gotUser, cfg.Name, cfg.HomeDir, cfg.UnixUsername)
	}
	if h.gotHome == "/home/"+rt.ExecUser() {
		t.Error("GetEnv got the in-instance home, want cfg.HomeDir")
	}
	found := false
	for _, ev := range inst.Containers[0].Env {
		found = found || (ev.GetName() == "EXAMPLE_SEEN" && ev.GetValue() == "set")
	}
	if !found {
		t.Error("control: the harness env did not reach the instance")
	}
}

// T7: the harness env entries are emitted in name order, so the instance
// spec is deterministic, and an existing instance built with the same
// harness env matches it and is reused (no env_names diff). 26 names make
// a map-iteration order that happens to be sorted negligible.
func TestCloudRunRun_HarnessEnvSpecStable(t *testing.T) {
	harnessEnv := map[string]string{}
	for c := 'Z'; c >= 'A'; c-- {
		harnessEnv["EXAMPLE_"+string(c)] = string(c)
	}

	t.Run("name order", func(t *testing.T) {
		rt := newFakeCloudRunRuntime(t, &fakeInstancesClient{getErr: notFoundErr()})
		inst := rt.buildCloudRunInstance(crEnvCfg(newCREnvHarness(harnessEnv, nil)), 1000, 1000, nil)
		var order []string
		for _, ev := range inst.Containers[0].Env {
			if _, ok := harnessEnv[ev.GetName()]; ok {
				order = append(order, ev.GetName())
			}
		}
		if len(order) != len(harnessEnv) || !slices.IsSorted(order) {
			t.Errorf("harness env order = %v, want all %d names sorted", order, len(harnessEnv))
		}
	})

	t.Run("matching instance reused", func(t *testing.T) {
		fake := &fakeInstancesClient{}
		rt := newReadyTestRuntime(t, fake)
		cfg := readyRunCfg()
		cfg.UnixUsername = "scion"
		cfg.Harness = newCREnvHarness(harnessEnv, nil)
		fake.getInstance = computedCloudRunInstance(t, rt, cfg)
		if _, err := rt.Run(context.Background(), cfg); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if len(fake.startReqs) != 1 || len(fake.deleteReqs) != 0 || len(fake.createReqs) != 0 {
			t.Errorf("start/delete/create = %d/%d/%d, want 1/0/0 (reused)", len(fake.startReqs), len(fake.deleteReqs), len(fake.createReqs))
		}
	})
}

// T9: with a broker-resolved identity (hub.cloudrun_native_identity), the
// harness env is applied as without one, the hub-written GCP metadata
// settings in cfg.Env are kept exactly once, and the instance identity is
// unaffected.
func TestCloudRunRun_HarnessEnvWithNativeIdentity(t *testing.T) {
	const canary = "harness-identity-canary-7295"
	fake := &fakeInstancesClient{getErr: notFoundErr()}
	rt, _ := nativeIdentityRuntime(t, fake)
	cfg := nativeIdentityRunConfig(&api.CloudRunIdentity{ServiceAccount: nativeIDAssignedSA, Mode: "assign"})
	cfg.UnixUsername = "scion"
	cfg.Env = append(cfg.Env, "SCION_METADATA_SA_EMAIL="+nativeIDAssignedSA, "SCION_METADATA_PROJECT_ID=example-project", "EXAMPLE_HUB_SETTING=from-hub")
	cfg.Harness = newCREnvHarness(map[string]string{
		"EXAMPLE_HARNESS":           "from-harness",
		"EXAMPLE_HUB_SETTING":       canary, // cfg.Env wins: emitted once, with the cfg.Env value
		"SCION_METADATA_SA_EMAIL":   canary,
		"SCION_METADATA_PROJECT_ID": canary,
	}, nil)
	if _, err := rt.Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	env := crInstanceEnv(t, fake)
	want := map[string]string{
		"EXAMPLE_HARNESS":           "from-harness",
		"EXAMPLE_HUB_SETTING":       "from-hub",
		"SCION_METADATA_SA_EMAIL":   nativeIDAssignedSA,
		"SCION_METADATA_PROJECT_ID": "example-project",
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("%s = %q, want %q", k, env[k], v)
		}
	}
	inst := fake.createReqs[0].Instance
	if inst.GetServiceAccount() != nativeIDAssignedSA {
		t.Errorf("ServiceAccount = %q, want the resolved identity", inst.GetServiceAccount())
	}
	if inst.GetLabels()[cloudRunIdentityLabel] != cloudRunIdentityHash(nativeIDAssignedSA) {
		t.Error("the identity label is not the resolved identity's hash")
	}
}

// harnessEnvEntries follows the Docker rules: telemetry over GetEnv when
// enabled, empty values skipped (an empty telemetry value keeps GetEnv's),
// sorted, nil without a harness.
func TestHarnessEnvEntries(t *testing.T) {
	if got := harnessEnvEntries(RunConfig{}, "/home"); got != nil {
		t.Errorf("no harness: %v, want nil", got)
	}
	h := newCREnvHarness(
		map[string]string{"B": "env", "A": "env", "C": "", "D": "env"},
		map[string]string{"B": "tel", "D": "", "E": "tel"},
	)
	got := harnessEnvEntries(RunConfig{Harness: h, TelemetryEnabled: true}, "/home")
	want := []string{"A=env", "B=tel", "D=env", "E=tel"}
	if !slices.Equal(got, want) {
		t.Errorf("telemetry enabled: %v, want %v", got, want)
	}
	got = harnessEnvEntries(RunConfig{Harness: h}, "/home")
	want = []string{"A=env", "B=env", "D=env"}
	if !slices.Equal(got, want) {
		t.Errorf("telemetry disabled: %v, want %v", got, want)
	}
}
