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
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sciontoolhub "github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type capturedAgentSetReq struct {
	Key          string
	Token        string
	Value        string
	Type         string
	Target       string
	Scope        string
	Force        bool
	AllowProgeny *bool
}

func resetAgentSecretSetFlags(t *testing.T) {
	t.Helper()
	oldType := agentSecretType
	oldTarget := agentSecretTarget
	oldScope := agentSecretSetScope
	oldForce := agentSecretForce
	oldProgeny := agentSecretAllowProgeny
	oldFormat := outputFormat
	agentSecretType = ""
	agentSecretTarget = ""
	agentSecretSetScope = ""
	agentSecretForce = false
	agentSecretAllowProgeny = false
	outputFormat = ""
	t.Cleanup(func() {
		agentSecretType = oldType
		agentSecretTarget = oldTarget
		agentSecretSetScope = oldScope
		agentSecretForce = oldForce
		agentSecretAllowProgeny = oldProgeny
		outputFormat = oldFormat
	})
}

func newAgentSecretSetTestCmd(stdin string) *cobra.Command {
	c := &cobra.Command{}
	c.SetContext(context.Background())
	c.Flags().StringVar(&agentSecretType, "type", "", "")
	c.Flags().StringVar(&agentSecretTarget, "target", "", "")
	c.Flags().StringVar(&agentSecretSetScope, "scope", "", "")
	c.Flags().BoolVar(&agentSecretForce, "force", false, "")
	c.Flags().BoolVar(&agentSecretAllowProgeny, "allow-progeny", false, "")
	if stdin != "" {
		c.SetIn(strings.NewReader(stdin))
	}
	return c
}

