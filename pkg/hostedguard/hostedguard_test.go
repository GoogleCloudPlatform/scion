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
		{name: "gcs upper case on kubernetes", vol: api.VolumeMount{Type: "GCS", Bucket: "b", Target: "/b"}, runtimeKind: RuntimeKubernetes},
		{name: "gcs runtime unknown", vol: api.VolumeMount{Type: "gcs", Bucket: "b", Target: "/b"}},
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
	for _, cfg := range []*api.ScionConfig{nil, {}, {Volumes: []api.VolumeMount{}}, {Harness: "claude", Env: map[string]string{"A": "b"}}} {
		if err := CheckMemberConfig(cfg, Origin{Kind: OriginAgentConfig}, "docker"); err != nil {
			t.Errorf("CheckMemberConfig(%+v) = %v, want nil", cfg, err)
		}
	}
}
