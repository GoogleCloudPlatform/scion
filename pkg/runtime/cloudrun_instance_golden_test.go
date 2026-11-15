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
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

var updateInstanceGolden = flag.Bool("update-instance-golden", false, "rewrite testdata/cloudrun_instance_golden from the current buildCloudRunInstance output")

// goldenInstanceCfg is a Cloud Run start with env, labels, resources and a
// raw command: everything buildCloudRunInstance turns into the instance.
func goldenInstanceCfg() RunConfig {
	return RunConfig{
		Image:              "us-docker.pkg.dev/p/r/scion-claude:latest",
		ContainerWorkspace: "/workspace",
		Env:                append([]string{"FOO=bar"}, clonePerAgentEnv...),
		Labels:             map[string]string{"agent_id": crTestAgentID, "scion.name": crTestSlug, "scion.run_id": "run-1"},
		CommandArgs:        []string{"sleep", "infinity"},
	}
}

func goldenInstancePaths() *cloudRunNFSProvisionPaths {
	return &cloudRunNFSProvisionPaths{
		server:              "10.0.0.2",
		workspaceExportPath: "/share/projects/p1/agents/" + crTestSlug + "/workspace",
		homeExportPath:      "/share/projects/p1/agents/" + crTestSlug + "/home-" + crTestAgentID,
	}
}

// instanceJSON renders inst as indented JSON with sorted keys, stable
// across builds (protojson's own spacing is not).
func instanceJSON(t *testing.T, inst *runpb.Instance) []byte {
	t.Helper()
	raw, err := protojson.Marshal(inst)
	if err != nil {
		t.Fatal(err)
	}
	var v interface{}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(out, '\n')
}

func checkInstanceGolden(t *testing.T, name string, cfg RunConfig) {
	t.Helper()
	rt, err := NewCloudRunRuntime(&config.CloudRunConfig{ProjectID: "p", Location: "us-central1", ServiceAccount: "sa@p.iam.gserviceaccount.com"})
	if err != nil {
		t.Fatal(err)
	}
	inst, err := rt.buildCloudRunInstance(cfg, 1000, 1000, goldenInstancePaths())
	if err != nil {
		t.Fatal(err)
	}
	got := instanceJSON(t, inst)
	file := filepath.Join("testdata", "cloudrun_instance_golden", name+".json")
	if *updateInstanceGolden {
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read golden (regenerate with -update-instance-golden): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("instance differs from %s:\n%s", file, got)
	}
}

// TestCloudRunInstanceGolden_Persistent: a persistent instance is
// byte-identical to the one built before persistence existed (the golden
// was generated from the base commit), with persistence unset or set to
// persistent.
func TestCloudRunInstanceGolden_Persistent(t *testing.T) {
	checkInstanceGolden(t, "persistent", goldenInstanceCfg())
}

// TestCloudRunInstanceGolden_PersistentExplicit: persistence: persistent
// builds the same instance as no persistence.
func TestCloudRunInstanceGolden_PersistentExplicit(t *testing.T) {
	cfg := goldenInstanceCfg()
	cfg.Persistence = api.PersistencePersistent
	checkInstanceGolden(t, "persistent", cfg)
}

// TestCheckPersistenceSupported: no runtime runs an ephemeral agent until
// one implements EphemeralPersistenceRuntime; persistent runs anywhere.
func TestCheckPersistenceSupported(t *testing.T) {
	for _, rt := range []Runtime{&CloudRunRuntime{}, &DockerRuntime{}} {
		if err := CheckPersistenceSupported(rt, api.PersistenceEphemeral); err == nil || !strings.Contains(err.Error(), "ephemeral persistence is not supported") {
			t.Errorf("%T ephemeral = %v, want a refusal", rt, err)
		}
		if err := CheckPersistenceSupported(rt, api.PersistencePersistent); err != nil {
			t.Errorf("%T persistent: %v", rt, err)
		}
	}
}
