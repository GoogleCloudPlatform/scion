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

package opsettings

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestValidate_SharedDirStorageBackendsKeys(t *testing.T) {
	docs := map[string]func(key, value string) string{
		"profiles": func(key, value string) string {
			return `{"gke": {"runtime": "k8s", "shared_dir_storage_backends": {"` + key + `": "` + value + `"}}}`
		},
		"runtimes": func(key, value string) string {
			return `{"k8s": {"type": "kubernetes", "shared_dir_storage_backends": {"` + key + `": "` + value + `"}}}`
		},
	}
	for section, doc := range docs {
		assert.Empty(t, Validate(section, json.RawMessage(doc("build-cache", "nfs"))), section)
		assert.NotEmpty(t, Validate(section, json.RawMessage(doc("Bad_Name", "local"))), section+": bad name")
		assert.NotEmpty(t, Validate(section, json.RawMessage(doc("notes", "disk"))), section+": bad value")
	}
}
