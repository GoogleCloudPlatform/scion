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

package hostedguard

import (
	"errors"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

func TestCheckMemberConfig_Volumes(t *testing.T) {
	agentCfg := Origin{Kind: OriginAgentConfig}
	tests := []struct {
		name        string
		vol         api.VolumeMount
		runtimeKind string
		wantRefused bool
		wantReason  string
	}{
		{name: "socket bind", vol: api.VolumeMount{Source: "/var/run/docker.sock", Target: "/var/run/docker.sock"}, runtimeKind: "docker", wantRefused: true, wantReason: "host volumes"},
		{name: "etc bind read-only", vol: api.VolumeMount{Source: "/etc", Target: "/host-etc", ReadOnly: true}, runtimeKind: "docker", wantRefused: true, wantReason: "host volumes"},
		{name: "proc bind explicit local", vol: api.VolumeMount{Type: "local", Source: "/proc", Target: "/p"}, runtimeKind: "podman", wantRefused: true, wantReason: "host volumes"},
		{name: "local type mixed case", vol: api.VolumeMount{Type: " Local ", Source: "/etc", Target: "/e"}, runtimeKind: "docker", wantRefused: true, wantReason: "host volumes"},
		{name: "host bind on kubernetes", vol: api.VolumeMount{Source: "/etc", Target: "/e"}, runtimeKind: RuntimeKubernetes, wantRefused: true, wantReason: "host volumes"},
		{name: "host bind runtime unknown", vol: api.VolumeMount{Source: "/etc", Target: "/e"}, wantRefused: true, wantReason: "host volumes"},
		{name: "nfs", vol: api.VolumeMount{Type: "nfs", Server: "10.0.0.1", Source: "/export", Target: "/n"}, runtimeKind: "docker", wantRefused: true, wantReason: "nfs volumes"},
		{name: "nfs on kubernetes", vol: api.VolumeMount{Type: "nfs", Server: "10.0.0.1", Source: "/export", Target: "/n"}, runtimeKind: RuntimeKubernetes, wantRefused: true, wantReason: "nfs volumes"},
		{name: "cloudrun volume", vol: api.VolumeMount{Type: "cloudrun-volume", VolumeName: "v", Target: "/c"}, runtimeKind: "cloudrun", wantRefused: true, wantReason: "cloudrun-volume volumes"},
		{name: "gke shared volume", vol: api.VolumeMount{Type: "gke-shared-volume", VolumeName: "v", Target: "/g"}, runtimeKind: RuntimeKubernetes, wantRefused: true, wantReason: "gke-shared-volume volumes"},
		{name: "unknown type", vol: api.VolumeMount{Type: "tmpfs", Target: "/t"}, runtimeKind: RuntimeKubernetes, wantRefused: true, wantReason: "volumes of this type"},
		{name: "gcs on docker", vol: api.VolumeMount{Type: "gcs", Bucket: "b", Target: "/b"}, runtimeKind: "docker", wantRefused: true, wantReason: "only for agents run on Kubernetes"},
		{name: "gcs on podman", vol: api.VolumeMount{Type: "gcs", Bucket: "b", Target: "/b"}, runtimeKind: "podman", wantRefused: true, wantReason: "only for agents run on Kubernetes"},
		{name: "gcs on apple container", vol: api.VolumeMount{Type: "gcs", Bucket: "b", Target: "/b"}, runtimeKind: "container", wantRefused: true, wantReason: "only for agents run on Kubernetes"},
		{name: "gcs on kubernetes", vol: api.VolumeMount{Type: "gcs", Bucket: "b", Target: "/b"}, runtimeKind: RuntimeKubernetes},
		{name: "gcs upper case on kubernetes", vol: api.VolumeMount{Type: "GCS", Bucket: "b", Target: "/b"}, runtimeKind: RuntimeKubernetes, wantRefused: true, wantReason: "volume type must be spelled"},
		{name: "gcs upper case runtime unknown", vol: api.VolumeMount{Type: "GCS", Bucket: "b", Target: "/b"}, wantRefused: true, wantReason: "volume type must be spelled"},
		{name: "gcs upper case on docker", vol: api.VolumeMount{Type: "GCS", Bucket: "b", Target: "/b"}, runtimeKind: "docker", wantRefused: true, wantReason: "volume type must be spelled"},
		{name: "gcs mixed case runtime unknown", vol: api.VolumeMount{Type: "Gcs", Bucket: "b", Target: "/b"}, wantRefused: true, wantReason: "volume type must be spelled"},
		{name: "gcs mixed case on docker", vol: api.VolumeMount{Type: "Gcs", Bucket: "b", Target: "/b"}, runtimeKind: "docker", wantRefused: true, wantReason: "volume type must be spelled"},
		{name: "gcs padded runtime unknown", vol: api.VolumeMount{Type: " gcs", Bucket: "b", Target: "/b"}, wantRefused: true, wantReason: "volume type must be spelled"},
		{name: "gcs runtime unknown", vol: api.VolumeMount{Type: "gcs", Bucket: "b", Target: "/b"}},
		{name: "gcs runtime undetermined", vol: api.VolumeMount{Type: "gcs", Bucket: "b", Target: "/b"}, runtimeKind: RuntimeUndetermined, wantRefused: true, wantReason: "runtime could not be determined"},
		{name: "host bind runtime undetermined", vol: api.VolumeMount{Source: "/etc", Target: "/e"}, runtimeKind: RuntimeUndetermined, wantRefused: true, wantReason: "host volumes"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckMemberConfig(&api.ScionConfig{Volumes: []api.VolumeMount{tt.vol}}, agentCfg, tt.runtimeKind)
			if !tt.wantRefused {
				if err != nil {
					t.Fatalf("CheckMemberConfig() = %v, want nil", err)
				}
				return
			}
			var re *RefusalError
			if !errors.As(err, &re) {
				t.Fatalf("CheckMemberConfig() = %v, want *RefusalError", err)
			}
			if re.Field != "volume" || re.Entry != tt.vol.Target || re.Origin != agentCfg {
				t.Errorf("refusal = %+v, want field volume, entry %q, origin %v", re, tt.vol.Target, agentCfg)
			}
			msg := err.Error()
			if !strings.Contains(msg, tt.wantReason) {
				t.Errorf("message %q does not contain %q", msg, tt.wantReason)
			}
			if !strings.Contains(msg, "in agent config") {
				t.Errorf("message %q does not name the origin", msg)
			}
		})
	}
}

