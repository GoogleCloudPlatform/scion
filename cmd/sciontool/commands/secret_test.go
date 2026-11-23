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
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
)

type fakeSecretHubClient struct {
	configured bool

	setCalls []struct {
		key, value, secretType, target, scope string
		force                                 bool
		allowProgeny                          *bool
	}
	setResp *hub.SetSecretResponse
	setErr  error

	deleteCalls []struct {
		key, scope string
	}
	deleteErr error
}

func (f *fakeSecretHubClient) IsConfigured() bool { return f.configured }

func (f *fakeSecretHubClient) SetSecret(_ context.Context, key, value, secretType, target, scope string, force bool, allowProgeny *bool) (*hub.SetSecretResponse, error) {
	f.setCalls = append(f.setCalls, struct {
		key, value, secretType, target, scope string
		force                                 bool
		allowProgeny                          *bool
	}{key, value, secretType, target, scope, force, allowProgeny})
	if f.setErr != nil {
		return nil, f.setErr
	}
	if f.setResp != nil {
		return f.setResp, nil
	}
	return &hub.SetSecretResponse{Key: key, Scope: scope}, nil
}

func (f *fakeSecretHubClient) DeleteSecret(_ context.Context, key, scope string) error {
	f.deleteCalls = append(f.deleteCalls, struct{ key, scope string }{key, scope})
	return f.deleteErr
}

func resetSecretFlags(t *testing.T) {
	t.Helper()
	prevType, prevTarget, prevForce, prevScope, prevProgeny, prevDelScope := secretType, secretTarget, secretForce, secretScope, secretAllowProgeny, secretDeleteScope
	t.Cleanup(func() {
		secretType = prevType
		secretTarget = prevTarget
		secretForce = prevForce
		secretScope = prevScope
		secretAllowProgeny = prevProgeny
		secretDeleteScope = prevDelScope
	})
	secretType = ""
	secretTarget = ""
	secretForce = false
	secretScope = ""
	secretAllowProgeny = false
	secretDeleteScope = "agent"
}

func TestRunSecretSet_AgentScopeArgvAndStdin(t *testing.T) {
	resetSecretFlags(t)

	fake := &fakeSecretHubClient{configured: true}
	prevFactory := newSecretHubClient
	newSecretHubClient = func() secretHubClient { return fake }
	t.Cleanup(func() { newSecretHubClient = prevFactory })

	t.Run("argv value with --scope agent", func(t *testing.T) {
		resetSecretFlags(t)
		fake.setCalls = nil
		secretScope = "agent"

		cmd := &cobra.Command{}
		cmd.Flags().BoolVar(&secretAllowProgeny, "allow-progeny", false, "")
		var out bytes.Buffer
		cmd.SetOut(&out)

		require.NoError(t, runSecretSet(cmd, []string{"MY_AGENT_KEY", "super-secret"}))
		require.Len(t, fake.setCalls, 1)
		call := fake.setCalls[0]
		assert.Equal(t, "MY_AGENT_KEY", call.key)
		assert.Equal(t, base64.StdEncoding.EncodeToString([]byte("super-secret")), call.value)
		assert.Equal(t, "agent", call.scope)
		assert.Nil(t, call.allowProgeny)
	})

	t.Run("stdin value with dash trims trailing newline for env", func(t *testing.T) {
		resetSecretFlags(t)
		fake.setCalls = nil
		secretScope = "agent"

		cmd := &cobra.Command{}
		cmd.Flags().BoolVar(&secretAllowProgeny, "allow-progeny", false, "")
		cmd.SetIn(strings.NewReader("from-stdin-token\n"))

		require.NoError(t, runSecretSet(cmd, []string{"STDIN_KEY", "-"}))
		require.Len(t, fake.setCalls, 1)
		decoded, err := base64.StdEncoding.DecodeString(fake.setCalls[0].value)
		require.NoError(t, err)
		assert.Equal(t, "from-stdin-token", string(decoded))
		assert.Equal(t, "agent", fake.setCalls[0].scope)
	})

	t.Run("stdin value preserves trailing newline for file type", func(t *testing.T) {
		resetSecretFlags(t)
		fake.setCalls = nil
		secretScope = "agent"
		secretType = "file"
		secretTarget = "~/cert.pem"

		cmd := &cobra.Command{}
		cmd.Flags().BoolVar(&secretAllowProgeny, "allow-progeny", false, "")
		cmd.SetIn(strings.NewReader("line1\nline2\n"))

		require.NoError(t, runSecretSet(cmd, []string{"CERT_PEM", "-"}))
		require.Len(t, fake.setCalls, 1)
		decoded, err := base64.StdEncoding.DecodeString(fake.setCalls[0].value)
		require.NoError(t, err)
		assert.Equal(t, "line1\nline2\n", string(decoded))
	})

	t.Run("stdin empty value is rejected", func(t *testing.T) {
		resetSecretFlags(t)
		secretScope = "agent"

		cmd := &cobra.Command{}
		cmd.Flags().BoolVar(&secretAllowProgeny, "allow-progeny", false, "")
		cmd.SetIn(strings.NewReader("\n"))

		err := runSecretSet(cmd, []string{"EMPTY_KEY", "-"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "empty")
	})

	t.Run("stdin exceeding 64KB limit is rejected", func(t *testing.T) {
		resetSecretFlags(t)
		secretScope = "agent"

		cmd := &cobra.Command{}
		cmd.Flags().BoolVar(&secretAllowProgeny, "allow-progeny", false, "")
		cmd.SetIn(strings.NewReader(strings.Repeat("a", maxSecretValueBytes+1)))

		err := runSecretSet(cmd, []string{"BIG_KEY", "-"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "64KB")
	})

	t.Run("invalid scope is rejected", func(t *testing.T) {
		resetSecretFlags(t)
		secretScope = "hub"

		cmd := &cobra.Command{}
		err := runSecretSet(cmd, []string{"K", "v"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--scope")
	})
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
