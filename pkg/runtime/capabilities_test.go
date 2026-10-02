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

import "testing"

// attachCapableRuntime is a MockRuntime that also implements the optional
// AttachCapableRuntime capability, reporting the given value.
type attachCapableRuntime struct {
	*MockRuntime
	supportsAttach bool
}

func (r *attachCapableRuntime) SupportsAttach() bool { return r.supportsAttach }

var _ AttachCapableRuntime = (*attachCapableRuntime)(nil)

// A runtime that implements AttachCapableRuntime and reports true is
// supported.
func TestHasAttachSupport_FieldPresentTrue(t *testing.T) {
	rt := &attachCapableRuntime{
		MockRuntime:    &MockRuntime{NameFunc: func() string { return "fake" }},
		supportsAttach: true,
	}
	if !HasAttachSupport(rt) {
		t.Error("HasAttachSupport = false, want true for a runtime reporting SupportsAttach() = true")
	}
}

// A runtime that does not implement AttachCapableRuntime at all — the
// missing-field case — is treated as supported.
func TestHasAttachSupport_FieldMissing(t *testing.T) {
	rt := &MockRuntime{NameFunc: func() string { return "fake" }}
	if !HasAttachSupport(rt) {
		t.Error("HasAttachSupport = false, want true for a runtime that doesn't implement AttachCapableRuntime")
	}
}

// A runtime that implements AttachCapableRuntime and reports false is
// unsupported.
func TestHasAttachSupport_FieldPresentFalse(t *testing.T) {
	rt := &attachCapableRuntime{
		MockRuntime:    &MockRuntime{NameFunc: func() string { return "fake" }},
		supportsAttach: false,
	}
	if HasAttachSupport(rt) {
		t.Error("HasAttachSupport = true, want false for a runtime reporting SupportsAttach() = false")
	}
}
