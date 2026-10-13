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

package brokeridentity

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

func dockerScope(daemon, endpoint string) ExecutionScope {
	return ExecutionScope{Type: TargetTypeDocker, Docker: &DockerScope{DaemonID: daemon, Endpoint: endpoint}}
}

func instanceDir(t *testing.T) string {
	t.Helper()
	return InstanceDir(t.TempDir(), "local-docker")
}

func TestIdentity_FirstBootMintsAndPersistsAcrossRestart(t *testing.T) {
	dir := instanceDir(t)
	first, err := LoadOrCreate(dir, "local-docker", TargetTypeDocker, dockerScope("D1", "unix:///var/run/docker.sock"), nil)
	if err != nil {
		t.Fatalf("first boot: %v", err)
	}
	if first.RuntimeBrokerID == "" || first.RuntimeTarget.ID == "" || first.RuntimeBrokerID == first.RuntimeTarget.ID {
		t.Fatalf("expected two distinct minted IDs, got %+v", first)
	}
	if first.RuntimeTarget.Type != TargetTypeDocker || first.InstanceKey != "local-docker" || first.SchemaVersion != SchemaVersion {
		t.Fatalf("unexpected identity: %+v", first)
	}
	info, err := os.Stat(filepath.Join(dir, IdentityFileName))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("identity file mode = %v, want 0600", info.Mode().Perm())
	}
	dinfo, _ := os.Stat(dir)
	if dinfo.Mode().Perm() != 0o700 {
		t.Fatalf("instance dir mode = %v, want 0700", dinfo.Mode().Perm())
	}
	// Simulated restart: same IDs.
	second, err := LoadOrCreate(dir, "local-docker", TargetTypeDocker, dockerScope("D1", "unix:///var/run/docker.sock"), nil)
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	if second.RuntimeBrokerID != first.RuntimeBrokerID || second.RuntimeTarget.ID != first.RuntimeTarget.ID {
		t.Fatalf("IDs changed across restart: %+v vs %+v", first, second)
	}
}

