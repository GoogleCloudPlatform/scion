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

package config

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// A flat instance's agent_defaults
// (.design/flat-runtime-brokers/contract-appendix-p3.md FC1, FC2, FC5, FC7):
// each value takes the rung the profile's value took, and an unset value
// falls through to the same lower settings rungs.

// legacyAgentDefaultsSettings has an active profile whose workload
// defaults must never reach a flat view, and lower settings rungs that a
// flat view must still reach when the instance leaves a field unset.
func legacyAgentDefaultsSettings() *VersionedSettings {
	return &VersionedSettings{
		ActiveProfile:        "batch",
		ImageRegistry:        "registry.example/global",
		DefaultHarnessConfig: "settings-hc",
		HarnessConfigs: map[string]HarnessConfigEntry{"claude": {
			Harness: "claude", Image: "settings-image:v1", ImagePullPolicy: "IfNotPresent", User: "scion",
			Env:     map[string]string{"BASE": "base", "SHARED": "from-harness-config"},
			Volumes: []api.VolumeMount{{Source: "/hc", Target: "/mnt/hc"}},
		}},
		Runtimes: map[string]V1RuntimeConfig{"docker": {Type: "docker"}},
		Profiles: map[string]V1ProfileConfig{"batch": {
			Runtime:              "docker",
			DefaultHarnessConfig: "profile-hc",
			ImageRegistry:        "registry.example/profile",
			Volumes:              []api.VolumeMount{{Source: "/profile", Target: "/mnt/profile"}},
			Resources:            &api.ResourceSpec{Requests: api.ResourceList{Memory: "7777Mi"}},
			Secrets:              []api.RequiredSecret{{Key: "PROFILE_SECRET"}},
			CloneDepth:           "7",
			HarnessOverrides: map[string]V1HarnessOverride{
				"claude": {Image: "profile-image:v7", ImagePullPolicy: "Never", User: "profile-user", AuthSelectedType: "auth-file",
					Env: map[string]string{"PROFILE_EMPTY": ""}, Resources: &api.ResourceSpec{Limits: api.ResourceList{CPU: "7"}}},
			},
		}},
	}
}

// fullAgentDefaults sets every field of V1InstanceAgentDefaults
// (TestFullAgentDefaults_SetsEveryField keeps it complete).
func fullAgentDefaults() *V1InstanceAgentDefaults {
	return &V1InstanceAgentDefaults{
		DefaultHarnessConfig: "instance-hc",
		ImageRegistry:        "registry.example/instance",
		Volumes:              []api.VolumeMount{{Source: "/inst", Target: "/mnt/inst"}, {Source: "/inst2", Target: "/mnt/inst2", ReadOnly: true}},
		Resources:            &api.ResourceSpec{Requests: api.ResourceList{CPU: "500m", Memory: "1Gi"}},
		HarnessOverrides: map[string]V1HarnessOverride{
			"claude": {Image: "instance-image:v2", ImagePullPolicy: "Always", User: "instance-user", AuthSelectedType: "vertex-ai",
				Env:       map[string]string{"SHARED": "from-instance", "INSTANCE_EMPTY": ""},
				Volumes:   []api.VolumeMount{{Source: "/ho", Target: "/mnt/ho"}},
				Resources: &api.ResourceSpec{Limits: api.ResourceList{CPU: "2"}}},
			"gemini": {Env: map[string]string{"OTHER_HC_EMPTY": "", "OTHER_HC_SET": "x"}},
		},
		Secrets:    []api.RequiredSecret{{Key: "INSTANCE_SECRET", Description: "instance secret", Type: "environment", AlternativeEnvKeys: []string{"ALT"}}},
		CloneDepth: "full",
	}
}

func agentDefaultsPolicy(key string, d *V1InstanceAgentDefaults) *FlatInstancePolicy {
	return NewFlatInstancePolicy(V1RuntimeBrokerInstanceConfig{Key: key, Name: key,
		RuntimeTarget: &V1RuntimeTargetConfig{Type: RuntimeTargetTypeDocker}, AgentDefaults: d})
}

func TestFullAgentDefaults_SetsEveryField(t *testing.T) {
	v := reflect.ValueOf(*fullAgentDefaults())
	for i := 0; i < v.NumField(); i++ {
		if v.Field(i).IsZero() {
			t.Errorf("fullAgentDefaults leaves %s unset; set it so the copy and accessor tests cover it", v.Type().Field(i).Name)
		}
	}
}

