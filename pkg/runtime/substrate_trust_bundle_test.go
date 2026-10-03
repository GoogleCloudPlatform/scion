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

package runtime

import (
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/substratecaps"
	"github.com/GoogleCloudPlatform/scion/pkg/substrateenv"
	"github.com/GoogleCloudPlatform/scion/third_party/ateapipb"
)

// -----------------------------------------------------------------------
// egress_trust_bundle: golden template, off
// -----------------------------------------------------------------------

// TestBuildActorTemplate_EgressTrustBundleUnset_MatchesPreChangeGolden pins
// buildActorTemplate's output with EgressTrustBundle unset (the default)
// against the exact template shape the function produced before
// egress_trust_bundle existed: no system-info volume, no /run/ate mount,
// Env nil. This is the "off means exact current behaviour" golden test —
// want is hand-built to mirror the pre-change source, before
// egress_trust_bundle existed, not derived from the function under test, so
// a regression that silently changes the unset-case shape is caught here
// rather than compared against itself.
func TestBuildActorTemplate_EgressTrustBundleUnset_MatchesPreChangeGolden(t *testing.T) {
	image := "repo/image@sha256:" + strings.Repeat("c", 64)
	sc := config.V1SubstrateConfig{
		SandboxClass:      "microvm",
		SandboxConfigName: "cfg-1",
		WorkerSelector:    map[string]string{"pool": "x"},
		SnapshotStorage:   "gs://bucket/prefix/",
	}
	resources := &api.ResourceSpec{Limits: api.ResourceList{CPU: "2", Memory: "4Gi"}}

	got := buildActorTemplate("scion-test", "scion-abc123", image, sc, resources)

	want := &ateapipb.ActorTemplate{
		Metadata: &ateapipb.ResourceMetadata{Atespace: "scion-test", Name: "scion-abc123"},
		Containers: []*ateapipb.Container{
			{
				Name:    "scion-agent",
				Image:   image,
				Command: []string{"/usr/local/bin/sciontool", "substrate-serve"},
				Env:     nil,
				VolumeMounts: []*ateapipb.VolumeMount{
					{Name: "workspace", MountPath: "/workspace"},
				},
				SecurityContext: &ateapipb.SecurityContext{
					Capabilities: &ateapipb.Capabilities{Add: slices.Clone(substratecaps.Names())},
				},
				Resources: &ateapipb.Resources{Limits: []*ateapipb.Limits{
					{Name: "cpu", Quantity: "2"},
					{Name: "memory", Quantity: "4Gi"},
				}},
			},
		},
		Volumes: []*ateapipb.Volume{
			{Name: "workspace", DurableDir: &ateapipb.DurableDirVolumeSource{}},
		},
		SnapshotsConfig: &ateapipb.SnapshotsConfig{
			OnPause:         ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_DATA,
			OnCommit:        ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_DATA,
			StorageLocation: sc.SnapshotStorage,
		},
		SandboxConfig: &ateapipb.SandboxConfig{
			SandboxClass: ateapipb.SandboxClass_SANDBOX_CLASS_MICROVM,
			ConfigName:   sc.SandboxConfigName,
		},
		WorkerSelector: &ateapipb.Selector{MatchLabels: sc.WorkerSelector},
	}

	if !proto.Equal(got, want) {
		t.Errorf("buildActorTemplate() with EgressTrustBundle unset differs from the pre-change golden template:\ngot:  %s\nwant: %s", got, want)
	}
}

