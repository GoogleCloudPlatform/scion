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

package initenv

import (
	"os"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/envpolicy"
)

func TestLoad_UnsetsMarkerAndCombinesWithStaticDenylist(t *testing.T) {
	t.Setenv(envpolicy.HookWithheldEnvVar, "BUNDLE_KEY,PATH")
	t.Setenv("SCION_HOOK_UNKNOWN_FUTURE_VAR", "ignored")

	pol := Load()

	if _, present := os.LookupEnv(envpolicy.HookWithheldEnvVar); present {
		t.Fatalf("%s was not unset by Load()", envpolicy.HookWithheldEnvVar)
	}
	if pol.Mode != envpolicy.HookEnvModeDenylist {
		t.Errorf("Mode = %q, want %q", pol.Mode, envpolicy.HookEnvModeDenylist)
	}
	for _, want := range []string{"BUNDLE_KEY", "PATH", "LD_PRELOAD", "GIT_CONFIG_COUNT"} {
		if !pol.Withheld.Withholds(want) {
			t.Errorf("Withheld.Withholds(%q) = false, want true", want)
		}
	}
	if pol.Withheld.Withholds("OPERATOR_KEY") {
		t.Error("Withheld.Withholds(OPERATOR_KEY) = true, want false")
	}
}

func TestLoad_WithoutMarkerAppliesStaticDenylistOnly(t *testing.T) {
	t.Setenv(envpolicy.HookWithheldEnvVar, "")
	_ = os.Unsetenv(envpolicy.HookWithheldEnvVar)

	pol := Load()

	if pol.Mode != envpolicy.HookEnvModeDenylist {
		t.Errorf("Mode = %q, want %q", pol.Mode, envpolicy.HookEnvModeDenylist)
	}
	if !pol.Withheld.Withholds("LD_PRELOAD") {
		t.Error("expected static denylist entry LD_PRELOAD to be withheld")
	}
	if pol.Withheld.Withholds("BUNDLE_KEY") {
		t.Error("expected BUNDLE_KEY not to be withheld when marker is absent")
	}
	if got := pol.Withheld.Extra(); len(got) != 0 {
		t.Errorf("Extra() = %v, want empty", got)
	}
}
