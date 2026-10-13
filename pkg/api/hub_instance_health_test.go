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

package api

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestNormalizeHubInstanceChecks_FixedValuesOnly(t *testing.T) {
	got := NormalizeHubInstanceChecks(map[string]string{
		"database":          "healthy",
		"workspace_storage": "unhealthy: mount not available",
		"colocated_broker":  "unhealthy: registration pending",
		"other":             "something else",
		"spare":             "Unavailable (detail)",
	})
	want := map[string]string{
		"database":          "healthy",
		"workspace_storage": "unhealthy",
		"colocated_broker":  "unhealthy",
		"other":             "unknown",
		"spare":             "unavailable",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestNormalizeHubInstanceChecks_DropsNamesOutsidePattern(t *testing.T) {
	got := NormalizeHubInstanceChecks(map[string]string{
		"ok_name_1":                  "healthy",
		strings.Repeat("a", 64):      "healthy",
		strings.Repeat("b", 65):      "healthy",
		"Upper":                      "healthy",
		"dash-name":                  "healthy",
		"dot.name":                   "healthy",
		"":                           "healthy",
		"workspace_storage_mount_ok": "unavailable: detail",
	})
	want := map[string]string{
		"ok_name_1":                  "healthy",
		strings.Repeat("a", 64):      "healthy",
		"workspace_storage_mount_ok": "unavailable",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestNormalizeHubInstanceChecks_CapsAtSixteenSorted(t *testing.T) {
	in := map[string]string{}
	for i := 0; i < 20; i++ {
		in[fmt.Sprintf("check_%02d", i)] = "healthy"
	}
	got := NormalizeHubInstanceChecks(in)
	if len(got) != BrokerHealthMaxChecks {
		t.Fatalf("len = %d, want %d", len(got), BrokerHealthMaxChecks)
	}
	for i := 0; i < BrokerHealthMaxChecks; i++ {
		if _, ok := got[fmt.Sprintf("check_%02d", i)]; !ok {
			t.Fatalf("check_%02d missing: the first names in sorted order are kept", i)
		}
	}
}

func TestNormalizeHubInstanceChecks_EmptyIsNil(t *testing.T) {
	if got := NormalizeHubInstanceChecks(nil); got != nil {
		t.Fatalf("got %v, want nil", got)
	}
	if got := NormalizeHubInstanceChecks(map[string]string{"Bad": "healthy"}); got != nil {
		t.Fatalf("got %v, want nil", got)
	}
}

// workspace_storage_mount_verification (36 characters) is an existing hub
// check; it must be kept and named, not dropped.
func TestNormalizeHubInstanceChecks_KeepsExistingLongHubCheckName(t *testing.T) {
	got := NormalizeHubInstanceChecks(map[string]string{
		"workspace_storage":                    "healthy",
		"workspace_storage_mount_verification": "unavailable: could not compare filesystem device IDs",
		strings.Repeat("x", 65):                "unhealthy",
	})
	want := map[string]string{
		"workspace_storage":                    "healthy",
		"workspace_storage_mount_verification": "unavailable",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}