func TestAgentSecretSet_AgentScopeArgvAndStdin(t *testing.T) {
	var last capturedAgentSetReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPut, r.Method)
		prefix := "/api/v1/agents/" + secretGetTestAgentID + "/secrets/"
		require.True(t, strings.HasPrefix(r.URL.Path, prefix), "unexpected path %s", r.URL.Path)

		var body sciontoolhub.SetSecretRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		last = capturedAgentSetReq{
			Key:          strings.TrimPrefix(r.URL.Path, prefix),
			Token:        r.Header.Get("X-Scion-Agent-Token"),
			Value:        body.Value,
			Type:         body.Type,
			Target:       body.Target,
			Scope:        body.Scope,
			Force:        body.Force,
			AllowProgeny: body.AllowProgeny,
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(sciontoolhub.SetSecretResponse{
			Key:   last.Key,
			Scope: body.Scope,
		})
	}))
	t.Cleanup(srv.Close)

	useSecretGetHub(t, srv)

	t.Run("argv value with --scope agent", func(t *testing.T) {
		resetAgentSecretSetFlags(t)
		last = capturedAgentSetReq{}
		c := newAgentSecretSetTestCmd("")
		var out bytes.Buffer
		c.SetOut(&out)
		require.NoError(t, c.Flags().Set("scope", "agent"))

		require.NoError(t, runAgentSecretSet(c, []string{"MY_AGENT_KEY", "super-secret"}))
		assert.Equal(t, "MY_AGENT_KEY", last.Key)
		assert.Equal(t, secretGetTestToken, last.Token)
		assert.Equal(t, base64.StdEncoding.EncodeToString([]byte("super-secret")), last.Value)
		assert.Equal(t, "agent", last.Scope)
		assert.Nil(t, last.AllowProgeny)
		assert.Contains(t, out.String(), `Secret "MY_AGENT_KEY" stored (scope: agent)`)
	})

	t.Run("stdin trims trailing LF and CRLF for env and preserves lone CR", func(t *testing.T) {
		for _, tc := range []struct {
			name, in, want string
		}{
			{"LF", "from-stdin-token\n", "from-stdin-token"},
			{"CRLF", "crlf-token\r\n", "crlf-token"},
			{"lone CR", "lone-cr\r", "lone-cr\r"},
			{"leading @", "@not-a-file-password\n", "@not-a-file-password"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				resetAgentSecretSetFlags(t)
				last = capturedAgentSetReq{}
				c := newAgentSecretSetTestCmd(tc.in)
				var out bytes.Buffer
				c.SetOut(&out)
				require.NoError(t, c.Flags().Set("scope", "agent"))

				require.NoError(t, runAgentSecretSet(c, []string{"STDIN_KEY", "-"}))
				decoded, err := base64.StdEncoding.DecodeString(last.Value)
				require.NoError(t, err)
				assert.Equal(t, tc.want, string(decoded))
				assert.Equal(t, "agent", last.Scope)
			})
		}
	})

	t.Run("stdin preserves trailing newline for file type", func(t *testing.T) {
		resetAgentSecretSetFlags(t)
		last = capturedAgentSetReq{}
		c := newAgentSecretSetTestCmd("line1\nline2\n")
		var out bytes.Buffer
		c.SetOut(&out)
		require.NoError(t, c.Flags().Set("scope", "agent"))
		require.NoError(t, c.Flags().Set("type", "file"))
		require.NoError(t, c.Flags().Set("target", "~/cert.pem"))

		require.NoError(t, runAgentSecretSet(c, []string{"CERT_PEM", "-"}))
		decoded, err := base64.StdEncoding.DecodeString(last.Value)
		require.NoError(t, err)
		assert.Equal(t, "line1\nline2\n", string(decoded))
		assert.Equal(t, "file", last.Type)
		assert.Equal(t, "~/cert.pem", last.Target)
	})

	t.Run("stdin empty value is rejected", func(t *testing.T) {
		resetAgentSecretSetFlags(t)
		c := newAgentSecretSetTestCmd("\n")
		require.NoError(t, c.Flags().Set("scope", "agent"))

		err := runAgentSecretSet(c, []string{"EMPTY_KEY", "-"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "empty")
	})

	t.Run("stdin exceeding 64KB limit is rejected", func(t *testing.T) {
		resetAgentSecretSetFlags(t)
		c := newAgentSecretSetTestCmd(strings.Repeat("a", maxAgentSecretValueBytes+1))
		require.NoError(t, c.Flags().Set("scope", "agent"))

		err := runAgentSecretSet(c, []string{"BIG_KEY", "-"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "64KB")
	})
}

func TestAgentSecretSet_UserScopePendingAndAllowProgeny(t *testing.T) {
	var (
		last             capturedAgentSetReq
		replacesExisting bool
		statusCode       int
		errBody          string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		prefix := "/api/v1/agents/" + secretGetTestAgentID + "/secrets/"
		var body sciontoolhub.SetSecretRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		last = capturedAgentSetReq{
			Key:          strings.TrimPrefix(r.URL.Path, prefix),
			Token:        r.Header.Get("X-Scion-Agent-Token"),
			Value:        body.Value,
			Type:         body.Type,
			Target:       body.Target,
			Scope:        body.Scope,
			Force:        body.Force,
			AllowProgeny: body.AllowProgeny,
		}
		w.Header().Set("Content-Type", "application/json")
		if statusCode != 0 {
			w.WriteHeader(statusCode)
			_, _ = w.Write([]byte(errBody))
			return
		}
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(sciontoolhub.SetSecretResponse{
			Key:              last.Key,
			Scope:            "user",
			State:            "pending",
			PendingID:        "pending-123",
			ReplacesExisting: replacesExisting,
		})
	}))
	t.Cleanup(srv.Close)

	useSecretGetHub(t, srv)

	t.Run("user scope defaults allowProgeny=true and reports pending approval", func(t *testing.T) {
		resetAgentSecretSetFlags(t)
		statusCode = 0
		replacesExisting = false
		c := newAgentSecretSetTestCmd("user-tok\n")
		var out bytes.Buffer
		c.SetOut(&out)
		require.NoError(t, c.Flags().Set("scope", "user"))

		require.NoError(t, runAgentSecretSet(c, []string{"USER_TOK", "-"}))
		assert.Equal(t, "user", last.Scope)
		require.NotNil(t, last.AllowProgeny)
		assert.True(t, *last.AllowProgeny)
		assert.Equal(t, "Secret \"USER_TOK\" saved, pending approval in Scion (scope: user)\n", out.String())
	})

	t.Run("user scope honors explicit --allow-progeny=false and reports replacement", func(t *testing.T) {
		resetAgentSecretSetFlags(t)
		statusCode = 0
		replacesExisting = true
		c := newAgentSecretSetTestCmd("user-tok-2\n")
		var out bytes.Buffer
		c.SetOut(&out)
		require.NoError(t, c.Flags().Set("scope", "user"))
		require.NoError(t, c.Flags().Set("allow-progeny", "false"))

		require.NoError(t, runAgentSecretSet(c, []string{"USER_TOK", "-"}))
		require.NotNil(t, last.AllowProgeny)
		assert.False(t, *last.AllowProgeny)
		assert.Contains(t, out.String(), "pending approval in Scion")
		assert.Contains(t, out.String(), "replaces your existing secret once approved")
	})

	t.Run("user scope json output", func(t *testing.T) {
		resetAgentSecretSetFlags(t)
		statusCode = 0
		replacesExisting = false
		outputFormat = "json"
		c := newAgentSecretSetTestCmd("user-tok-3")
		var out bytes.Buffer
		c.SetOut(&out)
		require.NoError(t, c.Flags().Set("scope", "user"))

		require.NoError(t, runAgentSecretSet(c, []string{"USER_TOK", "-"}))
		var got map[string]any
		require.NoError(t, json.Unmarshal(out.Bytes(), &got))
		assert.Equal(t, "USER_TOK", got["key"])
		assert.Equal(t, "user", got["scope"])
		assert.Equal(t, "pending", got["state"])
		assert.Equal(t, "pending-123", got["pendingId"])
	})

	t.Run("held by another agent returns conflict error with code", func(t *testing.T) {
		resetAgentSecretSetFlags(t)
		statusCode = http.StatusConflict
		errBody = `{"error":{"code":"secret_pending_held_by_other_agent","message":"held"}}`
		c := newAgentSecretSetTestCmd("val")
		require.NoError(t, c.Flags().Set("scope", "user"))

		err := runAgentSecretSet(c, []string{"HELD_KEY", "-"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), sciontoolhub.ErrCodeSecretPendingHeldByOtherAgent)
	})

	t.Run("pending cap returns 429 error with code", func(t *testing.T) {
		resetAgentSecretSetFlags(t)
		statusCode = http.StatusTooManyRequests
		errBody = `{"error":{"code":"secret_pending_cap","message":"cap"}}`
		c := newAgentSecretSetTestCmd("val")
		require.NoError(t, c.Flags().Set("scope", "user"))

		err := runAgentSecretSet(c, []string{"CAP_KEY", "-"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), sciontoolhub.ErrCodeSecretPendingCap)
	})
}

