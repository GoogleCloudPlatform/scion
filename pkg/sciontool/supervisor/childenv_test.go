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

package supervisor

import (
	"slices"
	"testing"
)

// TestChildEnv pins the harness child's environment assembly: user
// identity when dropping privileges, SCION_EXTRA_PATH prepended to PATH
// and removed, the overlay merged without overriding the runtime env,
// and secret overrides replacing entries.
func TestChildEnv(t *testing.T) {
	base := []string{"PATH=/usr/bin", "SCION_EXTRA_PATH=/opt/x", "HOME=/root", "KEEP=runtime", "API_KEY="}
	cfg := Config{
		UID: 1000, GID: 1000, Username: "scion",
		EnvOverlay:      map[string]string{"KEEP": "overlay", "NEW": "n"},
		SecretOverrides: map[string]string{"API_KEY": "fetched"},
	}
	got, err := ChildEnv(cfg, base)
	if err != nil {
		t.Fatal(err)
	}
	for _, kv := range []string{"PATH=/opt/x:/usr/bin", "HOME=/home/scion", "USER=scion", "LOGNAME=scion", "KEEP=runtime", "NEW=n", "API_KEY=fetched"} {
		if !slices.Contains(got, kv) {
			t.Errorf("ChildEnv lacks %s: %v", kv, got)
		}
	}
	for _, kv := range got {
		if kv == "SCION_EXTRA_PATH=/opt/x" || kv == "HOME=/root" || kv == "API_KEY=" {
			t.Errorf("ChildEnv keeps %s", kv)
		}
	}
	if base[0] != "PATH=/usr/bin" {
		t.Error("ChildEnv modified its base")
	}

	// No privilege drop and not rootless: the identity is left alone.
	got, err = ChildEnv(Config{Username: "scion"}, []string{"HOME=/root"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"HOME=/root"}) {
		t.Errorf("ChildEnv without a drop = %v", got)
	}
}
