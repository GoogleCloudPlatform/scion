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

//go:build !no_sqlite

package entadapter

import (
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/GoogleCloudPlatform/scion/pkg/store/storetest"
)

// userTerminalWorkspaceFactory builds a CompositeStore-backed store.Store for
// the terminal-workspace conformance test.
func userTerminalWorkspaceFactory(t *testing.T) store.Store {
	t.Helper()

	entClient := enttest.NewClient(t)
	cs := NewCompositeStore(entClient)
	t.Cleanup(func() { _ = cs.Close() })

	return cs
}

// TestEntAdapter_UserTerminalWorkspace_Conformance runs the shared
// terminal-workspace conformance test against the Ent-backed store.
func TestEntAdapter_UserTerminalWorkspace_Conformance(t *testing.T) {
	storetest.UserTerminalWorkspaceConformance(t, userTerminalWorkspaceFactory)
}
