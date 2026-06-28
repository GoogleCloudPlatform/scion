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

package runtime

import (
	"testing"
)

func TestAppleDNSConstants(t *testing.T) {
	if AppleDNSHostname != "host.containers.internal" {
		t.Errorf("AppleDNSHostname = %q, want %q", AppleDNSHostname, "host.containers.internal")
	}
	if AppleDNSIP != "203.0.113.1" {
		t.Errorf("AppleDNSIP = %q, want %q", AppleDNSIP, "203.0.113.1")
	}
}
