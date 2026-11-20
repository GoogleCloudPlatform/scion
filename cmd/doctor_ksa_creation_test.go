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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/assert"
)

func TestDoctorCreateServiceAccounts_ReadsGlobalSettingsOnly(t *testing.T) {
	effective := &config.VersionedSettings{
		ActiveProfile: "gke",
		Profiles: map[string]config.V1ProfileConfig{
			"gke":        {Runtime: "k8s-create"},
			"project-on": {Runtime: "k8s-project-only"},
		},
		Runtimes: map[string]config.V1RuntimeConfig{
			"k8s-create":       {Type: "kubernetes", CreateServiceAccounts: true},
			"k8s-project-only": {Type: "kubernetes", CreateServiceAccounts: true},
		},
	}
	global := &config.VersionedSettings{
		Runtimes: map[string]config.V1RuntimeConfig{
			"k8s-create":       {Type: "kubernetes", CreateServiceAccounts: true},
			"k8s-project-only": {Type: "kubernetes", CreateServiceAccounts: false},
		},
	}

	assert.True(t, doctorCreateServiceAccounts(effective, global, ""), "active profile whose global runtime entry sets create_service_accounts")
	assert.True(t, doctorCreateServiceAccounts(effective, global, "gke"))
	assert.False(t, doctorCreateServiceAccounts(effective, global, "project-on"), "project-only create_service_accounts must not enable doctor check")
	assert.False(t, doctorCreateServiceAccounts(effective, global, "missing"))
	assert.False(t, doctorCreateServiceAccounts(nil, global, "gke"))
	assert.False(t, doctorCreateServiceAccounts(effective, nil, "gke"))
}