// TestBuildActorTemplate_EgressTrustBundleSet confirms the exact volume,
// mount, and five env vars buildActorTemplate adds when EgressTrustBundle
// is set — mirroring demos/egress/egress-mitm-template.yaml.tmpl
// (agent-substrate/substrate d277088b) and docs/egress-trust-bundle.md.
func TestBuildActorTemplate_EgressTrustBundleSet(t *testing.T) {
	image := "repo/image@sha256:" + strings.Repeat("d", 64)
	sc := config.V1SubstrateConfig{EgressTrustBundle: "egress-mitm.ate.dev"}

	tmpl := buildActorTemplate("scion-test", "scion-def456", image, sc, nil)

	wantVolumes := []*ateapipb.Volume{
		{Name: "workspace", DurableDir: &ateapipb.DurableDirVolumeSource{}},
		{
			Name: "system-info",
			SystemInfo: &ateapipb.SystemInfoVolumeSource{
				DataSources: []*ateapipb.SystemInfoDataSource{
					{
						TrustBundle: &ateapipb.TrustBundleDataSource{
							Name: "egress-mitm.ate.dev",
							Path: "trust-bundle.pem",
						},
					},
				},
			},
		},
	}
	if len(tmpl.GetVolumes()) != len(wantVolumes) {
		t.Fatalf("Volumes = %d, want %d: %v", len(tmpl.GetVolumes()), len(wantVolumes), tmpl.GetVolumes())
	}
	for i, want := range wantVolumes {
		if !proto.Equal(tmpl.GetVolumes()[i], want) {
			t.Errorf("Volumes[%d] = %s, want %s", i, tmpl.GetVolumes()[i], want)
		}
	}

	wantMounts := []*ateapipb.VolumeMount{
		{Name: "workspace", MountPath: "/workspace"},
		{Name: "system-info", MountPath: "/run/ate"},
	}
	if len(tmpl.GetContainers()) != 1 {
		t.Fatalf("Containers = %d, want 1", len(tmpl.GetContainers()))
	}
	gotMounts := tmpl.GetContainers()[0].GetVolumeMounts()
	if len(gotMounts) != len(wantMounts) {
		t.Fatalf("VolumeMounts = %d, want %d: %v", len(gotMounts), len(wantMounts), gotMounts)
	}
	for i, want := range wantMounts {
		if !proto.Equal(gotMounts[i], want) {
			t.Errorf("VolumeMounts[%d] = %s, want %s", i, gotMounts[i], want)
		}
	}

	wantEnv := []*ateapipb.EnvVar{
		{Name: "NODE_EXTRA_CA_CERTS", Value: "/run/ate/trust-bundle.pem"},
		{Name: "GIT_SSL_CAINFO", Value: "/run/ate/trust-bundle.pem"},
		{Name: "SSL_CERT_FILE", Value: "/run/ate/trust-bundle.pem"},
		{Name: "CURL_CA_BUNDLE", Value: "/run/ate/trust-bundle.pem"},
		{Name: "SSL_CERT_DIR", Value: "/run/ate"},
	}
	gotEnv := tmpl.GetContainers()[0].GetEnv()
	if len(gotEnv) != len(wantEnv) {
		t.Fatalf("Env = %d entries, want %d: %v", len(gotEnv), len(wantEnv), gotEnv)
	}
	for i, want := range wantEnv {
		if !proto.Equal(gotEnv[i], want) {
			t.Errorf("Env[%d] = %s, want %s", i, gotEnv[i], want)
		}
	}
}

// TestBuildActorTemplate_EnvNamesMatchSharedTrustBundleVarNames is the
// template-side half of the tie between buildActorTemplate's Env and
// pkg/sciontool/substrate's own trustBundleEnvPairs candidate list: both
// derive from substrateenv.TrustBundleVarNames directly, so a name added to
// the template without updating that shared slice (or vice versa) is
// caught here rather than surfacing as exec silently losing a var the
// template already carries.
func TestBuildActorTemplate_EnvNamesMatchSharedTrustBundleVarNames(t *testing.T) {
	image := "repo/image@sha256:" + strings.Repeat("f", 64)
	sc := config.V1SubstrateConfig{EgressTrustBundle: "egress-mitm.ate.dev"}

	tmpl := buildActorTemplate("scion-test", "scion-fedcba", image, sc, nil)

	gotNames := make([]string, len(tmpl.GetContainers()[0].GetEnv()))
	for i, e := range tmpl.GetContainers()[0].GetEnv() {
		gotNames[i] = e.GetName()
	}
	if !slices.Equal(gotNames, substrateenv.TrustBundleVarNames) {
		t.Errorf("buildActorTemplate() Env names = %v, want substrateenv.TrustBundleVarNames = %v", gotNames, substrateenv.TrustBundleVarNames)
	}
}

// -----------------------------------------------------------------------
// substrateTemplateName: unchanged when unset, different when set
// -----------------------------------------------------------------------

// substrateTemplateNameFixture is one row of the table-driven
// substrateTemplateName tests below: an image/config/resources input, and
// the literal name substrateTemplateName produces for it with
// EgressTrustBundle unset, computed once (before egress_trust_bundle
// existed as a hash input in the "base" case, and from the current,
// already-shipped hash formula for the others, since they were added after
// the field existed and so cannot pin a "pre-change" value — see each row's
// comment).
type substrateTemplateNameFixture struct {
	name      string
	image     string
	sc        config.V1SubstrateConfig
	resources *api.ResourceSpec
	wantUnset string
}