func TestAgentSecretSet_FileSyntaxAndForce(t *testing.T) {
	var last capturedAgentSetReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body sciontoolhub.SetSecretRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		last = capturedAgentSetReq{
			Value:  body.Value,
			Type:   body.Type,
			Target: body.Target,
			Scope:  body.Scope,
			Force:  body.Force,
		}
		if !body.Force {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":{"code":"conflict","message":"already exists"}}`))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	useSecretGetHub(t, srv)

	tmpDir := t.TempDir()
	credFile := filepath.Join(tmpDir, "creds.json")
	require.NoError(t, os.WriteFile(credFile, []byte(`{"tok":"abc"}`), 0600))

	t.Run("conflict without --force", func(t *testing.T) {
		resetAgentSecretSetFlags(t)
		c := newAgentSecretSetTestCmd("")
		require.NoError(t, c.Flags().Set("scope", "project"))

		err := runAgentSecretSet(c, []string{"CREDS", "@" + credFile})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "already exists")
		assert.Equal(t, "file", last.Type)
		assert.Equal(t, credFile, last.Target)
		assert.False(t, last.Force)
	})

	t.Run("succeeds with --force", func(t *testing.T) {
		resetAgentSecretSetFlags(t)
		c := newAgentSecretSetTestCmd("")
		var out bytes.Buffer
		c.SetOut(&out)
		require.NoError(t, c.Flags().Set("scope", "project"))
		require.NoError(t, c.Flags().Set("force", "true"))

		require.NoError(t, runAgentSecretSet(c, []string{"CREDS", "@" + credFile}))
		decoded, err := base64.StdEncoding.DecodeString(last.Value)
		require.NoError(t, err)
		assert.Equal(t, `{"tok":"abc"}`, string(decoded))
		assert.True(t, last.Force)
		assert.Contains(t, out.String(), `Secret "CREDS" stored (scope: project)`)
	})
}