// Behavioural guard: with an active profile in the underlying settings, a
// flat view returns only instance values from every accessor; the legacy
// view still returns the profile's.
func TestFlatAgentDefaults_EveryAccessorReadsOnlyPolicy(t *testing.T) {
	vs := legacyAgentDefaultsSettings()
	view := vs.ForFlatInstance(agentDefaultsPolicy("docker-1", fullAgentDefaults()))
	if view.Profiles != nil || view.ActiveProfile != "" {
		t.Fatalf("a flat view has no profile tier: %+v", view)
	}
	const base = "server.broker.instances[docker-1].agent_defaults"
	for _, profile := range []string{"", "batch"} {
		if got := view.ResolveImageRegistry(profile); got != "registry.example/instance" {
			t.Errorf("ResolveImageRegistry(%q) = %q", profile, got)
		}
		if got, src := view.ResolveCloneDepthWithSource(profile); got != "full" || src != base+".clone_depth" {
			t.Errorf("ResolveCloneDepthWithSource(%q) = (%q, %q)", profile, got, src)
		}
		if got := view.ProfileHarnessOverrideImage(profile, "claude"); got != "instance-image:v2" {
			t.Errorf("ProfileHarnessOverrideImage(%q) = %q", profile, got)
		}
		if got := view.ProfileHarnessOverrideImagePullPolicy(profile, "claude"); got != "Always" {
			t.Errorf("ProfileHarnessOverrideImagePullPolicy(%q) = %q", profile, got)
		}
		if hc, src := view.TierDefaultHarnessConfig(profile); hc != "instance-hc" || src != base+".default_harness_config" {
			t.Errorf("TierDefaultHarnessConfig(%q) = (%q, %q)", profile, hc, src)
		}
		res, hoRes, src := view.TierResources(profile, "claude")
		if res == nil || res.Requests.Memory != "1Gi" || hoRes == nil || hoRes.Limits.CPU != "2" || src != base {
			t.Errorf("TierResources(%q) = (%+v, %+v, %q)", profile, res, hoRes, src)
		}
		if secrets, src := view.TierSecrets(profile); len(secrets) != 1 || secrets[0].Key != "INSTANCE_SECRET" || src != base+".secrets" {
			t.Errorf("TierSecrets(%q) = (%+v, %q)", profile, secrets, src)
		}
		if o, ok, src := view.TierHarnessOverride(profile, "claude"); !ok || o.AuthSelectedType != "vertex-ai" || src != base+".harness_overrides.claude" {
			t.Errorf("TierHarnessOverride(%q) = (%+v, %v, %q)", profile, o, ok, src)
		}
		if keys, src := view.TierAllHarnessOverrideEnvKeys(profile); !reflect.DeepEqual(keys, map[string]struct{}{"INSTANCE_EMPTY": {}, "OTHER_HC_EMPTY": {}}) || src != base+".harness_overrides" {
			t.Errorf("TierAllHarnessOverrideEnvKeys(%q) = (%v, %q)", profile, keys, src)
		}
		hc, err := view.ResolveHarnessConfig(profile, "claude")
		if err != nil || hc.Image != "instance-image:v2" || hc.User != "instance-user" || hc.AuthSelectedType != "vertex-ai" {
			t.Errorf("ResolveHarnessConfig(%q) = (%+v, %v)", profile, hc, err)
		}
	}

	// Control: legacy resolution of the same settings is the profile's.
	if got := vs.ResolveImageRegistry(""); got != "registry.example/profile" {
		t.Errorf("legacy ResolveImageRegistry = %q", got)
	}
	if got, src := vs.ResolveCloneDepthWithSource(""); got != "7" || src != "profiles.batch.clone_depth" {
		t.Errorf("legacy ResolveCloneDepthWithSource = (%q, %q)", got, src)
	}
	if got := vs.ProfileHarnessOverrideImage("", "claude"); got != "profile-image:v7" {
		t.Errorf("legacy ProfileHarnessOverrideImage = %q", got)
	}
	if hc, src := vs.TierDefaultHarnessConfig(""); hc != "profile-hc" || src != "profile-batch" {
		t.Errorf("legacy TierDefaultHarnessConfig = (%q, %q)", hc, src)
	}
	if res, hoRes, src := vs.TierResources("", "claude"); res == nil || res.Requests.Memory != "7777Mi" || hoRes == nil || hoRes.Limits.CPU != "7" || src != "profiles.batch" {
		t.Errorf("legacy TierResources = (%+v, %+v, %q)", res, hoRes, src)
	}
	if secrets, src := vs.TierSecrets(""); len(secrets) != 1 || secrets[0].Key != "PROFILE_SECRET" || src != "profiles.batch.secrets" {
		t.Errorf("legacy TierSecrets = (%+v, %q)", secrets, src)
	}
	if o, ok, _ := vs.TierHarnessOverride("", "claude"); !ok || o.AuthSelectedType != "auth-file" {
		t.Errorf("legacy TierHarnessOverride = (%+v, %v)", o, ok)
	}
	if keys, _ := vs.TierAllHarnessOverrideEnvKeys(""); !reflect.DeepEqual(keys, map[string]struct{}{"PROFILE_EMPTY": {}}) {
		t.Errorf("legacy TierAllHarnessOverrideEnvKeys = %v", keys)
	}
}

