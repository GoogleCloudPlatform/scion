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

package hub

import "testing"

func TestResolveGCPProjectID(t *testing.T) {
	orig := onGCE
	t.Cleanup(func() { onGCE = orig })
	probed := false
	onGCE = func() bool { probed = true; return false }

	if got, err := ResolveGCPProjectID("my-project"); err != nil || got != "my-project" || probed {
		t.Fatalf("configured: got %q, %v, probed=%v; want my-project, nil, false", got, err, probed)
	}
	if got, err := ResolveGCPProjectID(""); err == nil || got != "" || !probed {
		t.Fatalf("off GCP: got %q, %v, probed=%v; want an error after probing", got, err, probed)
	}
}
