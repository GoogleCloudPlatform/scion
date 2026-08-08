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
	"errors"
	"strings"
	"testing"
)

func TestWrapHubError_SuppressesLocalHintForAgents(t *testing.T) {
	baseErr := errors.New("hub connection refused")

	t.Run("standalone CLI includes local-only hint", func(t *testing.T) {
		t.Setenv("SCION_AGENT_ID", "")
		got := wrapHubError(baseErr)
		if !strings.Contains(got.Error(), "local-only mode") {
			t.Errorf("expected local-only mode hint for standalone CLI, got: %s", got)
		}
	})

	t.Run("hub-managed agent omits local-only hint", func(t *testing.T) {
		t.Setenv("SCION_AGENT_ID", "agent-uuid-123")
		got := wrapHubError(baseErr)
		if strings.Contains(got.Error(), "local-only mode") {
			t.Errorf("expected no local-only mode hint for hub-managed agent, got: %s", got)
		}
		if !errors.Is(got, baseErr) {
			t.Errorf("expected wrapped error to contain base error")
		}
	})
}
