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

package commands

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
)

func TestLivenessAgentUID(t *testing.T) {
	assert.Equal(t, 1000, livenessAgentUID(1000), "privilege drop: the target UID")
	assert.Equal(t, os.Getuid(), livenessAgentUID(0), "no drop: the harness inherits init's UID")
}

func TestLivenessHarnessRootDepth(t *testing.T) {
	env := func(v string) func(string) string {
		return func(k string) string {
			if k == state.LivenessHarnessRootDepthEnv {
				return v
			}
			return ""
		}
	}
	assert.Equal(t, 1, livenessHarnessRootDepth(env("")), "unset")
	assert.Equal(t, 2, livenessHarnessRootDepth(env("2")))
	assert.Equal(t, 2, livenessHarnessRootDepth(env(" 2 ")))
	assert.Equal(t, 1, livenessHarnessRootDepth(env("two")), "unparsable")
}