func substrateTemplateNameFixtures() []substrateTemplateNameFixture {
	return []substrateTemplateNameFixture{
		{
			// The original fixture: pins the literal computed before
			// egress_trust_bundle existed as a hash input at all, against
			// the exact image/sandbox/config-name/snapshot-storage/
			// resources shape substrateTemplateName has always hashed. A
			// regression that starts hashing EgressTrustBundle
			// unconditionally (e.g. always as a 10th "%s" field, "" when
			// unset) would change this literal even though the setting is
			// off — exactly the silent golden-template reuse break this
			// pin exists to catch.
			name:  "base",
			image: "repo/image@sha256:" + strings.Repeat("e", 64),
			sc: config.V1SubstrateConfig{
				SandboxClass:      "gvisor",
				SandboxConfigName: "gvisor-default",
				SnapshotStorage:   "gs://bucket/prefix/",
			},
			resources: &api.ResourceSpec{Limits: api.ResourceList{CPU: "2", Memory: "4Gi"}},
			wantUnset: "scion-c31a0b3817b7",
		},
		{
			// Worker selector set, resources nil (so buildActorTemplate's
			// own nil->BuiltinDefaultResources() substitution is exercised
			// in the hash input too). Strengthens the plain-install pin
			// across a second, materially different fixture shape.
			name:  "worker selector, nil resources",
			image: "repo/image@sha256:" + strings.Repeat("g", 64),
			sc: config.V1SubstrateConfig{
				SandboxClass:      "gvisor",
				SandboxConfigName: "gvisor-default",
				WorkerSelector:    map[string]string{"pool": "scion-agents"},
				SnapshotStorage:   "gs://bucket/prefix/",
			},
			resources: nil,
			wantUnset: "scion-780d9b998ec6",
		},
	}
}

// TestBuildActorTemplate_LaunchArgv0IsAlwaysAbsolute is the guard test for
// the substrate actor's own launch entrypoint: PID 1's argv[0] is exec'd by
// whatever starts the container, before any of this codebase's own code
// (including pkg/sciontool/rootexec's fixed-PATH resolver) ever runs, so a
// bare name here would be looked up against the container's own PATH —
// which on this runtime includes a workload-owned directory ahead of any
// trusted system directory (see the rootexec package doc comment for the
// general shape of that hazard). Every container in the template, across
// the fixture table, must have an absolute argv[0]. Kubernetes's own
// "sciontool provision"/"sciontool" init-container argv are deliberately
// out of scope: that runtime rebuilds its container from a fresh image
// layer on every restart, so there is no persistent, workload-writable
// PATH entry for a bare name there to resolve against.
func TestBuildActorTemplate_LaunchArgv0IsAlwaysAbsolute(t *testing.T) {
	for _, tc := range substrateTemplateNameFixtures() {
		t.Run(tc.name, func(t *testing.T) {
			tmpl := buildActorTemplate("scion-test", "scion-abc123", tc.image, tc.sc, tc.resources)
			for _, c := range tmpl.Containers {
				if len(c.Command) == 0 {
					t.Fatalf("container %q has an empty Command", c.Name)
				}
				if !strings.HasPrefix(c.Command[0], "/") {
					t.Errorf("container %q Command[0] = %q, want an absolute path", c.Name, c.Command[0])
				}
			}
		})
	}
}

// TestSubstrateTemplateName_UnchangedWhenEgressTrustBundleUnset pins the
// literal name substrateTemplateName produces, with EgressTrustBundle
// unset, across a table of fixture configs (the original fixture plus one
// with a worker selector and nil resources).
func TestSubstrateTemplateName_UnchangedWhenEgressTrustBundleUnset(t *testing.T) {
	for _, tc := range substrateTemplateNameFixtures() {
		t.Run(tc.name, func(t *testing.T) {
			got := substrateTemplateName(tc.image, tc.sc, tc.resources)
			if got != tc.wantUnset {
				t.Errorf("substrateTemplateName() with EgressTrustBundle unset = %q, want pinned value %q", got, tc.wantUnset)
			}
		})
	}
}

// TestSubstrateTemplateName_ChangesWhenEgressTrustBundleSet confirms
// setting EgressTrustBundle changes the template's content-address, across
// the same fixture table — an existing golden template built before the
// setting was turned on must not be silently reused once it is, since it
// lacks the volume/mount/env a resumed or newly-scheduled actor would
// otherwise need.
func TestSubstrateTemplateName_ChangesWhenEgressTrustBundleSet(t *testing.T) {
	for _, tc := range substrateTemplateNameFixtures() {
		t.Run(tc.name, func(t *testing.T) {
			before := substrateTemplateName(tc.image, tc.sc, tc.resources)

			sc := tc.sc
			sc.EgressTrustBundle = "egress-mitm.ate.dev"
			after := substrateTemplateName(tc.image, sc, tc.resources)

			if before == after {
				t.Error("substrateTemplateName() did not change when EgressTrustBundle was set — an existing golden template would be silently reused without the trust-bundle volume/mount/env")
			}
		})
	}
}
