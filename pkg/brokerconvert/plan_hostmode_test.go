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

package brokerconvert

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Plan.HostMode is additive within SchemaVersion 1 (ptone/scion#7627): omitted
// when unset on BuildLocal, serialized under schemaVersion 1 when set, and
// never leaked into Hub-published Presentation or accompanied by settings
// values.
func TestPlanHostMode_AdditiveUnderSchemaVersion1AndRedacted(t *testing.T) {
	base := plantedPlan(t)
	unsetJSON := jsonOut(t, base)
	assert.NotContains(t, unsetJSON, `"hostMode"`, "unset HostMode is omitted")

	for _, mode := range []string{HostModeColocated, HostModeRemote, HostModeUnknown} {
		t.Run(mode, func(t *testing.T) {
			p := plantedPlan(t)
			p.HostMode = mode
			out := jsonOut(t, p)

			var doc struct {
				SchemaVersion int    `json:"schemaVersion"`
				HostMode      string `json:"hostMode"`
			}
			require.NoError(t, json.Unmarshal([]byte(out), &doc), out)
			assert.Equal(t, 1, doc.SchemaVersion, "SchemaVersion stays 1 for additive hostMode")
			assert.Equal(t, mode, doc.HostMode)
			for _, v := range []string{plantedEnvA, plantedEnvB, plantedKubeconfig, plantedProfileEnv, plantedContext} {
				assert.NotContains(t, out, v)
			}

			presBytes, err := json.Marshal(Presentation(p, testComputedAt))
			require.NoError(t, err)
			assert.NotContains(t, string(presBytes), `"hostMode"`, "Hub presentation never carries host-local hostMode")
		})
	}
}