// The message names the volume target, never the host source, NFS server
// or bucket.
func TestCheckMemberConfig_MessageNamesTargetOnly(t *testing.T) {
	vols := []api.VolumeMount{
		{Source: "/var/run/secret-host-path", Target: "/data"},
		{Type: "nfs", Server: "nfs.internal.example", Source: "/secret-export", Target: "/data"},
		{Type: "gcs", Bucket: "secret-bucket", Prefix: "secret-prefix", Target: "/data"},
	}
	for _, v := range vols {
		err := CheckMemberConfig(&api.ScionConfig{Volumes: []api.VolumeMount{v}}, Origin{Kind: OriginAgentConfig}, "docker")
		if err == nil {
			t.Fatalf("volume %+v not refused", v)
		}
		msg := err.Error()
		if !strings.HasPrefix(msg, `volume "/data": `) {
			t.Errorf("message %q does not start with the target", msg)
		}
		for _, secret := range []string{"secret", "nfs.internal"} {
			if strings.Contains(msg, secret) {
				t.Errorf("message %q echoes %q", msg, secret)
			}
		}
	}
}

func TestCheckMemberConfig_DesignMessage(t *testing.T) {
	err := CheckMemberConfig(&api.ScionConfig{Volumes: []api.VolumeMount{{Source: "/srv", Target: "/data"}}}, Origin{Kind: OriginAgentConfig}, "")
	want := `volume "/data": host volumes in agent config are not allowed on a hosted server; use a project shared directory or ask the server operator to configure the volume`
	if err == nil || err.Error() != want {
		t.Fatalf("CheckMemberConfig() = %v, want %q", err, want)
	}
}

