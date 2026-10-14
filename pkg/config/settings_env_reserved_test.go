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
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// agentRefusedSampleNames covers the server prefix, the legacy session secret
// alias and both host credential aliases.
var agentRefusedSampleNames = []string{
	"SCION_SERVER_SESSION_SECRET",
	"SESSION_SECRET",
	"SCION_SERVER_HUB_CONDUIT_PEERSECRET",
	"SCION_SERVER_HUB_CONDUIT_PEER_SECRET",
	"SCION_DEV_TOKEN",
	"SCION_AUTH_TOKEN",
}

func TestExpandEnvMap_ReservedHostEnvExpandsEmpty(t *testing.T) {
	const sentinel = "reserved-host-value"
	t.Setenv("SCION_SERVER_SESSION_SECRET", sentinel)
	t.Setenv("SESSION_SECRET", sentinel)
	t.Setenv("ORDINARY_VAR", "ordinary")

	got := expandEnvMap(map[string]string{
		"A":                 "${SCION_SERVER_SESSION_SECRET}",
		"B":                 "$SESSION_SECRET",
		"C":                 "${ORDINARY_VAR}",
		"${SESSION_SECRET}": "key-expansion",
		"SESSION_SECRET":    "",
		"D":                 "x-${SESSION_SECRET}-${ORDINARY_VAR}",
	})
	for k, v := range got {
		if strings.Contains(k, sentinel) || strings.Contains(v, sentinel) {
			t.Errorf("expanded map carries a reserved host value under %q", k)
		}
	}
	if got["A"] != "" || got["B"] != "" {
		t.Errorf("A = %q, B = %q; want both empty", got["A"], got["B"])
	}
	if got["C"] != "ordinary" {
		t.Errorf("C = %q, want %q", got["C"], "ordinary")
	}
	if got["D"] != "x--ordinary" {
		t.Errorf("D = %q, want %q", got["D"], "x--ordinary")
	}
	if v, ok := got["SESSION_SECRET"]; !ok || v != "" {
		t.Errorf("SESSION_SECRET = %q, %v; want an empty marker kept as-is", v, ok)
	}
	if len(got) != 5 {
		t.Errorf("got %d keys, want 5 (the key-expansion entry dropped): %v", len(got), got)
	}
}

func TestExpandEnvMap_HostCredentialAliasesExpandEmpty(t *testing.T) {
	const sentinel = "host-credential-value"
	t.Setenv("SCION_DEV_TOKEN", sentinel)
	t.Setenv("SCION_AUTH_TOKEN", sentinel)
	got := expandEnvMap(map[string]string{"A": "${SCION_DEV_TOKEN}", "B": "$SCION_AUTH_TOKEN"})
	if got["A"] != "" || got["B"] != "" {
		t.Errorf("A has length %d, B has length %d; want both empty", len(got["A"]), len(got["B"]))
	}
}

func TestExpandVolumeMounts_RefusedHostEnvExpandsEmpty(t *testing.T) {
	const sentinel = "refused-host-value"
	for _, name := range agentRefusedSampleNames {
		t.Setenv(name, sentinel)
	}
	t.Setenv("ORDINARY_DIR", "/data")

	var vols []api.VolumeMount
	for _, name := range agentRefusedSampleNames {
		vols = append(vols, api.VolumeMount{Source: "/src/${" + name + "}", Target: "/dst/$" + name})
	}
	vols = append(vols, api.VolumeMount{Source: "${ORDINARY_DIR}/in", Target: "/mnt/${ORDINARY_DIR}"})

	got := expandVolumeMounts(vols)
	for i, v := range got[:len(agentRefusedSampleNames)] {
		if strings.Contains(v.Source, sentinel) || strings.Contains(v.Target, sentinel) {
			t.Errorf("%s: mount path carries a refused host value", agentRefusedSampleNames[i])
		}
		if v.Source != "/src/" || v.Target != "/dst/" {
			t.Errorf("%s: mount = %q:%q, want %q:%q", agentRefusedSampleNames[i], v.Source, v.Target, "/src/", "/dst/")
		}
	}
	last := got[len(got)-1]
	if last.Source != "/data/in" || last.Target != "/mnt//data" {
		t.Errorf("ordinary mount = %q:%q, want expansion", last.Source, last.Target)
	}
}

// TestGitTracked_ChildEnvOmitsRefusedNames runs gitTracked against a fake git
// that records its env, and checks the child did not inherit server secret or
// host credential names while ordinary variables still pass.
func TestGitTracked_ChildEnvOmitsRefusedNames(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake git script requires a POSIX shell")
	}
	for _, name := range agentRefusedSampleNames {
		t.Setenv(name, "refused-host-value")
	}
	t.Setenv("ORDINARY_HOST_VAR", "ordinary")

	binDir := t.TempDir()
	envOut := filepath.Join(t.TempDir(), "env.txt")
	script := "#!/bin/sh\nenv > '" + envOut + "'\nexit 0\n"
	if err := os.WriteFile(filepath.Join(binDir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	target := filepath.Join(t.TempDir(), "file")
	writeFile(t, target, "content")
	if !gitTracked(target) {
		t.Fatal("gitTracked = false; the fake git did not run")
	}

	data, err := os.ReadFile(envOut)
	if err != nil {
		t.Fatalf("read child env: %v", err)
	}
	names := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		if k, _, ok := strings.Cut(line, "="); ok {
			names[k] = true
		}
	}
	for _, name := range agentRefusedSampleNames {
		if names[name] {
			t.Errorf("git child inherited %s", name)
		}
	}
	if !names["ORDINARY_HOST_VAR"] {
		t.Error("git child did not inherit ORDINARY_HOST_VAR")
	}
}
