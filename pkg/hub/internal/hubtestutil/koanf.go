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

package hubtestutil

import (
	"testing"

	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/v2"
)

// EmptyKoanf returns an empty koanf instance with "." as the key delimiter.
func EmptyKoanf() *koanf.Koanf {
	return koanf.New(".")
}

// NewFileKoanf returns a koanf instance loaded from flat, standing in for
// file-sourced settings.
func NewFileKoanf(t *testing.T, flat map[string]interface{}) *koanf.Koanf {
	t.Helper()
	k := koanf.New(".")
	if err := k.Load(confmap.Provider(flat, "."), nil); err != nil {
		t.Fatalf("failed to build file koanf: %v", err)
	}
	return k
}

// NewEnvKoanf returns a koanf instance loaded from flat, standing in for
// environment-sourced settings.
func NewEnvKoanf(t *testing.T, flat map[string]interface{}) *koanf.Koanf {
	t.Helper()
	k := koanf.New(".")
	if err := k.Load(confmap.Provider(flat, "."), nil); err != nil {
		t.Fatalf("failed to build env koanf: %v", err)
	}
	return k
}
