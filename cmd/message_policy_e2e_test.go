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

package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/knadh/koanf/v2"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type inProcessMessagePolicyE2EEnv struct {
	store        store.Store
	webChatStore hub.WebChatStore
	server       *hub.Server
	httpServer   *httptest.Server
	adminClient  hubclient.Client
	agentClient  hubclient.Client
	devToken     string
	agentToken   string
	project      *store.Project
	alice        *store.User
	senderAgent  *store.Agent
}

func startInProcessMessagePolicyE2EEnv(t *testing.T) *inProcessMessagePolicyE2EEnv {
	t.Helper()
	ctx := context.Background()

	s := newTestStore(t)

	chatDBPath := filepath.Join(t.TempDir(), "webchat.db")
	chatDB, err := sql.Open("sqlite", "file:"+chatDBPath+"?_pragma=busy_timeout(5000)")
	require.NoError(t, err)
	t.Cleanup(func() { _ = chatDB.Close() })

	wcs := hub.NewWebChatStore(chatDB, "sqlite")
	require.NoError(t, wcs.Init())

	devToken, err := apiclient.GenerateDevToken()
	require.NoError(t, err)

	srv, err := hub.New(hub.ServerConfig{
		HubID:        "mp-e2e-test-hub",
		DevAuthToken: devToken,
	}, s)
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	srv.SetWebChatStore(wcs)

	ops := hub.NewOperationalSettings(s.(store.HubSettingStore), koanf.New("."), koanf.New("."))
	_, err = ops.Refresh(ctx)
	require.NoError(t, err)
	srv.SetOperationalSettings(ops)

	expDoc := fmt.Sprintf(`{"overrides":{%q:true}}`, experiments.MessagePolicies)
	_, err = ops.Update(ctx, "experiments", json.RawMessage(expDoc), "test-admin", ops.SectionRevision("experiments"), "managed")
	require.NoError(t, err)

	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)

	alice := &store.User{
		ID:          uuid.NewString(),
		Email:       "alice-e2e@example.com",
		DisplayName: "Alice E2E",
		Role:        store.UserRoleMember,
		Status:      store.UserStatusActive,
		Created:     time.Now().UTC(),
	}
	require.NoError(t, s.CreateUser(ctx, alice))

	hubMembers, err := s.GetGroupBySlug(ctx, "hub-members")
	require.NoError(t, err)
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    hubMembers.ID,
		MemberType: store.GroupMemberTypeUser,
		MemberID:   alice.ID,
		Role:       store.GroupMemberRoleMember,
	}))

	project := &store.Project{
		ID:      uuid.NewString(),
		Name:    "mp-e2e-project",
		Slug:    "mp-e2e-project",
		OwnerID: alice.ID,
	}
	require.NoError(t, s.CreateProject(ctx, project))

	ownerRole, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: ownerRole.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      alice.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          project.ID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	brokerID := uuid.NewString()
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID:     brokerID,
		Name:   "mp-e2e-broker",
		Slug:   "mp-e2e-broker",
		Status: store.BrokerStatusOnline,
	}))

	sender := &store.Agent{
		ID:              uuid.NewString(),
		Name:            "sender-agent",
		Slug:            "sender-agent",
		ProjectID:       project.ID,
		Phase:           "running",
		OwnerID:         alice.ID,
		CreatedBy:       alice.ID,
		RuntimeBrokerID: brokerID,
		MessageMode:     store.MessageModeProject,
		Ancestry:        []string{alice.ID},
	}
	require.NoError(t, s.CreateAgent(ctx, sender))

	agentToken, cred, err := srv.GetAgentTokenService().SignAgentToken(hub.AgentTokenGrant{
		AgentID:   sender.ID,
		ProjectID: project.ID,
		Scopes:    hub.ScopesForRole(hub.AgentRoleFull),
		Ancestry:  sender.Ancestry,
	}, "")
	require.NoError(t, err)
	require.NoError(t, s.CreateAgentCredential(ctx, cred))

	adminClient, err := hubclient.New(httpSrv.URL, hubclient.WithDevToken(devToken))
	require.NoError(t, err)

	agentClient, err := hubclient.New(httpSrv.URL, hubclient.WithAgentToken(agentToken))
	require.NoError(t, err)

	return &inProcessMessagePolicyE2EEnv{
		store:        s,
		webChatStore: wcs,
		server:       srv,
		httpServer:   httpSrv,
		adminClient:  adminClient,
		agentClient:  agentClient,
		devToken:     devToken,
		agentToken:   agentToken,
		project:      project,
		alice:        alice,
		senderAgent:  sender,
	}
}

