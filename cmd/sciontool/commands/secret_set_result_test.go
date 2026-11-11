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
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
)

// TestReportSetSecretResult_PendingAtWarnLevel: the pending result is
// written whatever the log level, so capture-auth can read it.
func TestReportSetSecretResult_PendingAtWarnLevel(t *testing.T) {
	require.NoError(t, log.ApplyLogLevel("warn"))
	t.Cleanup(func() { _ = log.ApplyLogLevel("info") })

	var out bytes.Buffer
	reportSetSecretResult(&out, &hub.SetSecretResponse{Key: "TOOL_AUTH", Scope: "user", State: "pending"})
	assert.Equal(t, "Secret \"TOOL_AUTH\" saved, pending approval in Scion (scope: user)\n", out.String())

	out.Reset()
	reportSetSecretResult(&out, &hub.SetSecretResponse{Key: "TOOL_AUTH", Scope: "user", State: "pending", ReplacesExisting: true})
	assert.Contains(t, out.String(), "pending approval in Scion")
	assert.Contains(t, out.String(), "replaces your existing secret once approved")

	out.Reset()
	reportSetSecretResult(&out, &hub.SetSecretResponse{Key: "PROJ_KEY", Scope: "project"})
	assert.Empty(t, out.String(), "a stored project secret is reported through the logger")
}
