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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// harness-config sync retries finalize exactly once on 409
// harness_config_conflict (ptone/scion#4219).
func TestSyncHarnessConfigToHub_RetriesFinalizeOnceOnConflict(t *testing.T) {
	t.Run("one conflict then success", func(t *testing.T) {
		rec := &hcScopeRecorder{finalizeConflicts: 1}
		hubCtx, dir := newHCScopeTestEnv(t, rec)
		require.NoError(t, syncHarnessConfigToHub(hubCtx, "codex", dir, "global", "", "codex", ""))
		assert.Equal(t, 2, rec.finalized, "finalize is retried once after a conflict")
	})

	t.Run("two conflicts fail without a third attempt", func(t *testing.T) {
		rec := &hcScopeRecorder{finalizeConflicts: 2}
		hubCtx, dir := newHCScopeTestEnv(t, rec)
		err := syncHarnessConfigToHub(hubCtx, "codex", dir, "global", "", "codex", "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "harness config was changed by another commit")
		assert.Equal(t, 2, rec.finalized, "finalize is retried only once")
	})
}