func TestCheckMemberConfig_NamedOrigin(t *testing.T) {
	err := CheckMemberConfig(&api.ScionConfig{Volumes: []api.VolumeMount{{Source: "/srv", Target: "/data"}}}, Origin{Kind: OriginHarnessConfig, Name: "claude"}, "docker")
	if err == nil || !strings.Contains(err.Error(), `in harness config "claude"`) {
		t.Fatalf("CheckMemberConfig() = %v, want the named origin", err)
	}
}

// The first refused volume is reported; allowed ones before it are skipped.
func TestCheckMemberConfig_FirstRefusedVolume(t *testing.T) {
	cfg := &api.ScionConfig{Volumes: []api.VolumeMount{
		{Type: "gcs", Bucket: "b", Target: "/ok"},
		{Source: "/etc", Target: "/first"},
		{Source: "/proc", Target: "/second"},
	}}
	var re *RefusalError
	if err := CheckMemberConfig(cfg, Origin{Kind: OriginAgentConfig}, RuntimeKubernetes); !errors.As(err, &re) || re.Entry != "/first" {
		t.Fatalf("CheckMemberConfig() = %v, want a refusal of /first", err)
	}
}

func TestCheckMemberConfig_NothingToCheck(t *testing.T) {
	for _, cfg := range []*api.ScionConfig{nil, {}, {Volumes: []api.VolumeMount{}}, {Harness: "claude", Env: map[string]string{"A": "b"}}, {Hub: &api.AgentHubConfig{}}} {
		if err := CheckMemberConfig(cfg, Origin{Kind: OriginAgentConfig}, "docker"); err != nil {
			t.Errorf("CheckMemberConfig(%+v) = %v, want nil", cfg, err)
		}
	}
}