func clearScionTestEnv(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	for _, k := range []string{
		"SCION_HUB_ENDPOINT",
		"SCION_HUB_URL",
		"SCION_HUB_TOKEN",
		"SCION_AUTH_TOKEN",
		"SCION_DEV_TOKEN",
		"SCION_DEV_TOKEN_FILE",
		"SCION_AGENT_ID",
		"SCION_AGENT_NAME",
		"SCION_PROJECT",
		projectkeys.EnvProjectID,
		"SCION_HUB_PROJECT_ID",
		"SCION_CLI_MODE",
	} {
		t.Setenv(k, "")
	}
}

func setOperatorE2EEnv(t *testing.T, env *inProcessMessagePolicyE2EEnv) {
	t.Helper()
	clearScionTestEnv(t)
	t.Setenv("SCION_HUB_ENDPOINT", env.httpServer.URL)
	t.Setenv("SCION_DEV_TOKEN", env.devToken)
}

func setAgentE2EEnv(t *testing.T, env *inProcessMessagePolicyE2EEnv) {
	t.Helper()
	clearScionTestEnv(t)
	t.Setenv("SCION_HUB_ENDPOINT", env.httpServer.URL)
	t.Setenv("SCION_AUTH_TOKEN", env.agentToken)
	t.Setenv("SCION_AGENT_ID", env.senderAgent.ID)
	t.Setenv("SCION_AGENT_NAME", env.senderAgent.Slug)
	t.Setenv("SCION_PROJECT", env.project.Slug)
	t.Setenv(projectkeys.EnvProjectID, env.project.ID)
	t.Setenv("SCION_CLI_MODE", "agent")
}

