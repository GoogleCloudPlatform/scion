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
	"slices"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

// TestTransferDefaultExcludes_DotScion keeps transfer.DefaultExcludePatterns,
// which writes the workspace-root .scion exclude as a literal, in step with
// this package's DotScion name for the entry. It lives here, not in
// pkg/transfer, because pkg/config already imports pkg/transfer; checking it
// from pkg/transfer's tests would make that test build link pkg/config.
// pkg/transfer's TestDefaultExcludes_RootDotScion covers how the pattern
// matches.
func TestTransferDefaultExcludes_DotScion(t *testing.T) {
	if DotScion != ".scion" {
		t.Fatalf("config.DotScion = %q; update transfer.DefaultExcludePatterns", DotScion)
	}
	if !slices.Contains(transfer.DefaultExcludePatterns, DotScion+"/**") {
		t.Errorf("DefaultExcludePatterns %v lacks %q", transfer.DefaultExcludePatterns, DotScion+"/**")
	}
	if slices.Contains(transfer.DefaultExcludePatterns, DotScion) {
		t.Errorf("DefaultExcludePatterns contains bare %q, which also matches nested entries", DotScion)
	}
}