func TestCheckMemberConfig_Env(t *testing.T) {
	tests := []struct {
		name       string
		env        map[string]string
		wantEntry  string // "" = allowed
		wantReason string
	}{
		{name: "braced reference", env: map[string]string{"AWS_KEY": "${AWS_SECRET_ACCESS_KEY}"}, wantEntry: "AWS_KEY", wantReason: "host environment references"},
		{name: "bare reference", env: map[string]string{"K": "$HOME"}, wantEntry: "K", wantReason: "host environment references"},
		{name: "embedded reference", env: map[string]string{"K": "prefix-${X}-suffix"}, wantEntry: "K", wantReason: "host environment references"},
		{name: "special var", env: map[string]string{"K": "a$1"}, wantEntry: "K", wantReason: "host environment references"},
		{name: "malformed braces removed by expansion", env: map[string]string{"K": "a${}b"}, wantEntry: "K", wantReason: "host environment references"},
		{name: "unterminated brace", env: map[string]string{"K": "a${b"}, wantEntry: "K", wantReason: "host environment references"},
		{name: "reference in name", env: map[string]string{"${HOST_NAME}": "name-value"}, wantEntry: "${HOST_NAME}", wantReason: "host environment references"},
		{name: "empty passthrough", env: map[string]string{"GITHUB_TOKEN": ""}, wantEntry: "GITHUB_TOKEN", wantReason: "empty passthrough"},
		{name: "scion name", env: map[string]string{"SCION_HUB_URL": "https://x"}, wantEntry: "SCION_HUB_URL", wantReason: "names reserved"},
		{name: "scion auth token", env: map[string]string{"SCION_AUTH_TOKEN": "tok-value"}, wantEntry: "SCION_AUTH_TOKEN", wantReason: "names reserved"},
		{name: "scion metadata", env: map[string]string{"SCION_METADATA_MODE": "passthrough"}, wantEntry: "SCION_METADATA_MODE", wantReason: "names reserved"},
		{name: "gce metadata", env: map[string]string{"GCE_METADATA_HOST": "10.0.0.1"}, wantEntry: "GCE_METADATA_HOST", wantReason: "names reserved"},
		{name: "first refused in name order", env: map[string]string{"B": "", "A": "ok", "C": "$X"}, wantEntry: "B", wantReason: "empty passthrough"},
		{name: "literal value", env: map[string]string{"FOO": "bar"}},
		{name: "lone dollar at end", env: map[string]string{"PRICE": "5$"}},
		{name: "dollar before space", env: map[string]string{"K": "a $ b"}},
		{name: "lower-case scion prefix is not reserved", env: map[string]string{"scion_x": "v"}},
		{name: "auto-expose ports", env: map[string]string{"SCION_AUTO_EXPOSE_PORTS": "true"}},
		{name: "auto-expose mode", env: map[string]string{"SCION_AUTO_EXPOSE_MODE": "list"}},
		{name: "auto-expose list", env: map[string]string{"SCION_AUTO_EXPOSE_PORTS_LIST": "8080"}},
		{name: "auto-expose interval", env: map[string]string{"SCION_AUTO_EXPOSE_INTERVAL": "5s"}},
		{name: "cli log level", env: map[string]string{"SCION_LOG_LEVEL": "debug"}},
		{name: "cli telemetry", env: map[string]string{"SCION_TELEMETRY_ENABLED": "false"}},
		{name: "liveness depth is harness-config only", env: map[string]string{"SCION_LIVENESS_HARNESS_ROOT_DEPTH": "2"}, wantEntry: "SCION_LIVENESS_HARNESS_ROOT_DEPTH", wantReason: "names reserved"},
		{name: "exempt name still needs a literal value", env: map[string]string{"SCION_LOG_LEVEL": "${LVL}"}, wantEntry: "SCION_LOG_LEVEL", wantReason: "host environment references"},
		{name: "exempt name may be empty", env: map[string]string{"SCION_AUTO_EXPOSE_PORTS_LIST": ""}},
		{name: "exempt names may be empty", env: map[string]string{"SCION_TELEMETRY_ENABLED": "", "SCION_LOG_LEVEL": ""}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckMemberConfig(&api.ScionConfig{Env: tt.env}, Origin{Kind: OriginAgentConfig}, "docker")
			if tt.wantEntry == "" {
				if err != nil {
					t.Fatalf("CheckMemberConfig() = %v, want nil", err)
				}
				return
			}
			var re *RefusalError
			if !errors.As(err, &re) {
				t.Fatalf("CheckMemberConfig() = %v, want *RefusalError", err)
			}
			if re.Field != "env" || re.Entry != tt.wantEntry {
				t.Errorf("refused %s %q, want env %q", re.Field, re.Entry, tt.wantEntry)
			}
			if !strings.Contains(re.Reason, tt.wantReason) || !strings.Contains(re.Reason, "agent config") {
				t.Errorf("reason %q, want %q and the origin", re.Reason, tt.wantReason)
			}
			if v := tt.env[tt.wantEntry]; v != "" && strings.Contains(err.Error(), v) {
				t.Errorf("message %q echoes the value %q", err.Error(), v)
			}
		})
	}
}

func TestCheckMemberEnv_MessageNeverHasValue(t *testing.T) {
	const secret = "s3cr3t-value"
	for _, env := range []map[string]string{
		{"K": "${" + secret + "}"},
		{"K": secret + "$X"},
		{"SCION_HUB_ENDPOINT": "http://" + secret},
		{"GCE_METADATA_HOST": secret},
	} {
		err := CheckMemberEnv(env, Origin{Kind: OriginHarnessConfig, Name: "claude"})
		if err == nil {
			t.Fatalf("env %v not refused", env)
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("message %q echoes the value", err.Error())
		}
		if !strings.HasPrefix(err.Error(), `env "`) || !strings.Contains(err.Error(), `in harness config "claude"`) {
			t.Errorf("message %q does not name the env name and origin", err.Error())
		}
	}
}