func TestIdentity_EmptyDirectoryIsFirstBoot(t *testing.T) {
	dir := instanceDir(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreate(dir, "local-docker", TargetTypeDocker, dockerScope("D1", ""), nil); err != nil {
		t.Fatalf("empty dir must be first boot: %v", err)
	}
}

func TestIdentity_TempLeftoversAreFirstBoot(t *testing.T) {
	dir := instanceDir(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{lockFileName, tempFilePrefix + "123"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := LoadOrCreate(dir, "local-docker", TargetTypeDocker, dockerScope("D1", ""), nil); err != nil {
		t.Fatalf("lock and temp leftovers must be first boot: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, tempFilePrefix+"123")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("leftover temp file should be removed, stat err = %v", err)
	}
}

func TestIdentity_ConcurrentFirstBootYieldsOneIdentity(t *testing.T) {
	dir := instanceDir(t)
	const n = 8
	ids := make([]*Identity, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ids[i], errs[i] = LoadOrCreate(dir, "local-docker", TargetTypeDocker, dockerScope("D1", ""), nil)
		}(i)
	}
	wg.Wait()
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: %v", i, errs[i])
		}
		if ids[i].RuntimeBrokerID != ids[0].RuntimeBrokerID || ids[i].RuntimeTarget.ID != ids[0].RuntimeTarget.ID {
			t.Fatalf("concurrent first boot minted more than one identity: %+v vs %+v", ids[0], ids[i])
		}
	}
}

func TestIdentity_MissingWithLeftoverStateIsError(t *testing.T) {
	dir := instanceDir(t)
	if err := os.MkdirAll(filepath.Join(dir, "hub-credentials"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hub-credentials", "hub.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadOrCreate(dir, "local-docker", TargetTypeDocker, dockerScope("D1", ""), nil)
	if !errors.Is(err, ErrIdentityMissing) {
		t.Fatalf("want ErrIdentityMissing, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, IdentityFileName)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("identity must not be re-minted when other state exists")
	}
}

func writeIdentityFile(t *testing.T, dir string, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, IdentityFileName), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func validIdentityJSON(t *testing.T, mutate func(*Identity)) string {
	t.Helper()
	id := Identity{
		SchemaVersion:   SchemaVersion,
		InstanceKey:     "local-docker",
		RuntimeBrokerID: "b-1",
		RuntimeTarget:   api.RuntimeTargetDescriptor{ID: "t-1", Type: TargetTypeDocker},
		ExecutionScope:  dockerScope("D1", ""),
	}
	if mutate != nil {
		mutate(&id)
	}
	b, err := json.Marshal(id)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestIdentity_CorruptFileIsError(t *testing.T) {
	for name, content := range map[string]string{
		"invalid json":    "{not json",
		"empty broker id": validIdentityJSON(t, func(i *Identity) { i.RuntimeBrokerID = "" }),
		"empty target id": validIdentityJSON(t, func(i *Identity) { i.RuntimeTarget.ID = "" }),
		"no scope":        validIdentityJSON(t, func(i *Identity) { i.ExecutionScope = ExecutionScope{} }),
	} {
		t.Run(name, func(t *testing.T) {
			dir := instanceDir(t)
			writeIdentityFile(t, dir, content)
			_, err := LoadOrCreate(dir, "local-docker", TargetTypeDocker, dockerScope("D1", ""), nil)
			if !errors.Is(err, ErrIdentityCorrupt) {
				t.Fatalf("want ErrIdentityCorrupt, got %v", err)
			}
			got, _ := os.ReadFile(filepath.Join(dir, IdentityFileName))
			if string(got) != content {
				t.Fatal("a corrupt identity file must never be rewritten")
			}
		})
	}
}

func TestIdentity_UnknownSchemaVersionIsError(t *testing.T) {
	dir := instanceDir(t)
	writeIdentityFile(t, dir, validIdentityJSON(t, func(i *Identity) { i.SchemaVersion = 99 }))
	if _, err := LoadOrCreate(dir, "local-docker", TargetTypeDocker, dockerScope("D1", ""), nil); !errors.Is(err, ErrIdentityCorrupt) {
		t.Fatalf("want ErrIdentityCorrupt, got %v", err)
	}
}

func TestIdentity_KeyMismatchIsError(t *testing.T) {
	dir := instanceDir(t)
	writeIdentityFile(t, dir, validIdentityJSON(t, func(i *Identity) { i.InstanceKey = "other" }))
	if _, err := LoadOrCreate(dir, "local-docker", TargetTypeDocker, dockerScope("D1", ""), nil); !errors.Is(err, ErrIdentityKeyMismatch) {
		t.Fatalf("want ErrIdentityKeyMismatch, got %v", err)
	}
}

func TestIdentity_TargetTypeChangeIsError(t *testing.T) {
	dir := instanceDir(t)
	writeIdentityFile(t, dir, validIdentityJSON(t, nil))
	_, err := LoadOrCreate(dir, "local-docker", TargetTypeKubernetes,
		ExecutionScope{Type: TargetTypeKubernetes, Kubernetes: &KubernetesScope{ClusterUID: "c", Namespace: "n"}}, nil)
	if !errors.Is(err, ErrRuntimeTargetTypeChanged) {
		t.Fatalf("want ErrRuntimeTargetTypeChanged, got %v", err)
	}
}

func TestIdentity_CollidesWithAnyLegacyBrokerIDIsError(t *testing.T) {
	dir := instanceDir(t)
	writeIdentityFile(t, dir, validIdentityJSON(t, nil))
	_, err := LoadOrCreate(dir, "local-docker", TargetTypeDocker, dockerScope("D1", ""), []string{"legacy-a", "b-1"})
	if !errors.Is(err, ErrIdentityCollidesWithLegacy) {
		t.Fatalf("want ErrIdentityCollidesWithLegacy, got %v", err)
	}
	if _, err := LoadOrCreate(dir, "local-docker", TargetTypeDocker, dockerScope("D1", ""), []string{"legacy-a"}); err != nil {
		t.Fatalf("no collision expected: %v", err)
	}
}

func TestIdentity_FirstBootRequiresScopeIdentity(t *testing.T) {
	dir := instanceDir(t)
	_, err := LoadOrCreate(dir, "local-docker", TargetTypeDocker, dockerScope("", "unix:///var/run/docker.sock"), nil)
	if !errors.Is(err, ErrExecutionScopeUnidentified) {
		t.Fatalf("want ErrExecutionScopeUnidentified, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, IdentityFileName)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("no identity may be minted without a scope identity")
	}
}

func TestIdentity_InvalidKeyRejected(t *testing.T) {
	if _, err := LoadOrCreate(t.TempDir(), "Bad_Key", TargetTypeDocker, dockerScope("D1", ""), nil); err == nil {
		t.Fatal("invalid key must be rejected")
	}
}

// TestIdentity_LinkEEXISTLoadsWinner exercises the publish step when another
// process already linked identity.json: the winner's identity is returned and
// never replaced.
func TestIdentity_LinkEEXISTLoadsWinner(t *testing.T) {
	dir := instanceDir(t)
	writeIdentityFile(t, dir, validIdentityJSON(t, nil))
	got, err := mint(dir, filepath.Join(dir, IdentityFileName), "local-docker", TargetTypeDocker, dockerScope("D1", ""))
	if err != nil {
		t.Fatal(err)
	}
	if got.RuntimeBrokerID != "b-1" || got.RuntimeTarget.ID != "t-1" {
		t.Fatalf("the existing (winning) identity must be returned, got %+v", got)
	}
	if entries, _ := filepath.Glob(filepath.Join(dir, tempFilePrefix+"*")); len(entries) != 0 {
		t.Fatalf("temp files left behind: %v", entries)
	}
}

func TestIdentity_ExistingDirectoryTightenedTo0700(t *testing.T) {
	dir := instanceDir(t)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreate(dir, "local-docker", TargetTypeDocker, dockerScope("D1", ""), nil); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(dir)
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("instance dir mode = %v, want 0700", info.Mode().Perm())
	}
}

func TestIdentity_MissingErrorMessage(t *testing.T) {
	dir := instanceDir(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "other"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadOrCreate(dir, "local-docker", TargetTypeDocker, dockerScope("D1", ""), nil)
	if !errors.Is(err, ErrIdentityMissing) ||
		!contains(err.Error(), `identity state for instance "local-docker" is missing but other state exists; restore it or remove the directory to register a new Runtime Broker`) {
		t.Fatalf("frozen message not used: %v", err)
	}
}