// Fall-through (FC5): with every agent_defaults field unset, each accessor
// returns the lower settings rung, never "unset" early and never the
// active profile's value.
func TestFlatAgentDefaults_UnsetFallsThroughToLowerRungs(t *testing.T) {
	for name, d := range map[string]*V1InstanceAgentDefaults{"no block": nil, "empty block": {}} {
		t.Run(name, func(t *testing.T) {
			view := legacyAgentDefaultsSettings().ForFlatInstance(agentDefaultsPolicy("docker-1", d))
			if got := view.ResolveImageRegistry(""); got != "registry.example/global" {
				t.Errorf("ResolveImageRegistry = %q, want the top-level image_registry", got)
			}
			hc, err := view.ResolveHarnessConfig("", "claude")
			if err != nil || !reflect.DeepEqual(hc, legacyAgentDefaultsSettings().HarnessConfigs["claude"]) {
				t.Errorf("ResolveHarnessConfig = (%+v, %v), want the harness_configs base unchanged", hc, err)
			}
			res, err := ResolveHarnessConfigName(HarnessConfigInputs{Settings: view})
			if err != nil || res.Name != "settings-hc" || res.Source != "settings-default" {
				t.Errorf("ResolveHarnessConfigName = (%+v, %v), want the top-level default_harness_config (rung 7)", res, err)
			}
			if got, src := view.TierDefaultHarnessConfig(""); got != "" || src != "" {
				t.Errorf("TierDefaultHarnessConfig = (%q, %q), want unset", got, src)
			}
			// No lower settings rung: unset means the built-in behaviour.
			if got, src := view.ResolveCloneDepthWithSource(""); got != "" || src != "" {
				t.Errorf("ResolveCloneDepthWithSource = (%q, %q), want unset (default shallow clone)", got, src)
			}
			if got := view.ProfileHarnessOverrideImage("", "claude"); got != "" {
				t.Errorf("ProfileHarnessOverrideImage = %q, want unset (the settings image keeps its tier)", got)
			}
			if got := view.ProfileHarnessOverrideImagePullPolicy("", "claude"); got != "" {
				t.Errorf("ProfileHarnessOverrideImagePullPolicy = %q, want unset", got)
			}
			if res, hoRes, src := view.TierResources("", "claude"); res != nil || hoRes != nil || src != "" {
				t.Errorf("TierResources = (%+v, %+v, %q), want none (Hub and default_resources still apply)", res, hoRes, src)
			}
			if secrets, src := view.TierSecrets(""); secrets != nil || src != "" {
				t.Errorf("TierSecrets = (%+v, %q), want none", secrets, src)
			}
			if o, ok, src := view.TierHarnessOverride("", "claude"); ok || src != "" || !reflect.DeepEqual(o, V1HarnessOverride{}) {
				t.Errorf("TierHarnessOverride = (%+v, %v, %q), want none", o, ok, src)
			}
			if keys, src := view.TierAllHarnessOverrideEnvKeys(""); keys != nil || src != "" {
				t.Errorf("TierAllHarnessOverrideEnvKeys = (%v, %q), want none", keys, src)
			}
		})
	}
}