func TestCheckMemberConfig_EnvDesignMessage(t *testing.T) {
	err := CheckMemberEnv(map[string]string{"AWS_KEY": "${AWS_KEY}"}, Origin{Kind: OriginHarnessConfig, Name: "claude"})
	want := `env "AWS_KEY": host environment references and empty passthrough values in harness config "claude" are not allowed on a hosted server; use a literal value or a secret`
	if err == nil || err.Error() != want {
		t.Fatalf("CheckMemberEnv() = %v, want %q", err, want)
	}
}

func TestCheckMemberConfig_Hub(t *testing.T) {
	const endpoint = "http://127.0.0.1:8080/secret-path"
	for _, tt := range []struct {
		name    string
		hub     *api.AgentHubConfig
		refused bool
	}{
		{name: "no hub block", hub: nil},
		{name: "empty hub block", hub: &api.AgentHubConfig{}},
		{name: "loopback endpoint", hub: &api.AgentHubConfig{Endpoint: endpoint}, refused: true},
		{name: "public endpoint", hub: &api.AgentHubConfig{Endpoint: "https://hub.example.com"}, refused: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckMemberConfig(&api.ScionConfig{Hub: tt.hub}, Origin{Kind: OriginAgentConfig}, "")
			if !tt.refused {
				if err != nil {
					t.Fatalf("CheckMemberConfig() = %v, want nil", err)
				}
				return
			}
			var re *RefusalError
			if !errors.As(err, &re) || re.Field != "hub" || re.Entry != "endpoint" {
				t.Fatalf("CheckMemberConfig() = %v, want a hub endpoint refusal", err)
			}
			want := `hub "endpoint": a hub endpoint in agent config is not allowed on a hosted server; the server sets the agent's hub connection`
			if err.Error() != want {
				t.Errorf("message %q, want %q", err.Error(), want)
			}
			if strings.Contains(err.Error(), "127.0.0.1") || strings.Contains(err.Error(), "example.com") {
				t.Errorf("message %q echoes the endpoint", err.Error())
			}
		})
	}
}

// Each rule refuses on its own; volumes are checked first, then env, then hub.
func TestCheckMemberConfig_RuleOrder(t *testing.T) {
	cfg := &api.ScionConfig{
		Volumes: []api.VolumeMount{{Source: "/etc", Target: "/e"}},
		Env:     map[string]string{"K": ""},
		Hub:     &api.AgentHubConfig{Endpoint: "http://h"},
	}
	o := Origin{Kind: OriginAgentConfig}
	for _, want := range []string{"volume", "env", "hub"} {
		var re *RefusalError
		if err := CheckMemberConfig(cfg, o, "docker"); !errors.As(err, &re) || re.Field != want {
			t.Fatalf("CheckMemberConfig() = %v, want a %s refusal", err, want)
		}
		switch want {
		case "volume":
			cfg.Volumes = nil
		case "env":
			cfg.Env = nil
		}
	}
}

func TestIsReservedEnvName(t *testing.T) {
	for name, want := range map[string]bool{
		"SCION_HUB_ENDPOINT": true, "SCION_": true, "GCE_METADATA_IP": true,
		"SCION_AUTO_EXPOSE_PORTS": true, "SCION_LOG_LEVEL": true, "SCION_TELEMETRY_ENABLED": true,
		"SCION_LOG_LEVEL_X": true, "PATH": false, "MY_SCION_X": false,
	} {
		if got := IsReservedEnvName(name); got != want {
			t.Errorf("IsReservedEnvName(%q) = %v, want %v", name, got, want)
		}
	}
}