func TestAgentSecretSet_ValidationAndOutsideAgentErrors(t *testing.T) {
	t.Run("invalid scope returns usage error", func(t *testing.T) {
		resetAgentSecretSetFlags(t)
		c := newAgentSecretSetTestCmd("")
		require.NoError(t, c.Flags().Set("scope", "hub"))

		err := runAgentSecretSet(c, []string{"K", "v"})
		require.Error(t, err)
		var ue *usageError
		assert.True(t, errors.As(err, &ue))
		assert.Contains(t, err.Error(), "--scope")
	})

	t.Run("invalid key returns usage error", func(t *testing.T) {
		resetAgentSecretSetFlags(t)
		c := newAgentSecretSetTestCmd("")
		err := runAgentSecretSet(c, []string{"BAD=KEY", "v"})
		require.Error(t, err)
		var ue *usageError
		assert.True(t, errors.As(err, &ue))
	})

	t.Run("agent and user scopes require agent identity outside container", func(t *testing.T) {
		for _, scope := range []string{"agent", "user"} {
			t.Run(scope, func(t *testing.T) {
				resetAgentSecretSetFlags(t)
				stubValueClient(t, func() *sciontoolhub.Client { return nil })
				c := newAgentSecretSetTestCmd("")
				require.NoError(t, c.Flags().Set("scope", scope))

				err := runAgentSecretSet(c, []string{"K", "v"})
				require.Error(t, err)
				assert.Contains(t, err.Error(), "agent container")
				assert.Contains(t, err.Error(), "scion hub secret set")
			})
		}
	})

	t.Run("flags registered on agentSecretSetCmd", func(t *testing.T) {
		for _, name := range []string{"scope", "type", "target", "force", "allow-progeny"} {
			assert.NotNil(t, agentSecretSetCmd.Flags().Lookup(name), "missing flag --%s", name)
		}
	})
}

func TestAgentSecretSet_TypeAndOnDemandValidation(t *testing.T) {
	t.Run("variable type rejected naming on_demand", func(t *testing.T) {
		resetAgentSecretSetFlags(t)
		c := newAgentSecretSetTestCmd("")
		require.NoError(t, c.Flags().Set("type", "variable"))

		err := runAgentSecretSet(c, []string{"K", "v"})
		require.Error(t, err)
		var ue *usageError
		assert.True(t, errors.As(err, &ue))
		assert.Contains(t, err.Error(), "on_demand")
	})

	t.Run("unknown type rejected", func(t *testing.T) {
		resetAgentSecretSetFlags(t)
		c := newAgentSecretSetTestCmd("")
		require.NoError(t, c.Flags().Set("type", "bogus"))

		err := runAgentSecretSet(c, []string{"K", "v"})
		require.Error(t, err)
		var ue *usageError
		assert.True(t, errors.As(err, &ue))
		assert.Contains(t, err.Error(), "environment, file, on_demand")
	})

	t.Run("on_demand rejected with target", func(t *testing.T) {
		resetAgentSecretSetFlags(t)
		c := newAgentSecretSetTestCmd("")
		require.NoError(t, c.Flags().Set("type", "on_demand"))
		require.NoError(t, c.Flags().Set("target", "OTHER"))

		err := runAgentSecretSet(c, []string{"K", "v"})
		require.Error(t, err)
		var ue *usageError
		assert.True(t, errors.As(err, &ue))
		assert.Contains(t, err.Error(), "--target cannot be used with --type on_demand")
	})

	t.Run("on_demand rejected at user scope", func(t *testing.T) {
		resetAgentSecretSetFlags(t)
		c := newAgentSecretSetTestCmd("")
		require.NoError(t, c.Flags().Set("type", "on_demand"))
		require.NoError(t, c.Flags().Set("scope", "user"))

		err := runAgentSecretSet(c, []string{"K", "v"})
		require.Error(t, err)
		var ue *usageError
		assert.True(t, errors.As(err, &ue))
		assert.Contains(t, err.Error(), "only supported at project and agent scope")
	})

	t.Run("on_demand accepted at agent and project scope", func(t *testing.T) {
		var last capturedAgentSetReq
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body sciontoolhub.SetSecretRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			last = capturedAgentSetReq{
				Value:  body.Value,
				Type:   body.Type,
				Target: body.Target,
				Scope:  body.Scope,
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(sciontoolhub.SetSecretResponse{
				Key:   "OD_KEY",
				Scope: body.Scope,
			})
		}))
		t.Cleanup(srv.Close)
		useSecretGetHub(t, srv)

		for _, scope := range []string{"agent", "project"} {
			t.Run(scope, func(t *testing.T) {
				resetAgentSecretSetFlags(t)
				last = capturedAgentSetReq{}
				c := newAgentSecretSetTestCmd("od-secret\n")
				var out bytes.Buffer
				c.SetOut(&out)
				require.NoError(t, c.Flags().Set("scope", scope))
				require.NoError(t, c.Flags().Set("type", "on_demand"))

				require.NoError(t, runAgentSecretSet(c, []string{"OD_KEY", "-"}))
				assert.Equal(t, "on_demand", last.Type)
				assert.Equal(t, scope, last.Scope)
				assert.Empty(t, last.Target)
				decoded, err := base64.StdEncoding.DecodeString(last.Value)
				require.NoError(t, err)
				assert.Equal(t, "od-secret", string(decoded))
			})
		}
	})
}
