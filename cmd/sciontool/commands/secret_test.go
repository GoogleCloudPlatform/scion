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
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeSecretHubClient struct {
	configured bool

	deleteCalls []struct {
		key, scope string
	}
	deleteErr error
}

func (f *fakeSecretHubClient) IsConfigured() bool { return f.configured }

func (f *fakeSecretHubClient) DeleteSecret(_ context.Context, key, scope string) error {
	f.deleteCalls = append(f.deleteCalls, struct{ key, scope string }{key, scope})
	return f.deleteErr
}

func resetSecretFlags(t *testing.T) {
	t.Helper()
	prevDelScope := secretDeleteScope
	t.Cleanup(func() {
		secretDeleteScope = prevDelScope
	})
	secretDeleteScope = "agent"
}

func TestRunSecretDelete_AgentScope(t *testing.T) {
	resetSecretFlags(t)

	fake := &fakeSecretHubClient{configured: true}
	prevFactory := newSecretHubClient
	newSecretHubClient = func() secretHubClient { return fake }
	t.Cleanup(func() { newSecretHubClient = prevFactory })

	t.Run("deletes agent-scoped secret", func(t *testing.T) {
		resetSecretFlags(t)
		fake.deleteCalls = nil
		fake.deleteErr = nil
		secretDeleteScope = "agent"

		require.NoError(t, runSecretDelete(nil, []string{"MY_AGENT_KEY"}))
		require.Len(t, fake.deleteCalls, 1)
		assert.Equal(t, "MY_AGENT_KEY", fake.deleteCalls[0].key)
		assert.Equal(t, "agent", fake.deleteCalls[0].scope)
	})

	t.Run("rejects non-agent scope", func(t *testing.T) {
		resetSecretFlags(t)
		secretDeleteScope = "project"

		err := runSecretDelete(nil, []string{"MY_KEY"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--scope")
	})

	t.Run("rejects invalid key", func(t *testing.T) {
		resetSecretFlags(t)
		secretDeleteScope = "agent"

		err := runSecretDelete(nil, []string{"BAD=KEY"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "key")
	})

	t.Run("propagates hub client error", func(t *testing.T) {
		resetSecretFlags(t)
		fake.deleteErr = errors.New("not found")
		secretDeleteScope = "agent"

		err := runSecretDelete(nil, []string{"MISSING"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not found")
	})
}
