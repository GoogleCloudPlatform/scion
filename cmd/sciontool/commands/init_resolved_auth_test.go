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

package commands

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/stagedsecrets"
)

// Placeholder value only; not a real credential.
const resolvedAuthCanary = "placeholder-canary-api-key-7285"

// The init end of the resolved-auth path drops a reserved name again, even
// when the carry file holds one, and removes the file.
func TestTakeResolvedAuthEnv_DropsReservedNamesAtInit(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Written directly, bypassing the broker-side filter, as a planted or
	// faulty blob would arrive.
	if err := stagedsecrets.WriteResolvedAuthEnv(root, filepath.Join(root, "harness", "secrets"), map[string]string{
		stagedsecrets.HubTokenStagedEnvVar: "1",
		stagedsecrets.EnvVar:               "planted",
		"SCION_HOST_UID":                   "0",
		"SCION_HARNESS_SECRETS_DIR":        "/planted",
		"EXAMPLE_API_KEY":                  resolvedAuthCanary,
	}); err != nil {
		t.Fatalf("WriteResolvedAuthEnv: %v", err)
	}
	got, err := takeResolvedAuthEnv(root)
	if err != nil {
		t.Fatalf("takeResolvedAuthEnv: %v", err)
	}
	for _, name := range []string{stagedsecrets.HubTokenStagedEnvVar, stagedsecrets.EnvVar, "SCION_HOST_UID", "SCION_HARNESS_SECRETS_DIR"} {
		if _, ok := got[name]; ok {
			t.Errorf("reserved name %s reached the harness env entries", name)
		}
	}
	if got["EXAMPLE_API_KEY"] != resolvedAuthCanary {
		t.Error("control: the canary entry did not reach the harness env entries")
	}
	if _, err := os.Lstat(filepath.Join(root, stagedsecrets.ResolvedAuthEnvFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("carry file still present after init read it (lstat err=%v)", err)
	}
}

// Nothing staged: no entries, no error.
func TestTakeResolvedAuthEnv_NothingStaged(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	got, err := takeResolvedAuthEnv(root)
	if err != nil || got != nil {
		t.Errorf("takeResolvedAuthEnv with no carry file = %d entries, err %v; want none", len(got), err)
	}
	got, err = takeResolvedAuthEnv(filepath.Join(root, "not-mounted"))
	if err != nil || got != nil {
		t.Errorf("takeResolvedAuthEnv with no in-memory volume = %d entries, err %v; want none", len(got), err)
	}
}
