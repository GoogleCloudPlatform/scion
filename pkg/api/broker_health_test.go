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

// rawNFSError is the shape of an NFS mount failure as the broker's
// nfs_mounts check reports it to /healthz.
const rawNFSError = "unhealthy: ws1: mount failed: mount 10.0.0.2:/export on /mnt/nfs/ws1 failed: exit status 32 (output: mount.nfs: access denied by server)"

func TestNormalizeBrokerHealthReport_Nil(t *testing.T) {
	if got := NormalizeBrokerHealthReport(nil); got != nil {
		t.Errorf("NormalizeBrokerHealthReport(nil) = %+v, want nil", got)
	}
}

func TestNormalizeBrokerHealthReport_Status(t *testing.T) {
	for in, want := range map[string]string{
		"healthy":                   "healthy",
		"degraded":                  "degraded",
		"unhealthy":                 "unhealthy",
		"Degraded":                  "degraded",
		" unhealthy: db down":       "unhealthy",
		"":                          "unknown",
		"available":                 "unknown", // a check value, not a status
		"on fire":                   "unknown",
		"healthyish":                "unknown",
		"degraded/../../etc/passwd": "unknown",
		"degraded(runtime)":         "degraded",
		"degraded\nx":               "degraded",
		"degraded\rx":               "degraded",
		"degraded (runtime)":        "degraded",
		"degraded;runtime":          "degraded",
		"degraded,runtime":          "degraded",
		"degraded\tcause":           "degraded",
		strings.Repeat("x", 500):    "unknown",
		rawNFSError:                 "unhealthy",
	} {
		got := NormalizeBrokerHealthReport(&BrokerHealthReport{Status: in})
		if got.Status != want {
			t.Errorf("status %q -> %q, want %q", in, got.Status, want)
		}
	}
}

// Check values keep only their leading fixed word: no free text (server,
// export, mount path, command output) survives.
func TestNormalizeBrokerHealthReport_CheckValuesFixed(t *testing.T) {
	got := NormalizeBrokerHealthReport(&BrokerHealthReport{
		Status: "degraded",
		Checks: map[string]string{
			"nfs_mounts": rawNFSError,
			"runtime":    "unavailable",
			"docker":     "Available",
			"other":      "pending: waiting for 10.0.0.2",
			"empty":      "",
			"word_x":     "healthy_x",
			"word_dash":  "available-not",
			"word_digit": "healthy123",
			"word_utf8":  "healthyé",
			"word_dot":   "available.",
			"word_cause": "unavailable (no runtime)",
		},
	})
	want := map[string]string{
		"nfs_mounts": "unhealthy",
		"runtime":    "unavailable",
		"docker":     "available",
		"other":      "unknown",
		"empty":      "unknown",
		"word_x":     "unknown",
		"word_dash":  "unknown",
		"word_digit": "unknown",
		"word_utf8":  "unknown",
		"word_dot":   "unknown",
		"word_cause": "unavailable",
	}
	if !reflect.DeepEqual(got.Checks, want) {
		t.Errorf("checks = %v, want %v", got.Checks, want)
	}
	for k, v := range got.Checks {
		if !brokerHealthCheckValues[v] {
			t.Errorf("checks[%q] = %q is not a fixed value", k, v)
		}
	}
}

// Check names outside [A-Za-z0-9_.-]{1,64} are dropped, not truncated, so
// two names can never collide into one.
func TestNormalizeBrokerHealthReport_CheckNames(t *testing.T) {
	got := NormalizeBrokerHealthReport(&BrokerHealthReport{
		Status: "healthy",
		Checks: map[string]string{
			"cloudrun-sandbox":       "available",
			"k8s.v1_x":               "available",
			"":                       "available",
			"has space":              "available",
			"/mnt/nfs/ws1":           "healthy",
			"10.0.0.2:/export":       "healthy",
			strings.Repeat("a", 64):  "available",
			strings.Repeat("a", 65):  "unavailable",
			strings.Repeat("a", 200): "unavailable",
			"é":                      "available",
		},
	})
	want := map[string]string{
		"cloudrun-sandbox":      "available",
		"k8s.v1_x":              "available",
		strings.Repeat("a", 64): "available",
	}
	if !reflect.DeepEqual(got.Checks, want) {
		t.Errorf("checks = %v, want %v", got.Checks, want)
	}
}

// At most BrokerHealthMaxChecks checks are kept: the first in sorted name
// order, the same on every call.
func TestNormalizeBrokerHealthReport_CapsChecks(t *testing.T) {
	in := &BrokerHealthReport{Status: "healthy", Checks: map[string]string{}}
	for i := 0; i < BrokerHealthMaxChecks+10; i++ {
		in.Checks[fmt.Sprintf("check_%02d", i)] = "healthy"
	}
	in.Checks["bad name"] = "healthy" // dropped before the cap, never counted

	first := NormalizeBrokerHealthReport(in)
	if len(first.Checks) != BrokerHealthMaxChecks {
		t.Fatalf("kept %d checks, want %d", len(first.Checks), BrokerHealthMaxChecks)
	}
	for i := 0; i < BrokerHealthMaxChecks; i++ {
		if _, ok := first.Checks[fmt.Sprintf("check_%02d", i)]; !ok {
			t.Errorf("check_%02d dropped, want the first %d sorted names kept", i, BrokerHealthMaxChecks)
		}
	}
	for i := 0; i < 50; i++ {
		if again := NormalizeBrokerHealthReport(in); !reflect.DeepEqual(again, first) {
			t.Fatalf("call %d = %v, want the same result as the first call %v", i, again.Checks, first.Checks)
		}
	}
	if len(in.Checks) != BrokerHealthMaxChecks+11 {
		t.Error("the input report was modified")
	}
}

func TestNormalizeBrokerHealthReport_EmptyChecksNil(t *testing.T) {
	for _, checks := range []map[string]string{nil, {}, {"bad name": "healthy"}} {
		got := NormalizeBrokerHealthReport(&BrokerHealthReport{Status: "healthy", Checks: checks})
		if got.Checks != nil {
			t.Errorf("checks %v -> %v, want nil", checks, got.Checks)
		}
	}
}

// The cap bounds the stored broker row. It is pinned here so a change to
// it is deliberate.
func TestBrokerHealthLimitsPinned(t *testing.T) {
	if BrokerHealthMaxChecks != 16 {
		t.Errorf("BrokerHealthMaxChecks = %d, want 16", BrokerHealthMaxChecks)
	}
	if BrokerHealthMaxNameChars != 64 {
		t.Errorf("BrokerHealthMaxNameChars = %d, want 64", BrokerHealthMaxNameChars)
	}
}
