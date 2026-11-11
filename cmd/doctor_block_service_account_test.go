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

package cmd

import (
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// TestCheckDoctorKubernetesBlockServiceAccount covers D10 (ptone/scion#4034):
// Kubernetes profiles with no kubernetes_block_service_account are listed as
// information, never as a warning or failure.
func TestCheckDoctorKubernetesBlockServiceAccount(t *testing.T) {
	vs := &config.VersionedSettings{
		Profiles: map[string]config.V1ProfileConfig{
			"team":  {Runtime: "gke", KubernetesBlockServiceAccount: "team-block"},
			"other": {Runtime: "gke"},
			"local": {Runtime: "docker"},
		},
		Runtimes: map[string]config.V1RuntimeConfig{
			"gke":    {Type: "kubernetes"},
			"docker": {Type: "docker"},
		},
	}
	res := checkDoctorKubernetesBlockServiceAccount(vs)
	if res.Status != "info" {
		t.Fatalf("Status = %q, want info", res.Status)
	}
	if listed := res.Message[strings.LastIndex(res.Message, ": ")+2:]; listed != "other" {
		t.Errorf("expected only profile 'other' to be listed, got %q", res.Message)
	}
	if !strings.Contains(res.Message, "local broker settings") {
		t.Errorf("expected the message to say it reflects the local broker settings, got %q", res.Message)
	}

	vs.Runtimes["gke"] = config.V1RuntimeConfig{Type: "kubernetes", KubernetesBlockServiceAccount: "entry-block"}
	if res := checkDoctorKubernetesBlockServiceAccount(vs); res.Status != "pass" {
		t.Errorf("with a runtime-entry block ServiceAccount, Status = %q (%s), want pass", res.Status, res.Message)
	}
	if res := checkDoctorKubernetesBlockServiceAccount(nil); res.Status != "pass" {
		t.Errorf("with no settings, Status = %q, want pass", res.Status)
	}
}