// FC5 `default_harness_config`: the instance value sits at rung 6 of
// ResolveHarnessConfigName: below the template's (rungs 3-4) and the stored
// config (rungs 2 and 5), above the top-level default (rung 7).
func TestFlatAgentDefaults_DefaultHarnessConfigRung(t *testing.T) {
	view := legacyAgentDefaultsSettings().ForFlatInstance(agentDefaultsPolicy("docker-1", &V1InstanceAgentDefaults{DefaultHarnessConfig: "instance-hc"}))
	cases := []struct {
		name   string
		in     HarnessConfigInputs
		want   string
		source string
	}{
		{"instance beats top-level", HarnessConfigInputs{Settings: view}, "instance-hc", "server.broker.instances[docker-1].agent_defaults.default_harness_config"},
		{"template default beats instance", HarnessConfigInputs{Settings: view, TemplateCfg: &api.ScionConfig{DefaultHarnessConfig: "tpl-hc"}}, "tpl-hc", "template-default"},
		{"template harness config beats instance", HarnessConfigInputs{Settings: view, TemplateCfg: &api.ScionConfig{HarnessConfig: "tpl-hc2"}}, "tpl-hc2", "template-harness-config"},
		{"stored harness name beats instance", HarnessConfigInputs{Settings: view, StoredConfig: &api.ScionConfig{Harness: "stored"}}, "stored", "stored-harness-name"},
		{"a profile name does not matter", HarnessConfigInputs{Settings: view, ProfileName: "batch"}, "instance-hc", "server.broker.instances[docker-1].agent_defaults.default_harness_config"},
	}
	for _, c := range cases {
		res, err := ResolveHarnessConfigName(c.in)
		if err != nil || res.Name != c.want || res.Source != c.source {
			t.Errorf("%s: got (%+v, %v), want (%q, %q)", c.name, res, err, c.want, c.source)
		}
	}
}

// Rung 6 call site: settings built through the flat view
// (Profiles == nil), an empty profile name and an instance value set. The
// former rung-6 guard (an empty effective profile skips the rung) must not
// skip the instance value.
func TestResolveHarnessConfigName_FlatCallSiteUsesInstanceDefault(t *testing.T) {
	vs := &VersionedSettings{DefaultHarnessConfig: "settings-hc",
		Profiles: map[string]V1ProfileConfig{"batch": {Runtime: "docker", DefaultHarnessConfig: "profile-hc"}}}
	view := vs.ForFlatInstance(agentDefaultsPolicy("docker-1", &V1InstanceAgentDefaults{DefaultHarnessConfig: "instance-hc"}))
	if view.Profiles != nil || view.ActiveProfile != "" {
		t.Fatalf("the flat view must have no profiles: %+v", view)
	}
	res, err := ResolveHarnessConfigName(HarnessConfigInputs{Settings: view, ProfileName: ""})
	if err != nil {
		t.Fatal(err)
	}
	if res.Name != "instance-hc" {
		t.Fatalf("resolved %q from %s, want the instance's agent_defaults.default_harness_config", res.Name, res.Source)
	}
}

// FC5 `image_registry`: the instance value beats the top-level one.
func TestFlatAgentDefaults_ImageRegistryRung(t *testing.T) {
	view := legacyAgentDefaultsSettings().ForFlatInstance(agentDefaultsPolicy("docker-1", &V1InstanceAgentDefaults{ImageRegistry: "registry.example/instance"}))
	if got := view.ResolveImageRegistry(""); got != "registry.example/instance" {
		t.Fatalf("got %q, want the instance registry over the top-level one", got)
	}
	if !view.IsImageRegistryConfigured("") {
		t.Fatal("an instance registry counts as configured")
	}
}