// Reserved names members may set depend on the source (api.MemberSettableEnv):
// agent config uses the create scope, a harness config its own scope, and a
// template none.
func TestCheckMemberEnv_ScopeByOrigin(t *testing.T) {
	agentCfg := Origin{Kind: OriginAgentConfig}
	hc := Origin{Kind: OriginHarnessConfig, Name: "codex"}
	tpl := Origin{Kind: OriginTemplate, Name: "t"}
	for _, tc := range []struct {
		name    string
		origin  Origin
		refused bool
	}{
		{"SCION_AUTO_EXPOSE_PORTS", agentCfg, false},
		{"SCION_AUTO_EXPOSE_PORTS", hc, false},
		{"SCION_LOG_LEVEL", agentCfg, false},
		{"SCION_TELEMETRY_ENABLED", agentCfg, false},
		{"SCION_LOG_LEVEL", hc, true},
		{"SCION_TELEMETRY_ENABLED", hc, true},
		{"SCION_LIVENESS_HARNESS_ROOT_DEPTH", hc, false},
		{"SCION_LIVENESS_HARNESS_ROOT_DEPTH", agentCfg, true},
		{"SCION_AUTO_EXPOSE_PORTS", tpl, true},
		{"SCION_HUB_ENDPOINT", hc, true},
	} {
		err := CheckMemberEnv(map[string]string{tc.name: "1"}, tc.origin)
		if refused := err != nil; refused != tc.refused {
			t.Errorf("%s from %s: refused = %v (%v), want %v", tc.name, tc.origin, refused, err, tc.refused)
		}
	}
	// Every listed name passes from each source its scope lists.
	for _, e := range api.MemberSettableEnv() {
		for origin, scope := range map[Origin]api.MemberEnvScope{agentCfg: api.MemberEnvCreate, hc: api.MemberEnvHarnessConfig} {
			if e.Scope&scope == 0 {
				continue
			}
			if err := CheckMemberEnv(map[string]string{e.Name: "1"}, origin); err != nil {
				t.Errorf("%s from %s refused: %v", e.Name, origin, err)
			}
		}
	}
}

func TestCheckMemberEnvTemplate(t *testing.T) {
	hc := Origin{Kind: OriginHarnessConfig, Name: "copilot"}
	const secret = "s3cr3t-template-value"
	for _, tc := range []struct {
		name    string
		origin  Origin
		refused bool
	}{
		{"SCION_HUB_ENDPOINT", hc, true},
		{"SCION_AUTH_TOKEN", hc, true},
		{"GCE_METADATA_HOST", hc, true},
		{"SCION_AGENT_NAME", hc, false},
		{"SCION_AUTO_EXPOSE_PORTS", hc, false},
		{"NO_COLOR", hc, false},
		{"SCION_AGENT_NAME", Origin{Kind: OriginTemplate, Name: "t"}, true},
	} {
		err := CheckMemberEnvTemplate(map[string]string{tc.name: "{{ .AgentName }}" + secret}, tc.origin)
		if refused := err != nil; refused != tc.refused {
			t.Errorf("%s from %s: refused = %v (%v), want %v", tc.name, tc.origin, refused, err, tc.refused)
			continue
		}
		if err == nil {
			continue
		}
		var re *RefusalError
		if !errors.As(err, &re) || re.Field != "env_template" || re.Entry != tc.name {
			t.Errorf("refusal = %v, want env_template %q", err, tc.name)
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("message %q echoes the value", err.Error())
		}
	}
	// Values are not checked: env_template values never come from the host.
	if err := CheckMemberEnvTemplate(map[string]string{"X": "${HOME}", "Y": ""}, hc); err != nil {
		t.Errorf("non-reserved names refused: %v", err)
	}
	if err := CheckMemberEnvTemplate(nil, hc); err != nil {
		t.Errorf("nil env_template: %v", err)
	}
}
