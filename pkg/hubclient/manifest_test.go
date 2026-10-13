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

package hubclient

import (
	"reflect"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

func TestNewManifestBuilder_UsesSharedDefaultExcludes(t *testing.T) {
	saved := transfer.DefaultExcludePatterns
	t.Cleanup(func() { transfer.DefaultExcludePatterns = saved })

	// Use a distinct shared list so the test detects a hard-coded copy
	// rather than passing because the values happen to match.
	transfer.DefaultExcludePatterns = append(append([]string{}, saved...), "shared-default-sentinel")

	b := NewManifestBuilder(t.TempDir())
	if !reflect.DeepEqual(b.IgnorePatterns, transfer.DefaultExcludePatterns) {
		t.Fatalf("IgnorePatterns = %v, want shared defaults %v", b.IgnorePatterns, transfer.DefaultExcludePatterns)
	}

	// The template manifest builder and a plain transfer builder must agree.
	tb := transfer.NewManifestBuilder(t.TempDir())
	if !reflect.DeepEqual(b.IgnorePatterns, tb.ExcludePatterns) {
		t.Fatalf("IgnorePatterns = %v, transfer builder ExcludePatterns = %v", b.IgnorePatterns, tb.ExcludePatterns)
	}
}

func TestNewManifestBuilder_DoesNotAliasSharedDefaults(t *testing.T) {
	want := append([]string{}, transfer.DefaultExcludePatterns...)

	b := NewManifestBuilder(t.TempDir())
	if len(b.IgnorePatterns) > 0 {
		b.IgnorePatterns[0] = "mutated"
	}
	b.IgnorePatterns = append(b.IgnorePatterns, "extra")

	if !reflect.DeepEqual(transfer.DefaultExcludePatterns, want) {
		t.Fatalf("shared defaults changed to %v, want %v", transfer.DefaultExcludePatterns, want)
	}
}