// FC5 `volumes` and `harness_overrides.<hc>.*`: ResolveHarnessConfig applies the instance volumes after the
// harness config's (same order as a profile's), then the instance's
// harness override for the active harness config over the base: image,
// pull policy, user, auth type, env merged over the base env, volumes
// appended last. Another harness config's override does not apply.
func TestFlatAgentDefaults_ResolveHarnessConfigVolumesAndOverride(t *testing.T) {
	vs := legacyAgentDefaultsSettings()
	view := vs.ForFlatInstance(agentDefaultsPolicy("docker-1", fullAgentDefaults()))
	got, err := view.ResolveHarnessConfig("", "claude")
	if err != nil {
		t.Fatal(err)
	}
	want := HarnessConfigEntry{
		Harness: "claude", Image: "instance-image:v2", ImagePullPolicy: "Always", User: "instance-user", AuthSelectedType: "vertex-ai",
		Env: map[string]string{"BASE": "base", "SHARED": "from-instance", "INSTANCE_EMPTY": ""},
		Volumes: []api.VolumeMount{
			{Source: "/hc", Target: "/mnt/hc"},
			{Source: "/inst", Target: "/mnt/inst"}, {Source: "/inst2", Target: "/mnt/inst2", ReadOnly: true},
			{Source: "/ho", Target: "/mnt/ho"},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ResolveHarnessConfig:\n got %+v\nwant %+v", got, want)
	}
	// The policy and the settings are only read.
	if !reflect.DeepEqual(vs.HarnessConfigs["claude"], legacyAgentDefaultsSettings().HarnessConfigs["claude"]) {
		t.Fatal("ResolveHarnessConfig modified the settings harness config")
	}
	if !reflect.DeepEqual(view.flat.Defaults, *fullAgentDefaults()) {
		t.Fatal("ResolveHarnessConfig modified the policy")
	}
	other, err := view.ResolveHarnessConfig("", "gemini")
	if err != nil || other.Image != "" || other.Env["OTHER_HC_SET"] != "x" || len(other.Volumes) != 2 {
		t.Fatalf("gemini: got (%+v, %v), want only its own override and the instance volumes", other, err)
	}
}

// FC5 `harness_overrides.<hc>.*`, the ptone/scion#1799 exception. The explicit instance override image
// (and its pull policy) is reported separately from the plain settings
// image, so image precedence can rank it over the template and inline
// tiers; a harness config without an override reports none.
func TestFlatAgentDefaults_OverrideImageReportedSeparately(t *testing.T) {
	view := legacyAgentDefaultsSettings().ForFlatInstance(agentDefaultsPolicy("docker-1", fullAgentDefaults()))
	if got := view.ProfileHarnessOverrideImage("", "claude"); got != "instance-image:v2" {
		t.Fatalf("override image %q", got)
	}
	if got := view.ProfileHarnessOverrideImagePullPolicy("", "claude"); got != "Always" {
		t.Fatalf("override pull policy %q", got)
	}
	if got := view.ProfileHarnessOverrideImage("", "gemini"); got != "" {
		t.Fatalf("gemini has no override image, got %q", got)
	}
	if got := view.ProfileHarnessOverrideImage("", ""); got != "" {
		t.Fatalf("no harness config, got %q", got)
	}
}

// FC5 `clone_depth`: the value is returned as written ("full" is an explicit value) with
// the instance path as its source.
func TestFlatAgentDefaults_CloneDepthValueAndSource(t *testing.T) {
	for _, depth := range []api.CloneDepth{"full", "3"} {
		view := legacyAgentDefaultsSettings().ForFlatInstance(agentDefaultsPolicy("k-1", &V1InstanceAgentDefaults{CloneDepth: depth}))
		got, src := view.ResolveCloneDepthWithSource("batch")
		if got != depth || src != "server.broker.instances[k-1].agent_defaults.clone_depth" {
			t.Fatalf("got (%q, %q), want (%q, the instance path)", got, src, depth)
		}
	}
}

// The legacy branches keep the former call sites' guards: an empty profile
// name with no active profile finds nothing, and a nil Profiles map is
// safe.
func TestTierAccessors_LegacyGuards(t *testing.T) {
	vs := legacyAgentDefaultsSettings()
	vs.ActiveProfile = ""
	if _, ok, _ := vs.TierHarnessOverride("", "claude"); ok {
		t.Error("TierHarnessOverride: an empty profile name must find nothing")
	}
	if keys, _ := vs.TierAllHarnessOverrideEnvKeys(""); keys != nil {
		t.Errorf("TierAllHarnessOverrideEnvKeys: got %v", keys)
	}
	if secrets, _ := vs.TierSecrets(""); secrets != nil {
		t.Errorf("TierSecrets: got %v", secrets)
	}
	if hc, _ := vs.TierDefaultHarnessConfig(""); hc != "" {
		t.Errorf("TierDefaultHarnessConfig: got %q", hc)
	}
	if res, hoRes, _ := vs.TierResources("", "claude"); res != nil || hoRes != nil {
		t.Errorf("TierResources: got (%v, %v)", res, hoRes)
	}
	// An explicit profile name still resolves without an active profile.
	if o, ok, src := vs.TierHarnessOverride("batch", "claude"); !ok || o.Image != "profile-image:v7" || src != "profiles.batch.harness_overrides.claude" {
		t.Errorf("TierHarnessOverride(batch) = (%+v, %v, %q)", o, ok, src)
	}
	noProfiles := &VersionedSettings{ActiveProfile: "batch"}
	if secrets, _ := noProfiles.TierSecrets(""); secrets != nil {
		t.Errorf("nil Profiles: TierSecrets got %v", secrets)
	}
	var nilVS *VersionedSettings
	if _, ok, _ := nilVS.TierHarnessOverride("batch", "claude"); ok {
		t.Error("nil settings: TierHarnessOverride must find nothing")
	}
	if res, err := ResolveHarnessConfigName(HarnessConfigInputs{Settings: nil, CLIFlag: "x"}); err != nil || res.Name != "x" {
		t.Errorf("nil settings: got (%+v, %v)", res, err)
	}
}

// The policy is a deep copy of agent_defaults: changing every map, slice
// and pointer of the loaded entry afterwards never changes the policy.
func TestNewFlatInstancePolicy_DeepCopiesAgentDefaults(t *testing.T) {
	d := fullAgentDefaults()
	p := agentDefaultsPolicy("docker-1", d)
	if !reflect.DeepEqual(p.Defaults, *fullAgentDefaults()) {
		t.Fatalf("policy defaults %+v", p.Defaults)
	}
	mutateAgentDefaults(d)
	if !reflect.DeepEqual(p.Defaults, *fullAgentDefaults()) {
		t.Fatalf("the policy shares state with the entry: %+v", p.Defaults)
	}
	if got := agentDefaultsPolicy("bare", nil); !reflect.DeepEqual(got.Defaults, V1InstanceAgentDefaults{}) {
		t.Fatalf("an entry without agent_defaults gives the zero defaults: %+v", got.Defaults)
	}
}

// mutateAgentDefaults changes every map, slice and pointer element of d in
// place.
func mutateAgentDefaults(d *V1InstanceAgentDefaults) {
	d.Volumes[0].Source = "/changed"
	d.Resources.Requests.CPU = "99"
	d.Secrets[0].Key = "CHANGED"
	d.Secrets[0].AlternativeEnvKeys[0] = "CHANGED"
	ho := d.HarnessOverrides["claude"]
	ho.Env["SHARED"] = "changed"
	ho.Volumes[0].Source = "/changed"
	ho.Resources.Limits.CPU = "99"
	d.HarnessOverrides["claude"] = V1HarnessOverride{Image: "changed"}
	d.HarnessOverrides["new"] = V1HarnessOverride{}
}

const agentDefaultsInstanceYAML = `schema_version: "1"
server:
  broker:
    instances:
      - key: docker-1
        name: docker-1
        runtime_target:
          type: docker
        agent_defaults:
          default_harness_config: instance-hc
          image_registry: registry.example/instance
          volumes:
            - source: /inst
              target: /mnt/inst
            - source: /inst2
              target: /mnt/inst2
              read_only: true
          resources:
            requests:
              cpu: 500m
              memory: 1Gi
          harness_overrides:
            claude:
              image: instance-image:v2
              image_pull_policy: Always
              user: instance-user
              auth_selected_type: vertex-ai
              env:
                SHARED: from-instance
                INSTANCE_EMPTY: ""
              volumes:
                - source: /ho
                  target: /mnt/ho
              resources:
                limits:
                  cpu: "2"
            gemini:
              env:
                OTHER_HC_EMPTY: ""
                OTHER_HC_SET: x
          secrets:
            - key: INSTANCE_SECRET
              description: instance secret
              type: environment
              alternative_env_keys: [ALT]
          clone_depth: full
`

// agent_defaults loads strictly, mirrors into the server config, converts
// back unchanged and passes the startup deep-equal comparison; the schema
// accepts it.
func TestRuntimeBrokerInstances_AgentDefaultsRoundTrip(t *testing.T) {
	if errs, err := ValidateSettings([]byte(agentDefaultsInstanceYAML), "1"); err != nil || len(errs) != 0 {
		t.Fatalf("schema: %v %v", errs, err)
	}
	global := flatTestHome(t)
	writeFlatTestFile(t, filepath.Join(global, "settings.yaml"), agentDefaultsInstanceYAML)
	strict, err := LoadRuntimeBrokerInstances("")
	if err != nil {
		t.Fatal(err)
	}
	if len(strict) != 1 || !reflect.DeepEqual(strict[0].AgentDefaults, fullAgentDefaults()) {
		t.Fatalf("strict load: got %+v", strict[0].AgentDefaults)
	}
	gc, err := LoadGlobalConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if len(gc.RuntimeBroker.Instances) != 1 || gc.RuntimeBroker.Instances[0].AgentDefaults == nil {
		t.Fatalf("server config mirror: got %+v", gc.RuntimeBroker.Instances)
	}
	if got := gc.RuntimeBroker.Instances[0].AgentDefaults; got.ImageRegistry != "registry.example/instance" ||
		got.CloneDepth != "full" || got.HarnessOverrides["claude"].Image != "instance-image:v2" {
		t.Fatalf("server config mirror: got %+v", got)
	}
	if !reflect.DeepEqual(RuntimeBrokerInstancesToGlobal(strict), gc.RuntimeBroker.Instances) {
		t.Fatalf("the startup comparison must find the strict and lenient results equal:\nstrict  %+v\nlenient %+v",
			RuntimeBrokerInstancesToGlobal(strict)[0].AgentDefaults, gc.RuntimeBroker.Instances[0].AgentDefaults)
	}
	if back := ConvertGlobalToV1ServerConfig(gc); !reflect.DeepEqual(back.Broker.Instances, strict) {
		t.Fatalf("back-conversion differs: %+v", back.Broker.Instances[0].AgentDefaults)
	}
}

// The server-config conversions deep-copy agent_defaults in both
// directions: nothing is shared between the two forms.
func TestRuntimeBrokerInstances_AgentDefaultsConversionDeepCopies(t *testing.T) {
	in := []V1RuntimeBrokerInstanceConfig{{Key: "d", Name: "d", RuntimeTarget: &V1RuntimeTargetConfig{Type: "docker"}, AgentDefaults: fullAgentDefaults()}}
	global := RuntimeBrokerInstancesToGlobal(in)
	back := globalInstancesToV1(global)
	if !reflect.DeepEqual(back, in) {
		t.Fatalf("round trip differs: %+v", back[0].AgentDefaults)
	}
	mutateAgentDefaults(in[0].AgentDefaults)
	if g := global[0].AgentDefaults; g.Volumes[0].Source != "/inst" || g.Resources.Requests.CPU != "500m" ||
		g.Secrets[0].AlternativeEnvKeys[0] != "ALT" || g.HarnessOverrides["claude"].Env["SHARED"] != "from-instance" ||
		g.HarnessOverrides["claude"].Volumes[0].Source != "/ho" || g.HarnessOverrides["claude"].Resources.Limits.CPU != "2" ||
		len(g.HarnessOverrides) != 2 {
		t.Fatalf("the server-config form shares state with the settings form: %+v", g)
	}
	mutateAgentDefaults(sharingV1View(global[0].AgentDefaults))
	if !reflect.DeepEqual(back[0].AgentDefaults, fullAgentDefaults()) {
		t.Fatalf("the settings form shares state with the server-config form: %+v", back[0].AgentDefaults)
	}
}

// sharingV1View views the mirror's fields as a V1InstanceAgentDefaults that
// shares its maps, slices and pointers, so mutateAgentDefaults can change
// the mirror in place.
func sharingV1View(a *InstanceAgentDefaultsConfig) *V1InstanceAgentDefaults {
	return &V1InstanceAgentDefaults{Volumes: a.Volumes, Resources: a.Resources, HarnessOverrides: a.HarnessOverrides, Secrets: a.Secrets}
}

// An unknown key anywhere in agent_defaults (the block, a harness override,
// a volume or a secret declaration) fails the strict load with its settings
// path; so do a non-string scalar and an invalid clone_depth.
func TestRuntimeBrokerInstances_AgentDefaultsStrictErrorsNamePath(t *testing.T) {
	const p = "server.broker.instances[0].agent_defaults"
	for name, tc := range map[string]struct{ from, to, want string }{
		"block key":        {"          clone_depth: full\n", "          clone_depth: full\n          default_template: t\n", p + ".default_template: unknown key"},
		"override key":     {"              user: instance-user\n", "              user: instance-user\n              usr: x\n", p + ".harness_overrides.claude.usr: unknown key"},
		"override volume":  {"                - source: /ho\n", "                - source: /ho\n                  sorce: x\n", p + ".harness_overrides.claude.volumes[0].sorce: unknown key"},
		"volume key":       {"              read_only: true\n", "              read_only: true\n              readonly: true\n", p + ".volumes[1].readonly: unknown key"},
		"resources key":    {"              memory: 1Gi\n", "              memory: 1Gi\n              gpu: \"1\"\n", p + ".resources.requests.gpu: unknown key"},
		"resources block":  {"          resources:\n            requests:\n", "          resources:\n            request: {}\n            requests:\n", p + ".resources.request: unknown key"},
		"override res key": {"                  cpu: \"2\"\n", "                  cpu: \"2\"\n                  gpus: \"1\"\n", p + ".harness_overrides.claude.resources.limits.gpus: unknown key"},
		"secret key":       {"              type: environment\n", "              type: environment\n              value: x\n", p + ".secrets[0].value: unknown key"},
		"numeric registry": {"          image_registry: registry.example/instance\n", "          image_registry: 5\n", p + ".image_registry must be a string"},
		"numeric env":      {"                SHARED: from-instance\n", "                SHARED: 1\n", p + ".harness_overrides.claude.env.SHARED must be a string"},
		"bad clone depth":  {"          clone_depth: full\n", "          clone_depth: shallow\n", p + ".clone_depth"},
	} {
		t.Run(name, func(t *testing.T) {
			if !strings.Contains(agentDefaultsInstanceYAML, tc.from) {
				t.Fatalf("fixture has no %q", tc.from)
			}
			global := flatTestHome(t)
			writeFlatTestFile(t, filepath.Join(global, "settings.yaml"), strings.Replace(agentDefaultsInstanceYAML, tc.from, tc.to, 1))
			_, err := LoadRuntimeBrokerInstances("")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

// agent_defaults values get the validation the same profile values get (the
// settings schema's definitions, through $defs/instanceAgentDefaults): an
// out-of-range image_pull_policy, auth_selected_type, volume type, secret
// type or clone_depth, or a volume without a target or a secret without a
// key, is refused, naming its settings path; valid values pass.
func TestValidateRuntimeBrokerInstances_AgentDefaultsValues(t *testing.T) {
	inst := V1RuntimeBrokerInstanceConfig{Key: "d", Name: "d", RuntimeTarget: &V1RuntimeTargetConfig{Type: "docker"},
		AgentDefaults: fullAgentDefaults()}
	if errs := ValidateRuntimeBrokerInstances([]V1RuntimeBrokerInstanceConfig{inst}); len(errs) != 0 {
		t.Fatalf("valid agent_defaults refused: %v", errs)
	}
	d := inst.AgentDefaults
	d.HarnessOverrides["claude"] = V1HarnessOverride{ImagePullPolicy: "Sometimes", AuthSelectedType: "magic",
		Volumes: []api.VolumeMount{{Source: "/s", Target: "/t", Type: "floppy"}, {Source: "/s"}}}
	d.Volumes = []api.VolumeMount{{Source: "/s", Target: "/t", Type: "floppy"}, {Source: "/s"}}
	d.Secrets = []api.RequiredSecret{{Key: "A", Type: "bogus"}, {Description: "no key"}}
	d.CloneDepth = "shallow"
	errs := ValidateRuntimeBrokerInstances([]V1RuntimeBrokerInstanceConfig{inst})
	const p = "server.broker.instances[0].agent_defaults"
	for _, want := range []string{
		p + ".harness_overrides.claude.image_pull_policy",
		p + ".harness_overrides.claude.auth_selected_type",
		p + ".harness_overrides.claude.volumes[0].type",
		p + ".harness_overrides.claude.volumes[1].target",
		p + ".volumes[0].type",
		p + ".volumes[1].target",
		p + ".secrets[0].type",
		p + ".secrets[1].key",
		p + ".clone_depth",
	} {
		found := false
		for _, e := range errs {
			found = found || e.Path == want
		}
		if !found {
			t.Errorf("no error at %s; got %v", want, errs)
		}
	}
	for _, e := range errs {
		if !strings.HasPrefix(e.Path, p) {
			t.Errorf("unexpected error %v", e)
		}
	}
}

func TestSettingsPathSuffix(t *testing.T) {
	for in, want := range map[string]string{
		"":            "",
		"clone_depth": ".clone_depth",
		"harness_overrides/claude/volumes/0/type": ".harness_overrides.claude.volumes[0].type",
		"harness_overrides/a~1b/env/X~0Y":         ".harness_overrides.a/b.env.X~Y",
	} {
		if got := settingsPathSuffix(in); got != want {
			t.Errorf("settingsPathSuffix(%q) = %q, want %q", in, got, want)
		}
	}
}
