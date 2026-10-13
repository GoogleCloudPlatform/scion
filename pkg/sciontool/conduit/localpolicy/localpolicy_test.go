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

package localpolicy

import "testing"

// TestReservedPortsList pins the deny-list contents (design §3.8).
func TestReservedPortsList(t *testing.T) {
	for _, p := range []int{9810, 18380} {
		if !IsReservedPort(p) {
			t.Errorf("port %d must be reserved", p)
		}
	}
	for _, p := range []int{0, 22, 80, 8080, 9811, 18381, 65535} {
		if IsReservedPort(p) {
			t.Errorf("port %d must not be reserved", p)
		}
	}
	if len(ReservedPorts) != 2 {
		t.Errorf("ReservedPorts = %v, want exactly 9810 and 18380", ReservedPorts)
	}
}

func TestReservedSocketsEmpty(t *testing.T) {
	if IsReservedSocket("/tmp/x.sock") {
		t.Error("no socket is reserved today")
	}
}