func executeMessagePolicyE2ECLI(t *testing.T, mode CLIMode, args ...string) (string, error) {
	t.Helper()

	origFormat := outputFormat
	origProject := projectPath
	origHub := hubEndpoint
	origGlobal := globalMode
	outputFormat = ""
	projectPath = ""
	hubEndpoint = ""
	globalMode = false
	t.Cleanup(func() {
		outputFormat = origFormat
		projectPath = origProject
		hubEndpoint = origHub
		globalMode = origGlobal
	})

	tree := &cobra.Command{
		Use:           "scion",
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	tree.PersistentFlags().StringVarP(&outputFormat, "format", "", "", "Output format (json, plain)")
	tree.PersistentFlags().StringVarP(&projectPath, "project", "p", "", "Target project")
	tree.PersistentFlags().StringVar(&hubEndpoint, "hub", "", "Hub endpoint")
	tree.PersistentFlags().BoolVarP(&globalMode, "global", "g", false, "Global scope")
	tree.AddCommand(newMessagePolicyCmd())

	if mode == ModeAgent {
		applyModeRestrictions(tree, ModeAgent)
	}

	var out bytes.Buffer
	tree.SetOut(&out)
	tree.SetErr(&out)
	tree.SetArgs(append([]string{"message-policy"}, args...))
	err := tree.Execute()
	return out.String(), err
}

func resetMessageCommandGlobals(t *testing.T) {
	t.Helper()
	msgChannel = ""
	msgThreadID = ""
	msgBodyFile = ""
	msgAttach = nil
	msgAttachUpload = nil
	msgArtifacts = nil
	msgArtifactRefs = nil
	resetDecisionFlags()
	msgInterrupt = false
	msgWake = false
	msgNotify = false
	msgPlain = false
	msgIn = ""
	msgAt = ""
	msgCC = nil
}

// TestMessagePolicy_EndToEnd_EnforceRejectThenShadowDeliver exercises the full
// M1 message-policy lifecycle in-process against a real SQLite Hub:
//  1. Operator authors an ENFORCE regex prose-choices policy via CLI (`scion message-policy create`).
//  2. Agent inspects policies via read-only `scion message-policy list` and `get` in agent mode.
//  3. Agent sends a violating outbound message to a user via `scion message` -> HTTP 422
//     `message_policy_rejected` with rendered reason + remediation, zero message rows stored,
//     and a content-free `action=rejected` decision row recorded.
//  4. Operator switches the policy to SHADOW mode; agent resends the violating message ->
//     message is delivered/persisted and an asynchronous content-free `action=shadow`
//     decision row is recorded referencing the delivered message_id with zero message text.
func TestMessagePolicy_EndToEnd_EnforceRejectThenShadowDeliver(t *testing.T) {
	ctx := context.Background()
	env := startInProcessMessagePolicyE2EEnv(t)

	// 1. Author an ENFORCE prose-choices policy through the CLI against the live in-process Hub.
	setOperatorE2EEnv(t, env)
	createOut, err := executeMessagePolicyE2ECLI(t, ModeHuman,
		"--project", env.project.ID,
		"create", "no-prose-choices",
		"--surface", "agent_to_user",
		"--all-agents",
		"--mode", "enforce",
		"--on-failed", "reject",
		"--rule", `!message.has_choices && message.body.matches("(?i)\\b(option\\s+[ab12]|choose\\s+one)\\b")`,
		"--reject-reason", "Policy {policy} blocked prose choices to {recipient}",
		"--reject-remediation", "Send structured choices via --choices-file instead of prose menus",
	)
	require.NoError(t, err, "create output: %s", createOut)
	assert.Contains(t, createOut, `Created message policy "no-prose-choices"`)
	assert.Contains(t, createOut, "mode: enforce, revision: 1")
	assert.Contains(t, createOut, "agent_to_user")

	// Dry-run test via CLI confirms violation without echoing sample body text.
	const dryRunMarker = "DRYRUN-CONFIDENTIAL-778899"
	dryRunOut, err := executeMessagePolicyE2ECLI(t, ModeHuman,
		"--project", env.project.ID,
		"test", "no-prose-choices",
		"--surface", "agent_to_user",
		"--body", dryRunMarker+": Please choose Option A or Option B.",
	)
	require.NoError(t, err, "dry-run output: %s", dryRunOut)
	assert.Contains(t, dryRunOut, "Class:           violation")
	assert.Contains(t, dryRunOut, "Action:          rejected")
	assert.NotContains(t, dryRunOut, dryRunMarker, "dry-run output must never echo sample body")

	// 2. Agent-mode read-only inspection (`list` and `get` allowed; mutating subcommands blocked).
	setAgentE2EEnv(t, env)
	agentListOut, err := executeMessagePolicyE2ECLI(t, ModeAgent, "list")
	require.NoError(t, err, "agent list output: %s", agentListOut)
	assert.Contains(t, agentListOut, "no-prose-choices")
	assert.Contains(t, agentListOut, "enforce")

	agentGetOut, err := executeMessagePolicyE2ECLI(t, ModeAgent, "get", "no-prose-choices")
	require.NoError(t, err, "agent get output: %s", agentGetOut)
	assert.Contains(t, agentGetOut, "Name:               no-prose-choices")
	assert.Contains(t, agentGetOut, "Mode:               enforce")

	for _, blocked := range []string{"create", "delete", "test", "decisions"} {
		_, blockedErr := executeMessagePolicyE2ECLI(t, ModeAgent, blocked)
		require.Error(t, blockedErr, "agent mode must block message-policy %s", blocked)
	}

	// 3. Agent sends a violating outbound message to user:alice-e2e@example.com via `scion message`.
	resetMessageCommandGlobals(t)
	origProject := projectPath
	origHub := hubEndpoint
	origFormat := outputFormat
	projectPath = ""
	hubEndpoint = ""
	outputFormat = ""
	t.Cleanup(func() {
		projectPath = origProject
		hubEndpoint = origHub
		outputFormat = origFormat
	})

	const enforceSecretMarker = "ENFORCE-SECRET-BODY-112233"
	violatingEnforceBody := enforceSecretMarker + ": Do you prefer Option A or Option B?"
	sendErr := messageCmd.RunE(messageCmd, []string{"user:" + env.alice.Email, violatingEnforceBody})
	require.Error(t, sendErr, "violating outbound agent message must be rejected in enforce mode")

	// Verify HTTP 422 message_policy_rejected and CLI rendering of reason + policy + remediation.
	var apiErr *apiclient.APIError
	require.True(t, errors.As(sendErr, &apiErr), "expected *apiclient.APIError, got %T: %v", sendErr, sendErr)
	assert.Equal(t, http.StatusUnprocessableEntity, apiErr.StatusCode)
	assert.Equal(t, apiclient.ErrCodeMessagePolicyRejected, apiErr.Code)

	errText := sendErr.Error()
	assert.Contains(t, errText, "message_policy_rejected")
	assert.Contains(t, errText, "Policy no-prose-choices blocked prose choices to "+env.alice.Email)
	assert.Contains(t, errText, "policy: no-prose-choices")
	assert.Contains(t, errText, "remediation: Send structured choices via --choices-file instead of prose menus")
	assert.NotContains(t, errText, enforceSecretMarker, "rejection error must never echo message body")

	// Zero message rows stored on enforce rejection.
	storedMsgs, err := env.store.ListMessages(ctx, store.MessageFilter{SenderID: env.senderAgent.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, storedMsgs.Items, "enforce rejection must persist zero message rows")

	dms, err := env.webChatStore.ListDMs(ctx, env.alice.ID)
	require.NoError(t, err)
	assert.Empty(t, dms, "enforce rejection must not register webchat DM rows")

	// Decision log records content-free action=rejected row.
	setOperatorE2EEnv(t, env)
	enforceDecOut, err := executeMessagePolicyE2ECLI(t, ModeHuman,
		"--project", env.project.ID,
		"--format", "json",
		"decisions", "no-prose-choices",
	)
	require.NoError(t, err, "decisions json output: %s", enforceDecOut)
	assert.NotContains(t, enforceDecOut, enforceSecretMarker)
	assert.NotContains(t, enforceDecOut, "Option A")

	var enforceDecResp hubclient.ListMessagePolicyDecisionsResponse
	require.NoError(t, json.Unmarshal([]byte(enforceDecOut), &enforceDecResp))
	require.Len(t, enforceDecResp.Decisions, 1)
	assert.Equal(t, store.MessagePolicyModeEnforce, enforceDecResp.Decisions[0].Mode)
	assert.Equal(t, store.MessagePolicyClassViolation, enforceDecResp.Decisions[0].Class)
	assert.Equal(t, store.MessagePolicyActionRejected, enforceDecResp.Decisions[0].Action)
	assert.Empty(t, enforceDecResp.Decisions[0].MessageID, "rejected decision must have empty message_id")

	// 4. Switch policy to SHADOW mode, resend violating message -> delivered + content-free shadow decision row.
	shadowMode := store.MessagePolicyModeShadow
	updatedPol, err := env.adminClient.ProjectMessagePolicies(env.project.ID).Update(ctx, "no-prose-choices", &hubclient.PatchMessagePolicyRequest{
		Mode: &shadowMode,
	})
	require.NoError(t, err)
	assert.Equal(t, store.MessagePolicyModeShadow, updatedPol.Mode)
	assert.Equal(t, int64(2), updatedPol.Revision)

	setAgentE2EEnv(t, env)
	resetMessageCommandGlobals(t)
	const shadowSecretMarker = "SHADOW-SECRET-BODY-445566"
	violatingShadowBody := shadowSecretMarker + ": Please choose one: Option 1 or Option 2."
	require.NoError(t, messageCmd.RunE(messageCmd, []string{"user:" + env.alice.Email, violatingShadowBody}))

	// Message IS delivered and persisted in shadow mode.
	storedMsgs, err = env.store.ListMessages(ctx, store.MessageFilter{SenderID: env.senderAgent.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, storedMsgs.Items, 1, "shadow mode must deliver and persist the outbound message")
	deliveredMessageID := storedMsgs.Items[0].ID
	require.NotEmpty(t, deliveredMessageID)

	// Wait for the bounded shadow worker pool to persist the content-free shadow decision row.
	setOperatorE2EEnv(t, env)
	var shadowDec store.MessagePolicyDecision
	var rawShadowJSON string
	require.Eventually(t, func() bool {
		out, decErr := executeMessagePolicyE2ECLI(t, ModeHuman,
			"--project", env.project.ID,
			"--format", "json",
			"decisions", "no-prose-choices",
			"--action", store.MessagePolicyActionShadow,
		)
		if decErr != nil {
			return false
		}
		var resp hubclient.ListMessagePolicyDecisionsResponse
		if json.Unmarshal([]byte(out), &resp) != nil || len(resp.Decisions) == 0 {
			return false
		}
		rawShadowJSON = out
		shadowDec = resp.Decisions[0]
		return true
	}, 5*time.Second, 25*time.Millisecond, "expected async shadow decision row to be recorded")

	assert.Equal(t, store.MessagePolicyModeShadow, shadowDec.Mode)
	assert.Equal(t, store.MessagePolicyClassViolation, shadowDec.Class)
	assert.Equal(t, store.MessagePolicyActionShadow, shadowDec.Action)
	assert.Equal(t, int64(2), shadowDec.PolicyRevision)
	assert.Equal(t, deliveredMessageID, shadowDec.MessageID, "shadow decision must reference the delivered message_id")
	assert.NotContains(t, rawShadowJSON, shadowSecretMarker, "shadow decision response must never contain message text")
	assert.NotContains(t, rawShadowJSON, "Option 1", "shadow decision response must never contain message text")
}
