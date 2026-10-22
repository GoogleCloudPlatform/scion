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

package hub

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// liveInventoryKey identifies one HTTP entry point declared in
// authzop.Catalog, by the operation that declares it and the declared
// method/pattern.
type liveInventoryKey struct {
	OperationID string
	Method      string
	Pattern     string
}

// idFixtures holds the real, store-seeded entity IDs this test substitutes
// into catalog patterns so a correctly-dispatched request reaches live
// business logic instead of a placeholder-driven 404. Populated by
// seedLiveInventoryFixtures.
type idFixtures struct {
	project                 string
	projectDel              string
	agent                   string
	agentDel                string
	agentPort               string
	group                   string
	groupDel                string
	user                    string
	userDel                 string
	member                  string
	skill                   string
	skillDel                string
	skillRegistry           string
	template                string
	templateDel             string
	harnessConfig           string
	harnessConfigDel        string
	schedule                string
	scheduledEvent          string
	limit                   string
	limitUD                 string
	entitlement             string
	entitlementUD           string
	gcpSA                   string
	gcpSADel                string
	accessConstraint        string
	accessConstraintUD      string
	previewJob              string
	invite                  string
	envVarKey               string
	uatRevokePost           string
	uatRevokeDelete         string
	runtimeBroker           string
	runtimeBrokerDel        string
	brokerEnvKey            string
	brokerSecretKey         string
	githubInstallationID    string
	roleDefinition          string
	roleDefinitionDel       string
	roleBindingDel          string
	projectMembership       string
	projectMembershipUpdate string
	memberPrincipalDel      string
	roleBindingProjectDel   string
	projectVisitor          string
	projectVisitorUpdate    string
	projectVisitorRule      string
	projectVisitorRuleUpd   string
	projectMemberRoleID     string
	allowListEmail          string
	maintenanceOpKey        string
	maintenanceMigrationKey string
	integrationName         string
	lifecycleHook           string
	hubPreStartHook         string
	chatTopic               string
	agentLifecycle          string
	agentRestore            string
	agentRestoreProject     string
	agentProvisioning       string
	artifact                string
	projectEnvKey           string
	projectSecretKey        string
	projectSharedDir        string
	projectSkillInjection   string
	projectPreStartHook     string
	// userSkillInjection is a user-scope injected skill of the caller: the
	// dev user's here, the matrix super-admin's in the bearer disposition
	// matrix (seedUserSkillInjection).
	userSkillInjection string
	// userTemplate and userTemplateDel are user-scope templates of the
	// caller, reached through /api/v1/users/me/templates: the dev user's
	// here, the matrix super-admin's in the bearer disposition matrix
	// (seedUserTemplates). userTemplateDel is the delete entry's target.
	userTemplate    string
	userTemplateDel string
	// userScopeTemplateDel, userHarnessConfig and userHarnessConfigDel are
	// the dev user's user-scope records for the user-scope branches of the
	// /api/v1/templates and /api/v1/harness-configs routes.
	userScopeTemplateDel string
	userHarnessConfig    string
	userHarnessConfigDel string
	// skillRegistryDel is a disposable skill registry for the delete
	// entry, so the pin entries keep skillRegistry.
	skillRegistryDel string
	// skillFiles is a global skill with one published version
	// (skillVersion, 1.0.0) holding one stored file, SKILL.md;
	// templateFiles and harnessConfigFiles are global records holding one
	// stored file each, README.md (seedLiveInventoryCatalogFiles).
	skillFiles         string
	skillVersion       string
	templateFiles      string
	harnessConfigFiles string
	// delegation holds an agent delegation grant bound to agent, a
	// disposable twin grant for the revoke entry, and an active delegated
	// credential of the first grant (seedLiveInventoryDelegation).
	delegation liveInventoryDelegation
	// testIdentity is a live hub test identity (handlers_test_identities.go).
	testIdentity string
	// testIdentityDel is a disposable live test identity for the delete
	// entry, so the re-issue entry keeps testIdentity.
	testIdentityDel string
	// testIdentityDelegation is a live workload issuer delegation
	// (handlers_test_identity_delegations.go) for the revoke entry.
	testIdentityDelegation string
	// inbox holds the caller's own messaging records (seedInboxRecords):
	// the dev user's here, the matrix super-admin's in the bearer
	// disposition matrix.
	inbox inboxRecords
	// chat holds the caller's own native chat records (seedChatRecords):
	// the dev user's here, the matrix super-admin's in the bearer
	// disposition matrix.
	chat chatRecords
	// agentDelProject is a disposable agent for the project-form DELETE,
	// so the by-ID DELETE keeps agentDel.
	agentDelProject string
	// agentPorts is the target of the port registration entries, with one
	// exposed port (agentPortsPort), so clearing its ports leaves agent's
	// port for the proxy entries.
	agentPorts     string
	agentPortsPort string
}

// chatRecords are native chat records of one user: a second thread of the
// fixture project (for the delete entry), a DM with an agent of that
// project and a DM with another user, messages the user wrote in the
// fixture thread (one for edit, one for delete, one more), and a message
// in the agent DM.
type chatRecords struct {
	topicDel           string
	dmAgent            string
	dmUser             string
	topicMessage       string
	topicMessageEdit   string
	topicMessageDelete string
	dmMessage          string
}

// seedChatRecords creates chatRecords for userID: topicID is the fixture
// thread of projectID, agentID an agent of projectID, peerUserID another
// active user.
func seedChatRecords(t *testing.T, ctx context.Context, s store.Store, wcs WebChatStore, userID, projectID, topicID, agentID, peerUserID string) chatRecords {
	t.Helper()
	at := time.Now().UTC().Add(-time.Minute)
	r := chatRecords{topicDel: uuid.NewString()}
	require.NoError(t, wcs.CreateTopic(ctx, WebChatTopic{
		ID: r.topicDel, ProjectID: projectID, Name: "chat-del-" + r.topicDel[:8],
		CreatedBy: userID, CreatedAt: at, LastActivityAt: at,
	}))
	var err error
	r.dmAgent, err = messages.DMConversationKey("user", userID, "agent", agentID)
	require.NoError(t, err)
	r.dmUser, err = messages.DMConversationKey("user", userID, "user", peerUserID)
	require.NoError(t, err)

	message := func(threadID, projectOfRow string, offset time.Duration) string {
		id := uuid.NewString()
		require.NoError(t, s.CreateMessage(ctx, &store.Message{
			ID: id, ProjectID: projectOfRow, Sender: "user:" + userID, SenderID: userID,
			Recipient: "chat", Msg: "chat record " + id[:8], Type: "chat",
			Channel: "web", ThreadID: threadID, CreatedAt: at.Add(offset),
		}))
		return id
	}
	r.topicMessageEdit = message(topicID, projectID, time.Second)
	r.topicMessageDelete = message(topicID, projectID, 2*time.Second)
	r.topicMessage = message(topicID, projectID, 3*time.Second)
	r.dmMessage = message(r.dmAgent, projectID, 4*time.Second)
	return r
}

// seedInboxRecords creates inboxRecords for userID in projectID; agentID
// is the peer of the direct conversation and the watched agent.
func seedInboxRecords(t *testing.T, ctx context.Context, s store.Store, userID, projectID, agentID string) inboxRecords {
	t.Helper()
	at := time.Now().UTC()
	r := inboxRecords{createConversations: "inbox-" + uuid.NewString()[:8]}

	r.message = uuid.NewString()
	require.NoError(t, s.CreateMessage(ctx, &store.Message{
		ID: r.message, ProjectID: projectID, Sender: "agent:li", Recipient: "user:" + userID, RecipientID: userID,
		Msg: "inbox record", Type: "instruction", CreatedAt: at,
	}))

	r.subscription = uuid.NewString()
	require.NoError(t, s.CreateNotificationSubscription(ctx, &store.NotificationSubscription{
		ID: r.subscription, Scope: store.SubscriptionScopeAgent, AgentID: agentID,
		SubscriberType: store.SubscriberTypeUser, SubscriberID: userID, ProjectID: projectID,
		TriggerActivities: []string{"COMPLETED"}, CreatedBy: userID,
	}))
	r.notification = uuid.NewString()
	require.NoError(t, s.CreateNotification(ctx, &store.Notification{
		ID: r.notification, SubscriptionID: r.subscription, AgentID: agentID, ProjectID: projectID,
		SubscriberType: store.SubscriberTypeUser, SubscriberID: userID, Status: "COMPLETED", Message: "inbox record",
	}))
	r.template = uuid.NewString()
	require.NoError(t, s.CreateSubscriptionTemplate(ctx, &store.SubscriptionTemplate{
		ID: r.template, Name: "inbox-template-" + r.template[:8], Scope: store.SubscriptionScopeProject,
		TriggerActivities: []string{"COMPLETED"}, ProjectID: projectID, CreatedBy: userID,
	}))

	group := func(name string) string {
		id := uuid.NewString()
		pid := projectID
		require.NoError(t, s.CreateConversation(ctx, &store.Conversation{
			ID: id, ProjectID: &pid, Kind: "group", Surface: "native", DisplayName: name,
			DriftState: "active", LastActivityAt: at, CreatedAt: at,
		}))
		require.NoError(t, s.AddParticipant(ctx, &store.ConversationParticipant{
			ID: uuid.NewString(), ConversationID: id, PrincipalKind: "user", PrincipalID: userID, Role: "member", JoinedAt: at,
		}))
		return id
	}
	r.groupConversation = group("inbox-group-" + r.template[:8])
	r.leaveConversation = group("inbox-leave-" + r.template[:8])

	extRef, err := messages.DMConversationKey("user", userID, "agent", agentID)
	require.NoError(t, err)
	r.directConversation = uuid.NewString()
	require.NoError(t, s.CreateConversation(ctx, &store.Conversation{
		ID: r.directConversation, Kind: "direct", Surface: "native", ExternalRef: extRef,
		DriftState: "active", LastActivityAt: at, CreatedAt: at,
	}))
	for _, p := range []struct{ kind, id string }{{"user", userID}, {"agent", agentID}} {
		require.NoError(t, s.AddParticipant(ctx, &store.ConversationParticipant{
			ID: uuid.NewString(), ConversationID: r.directConversation, PrincipalKind: p.kind, PrincipalID: p.id, Role: "member", JoinedAt: at,
		}))
	}

	conversationMessage := func(convID string) string {
		id := uuid.NewString()
		require.NoError(t, s.CreateMessage(ctx, &store.Message{
			ID: id, ProjectID: projectID, Sender: "user:" + userID, SenderID: userID, Recipient: "agent:li", RecipientID: agentID,
			Msg: "inbox conversation record", Type: "instruction", ConversationID: convID, CreatedAt: at,
		}))
		return id
	}
	r.groupMessage = conversationMessage(r.groupConversation)
	r.directMessage = conversationMessage(r.directConversation)

	threadGroup := func(name string) string {
		g := &store.ThreadGroup{ProjectID: projectID, Name: name, CreatedBy: userID, CreatedByKind: store.ThreadGroupCreatorUser}
		require.NoError(t, s.CreateThreadGroup(ctx, g))
		return g.ID
	}
	r.threadGroup = threadGroup("inbox-thread-group-" + r.template[:8])
	r.threadGroupDel = threadGroup("inbox-thread-group-del-" + r.template[:8])
	return r
}

// liveInventoryFederationAudience is the peer hub audience the live
// inventory server allows for POST /api/v1/auth/federation-token.
const liveInventoryFederationAudience = "https://peer-hub.example.com"

// seedLiveInventoryFixtures creates one real store row per resource family
// (plus a second, disposable instance for families with a destructive
// catalog operation, so a DELETE entry tested earlier in catalog
// declaration order cannot remove the fixture a later read/update entry
// still needs) and returns their IDs. It never mints a real credential and
// never calls out to any external service — everything it creates lives
// only in the test's in-memory SQLite store.
func seedLiveInventoryFixtures(t *testing.T, ctx context.Context, srv *Server, s store.Store) idFixtures {
	t.Helper()
	now := time.Now()

	f := idFixtures{
		project:              tid("li-project"),
		projectDel:           tid("li-project-del"),
		agent:                tid("li-agent"),
		agentDel:             tid("li-agent-del"),
		agentPort:            "18080",
		group:                tid("li-group"),
		groupDel:             tid("li-group-del"),
		user:                 tid("li-user"),
		userDel:              tid("li-user-del"),
		member:               tid("li-member"),
		skill:                tid("li-skill"),
		skillDel:             tid("li-skill-del"),
		skillRegistry:        tid("li-skill-registry"),
		template:             tid("li-template"),
		templateDel:          tid("li-template-del"),
		harnessConfig:        tid("li-hc"),
		harnessConfigDel:     tid("li-hc-del"),
		schedule:             tid("li-schedule"),
		scheduledEvent:       tid("li-sched-event"),
		limit:                tid("li-limit"),
		limitUD:              tid("li-limit-ud"),
		entitlement:          tid("li-entitlement"),
		entitlementUD:        tid("li-entitlement-ud"),
		gcpSA:                tid("li-gcp-sa"),
		gcpSADel:             tid("li-gcp-sa-del"),
		accessConstraint:     tid("li-ac"),
		accessConstraintUD:   tid("li-ac-ud"),
		previewJob:           tid("li-preview-job"),
		invite:               tid("li-invite"),
		allowListEmail:       "li-allowlist@test.com",
		envVarKey:            "LI_INVENTORY_TEST_VAR",
		uatRevokePost:        tid("li-uat-revoke-post"),
		uatRevokeDelete:      tid("li-uat-revoke-delete"),
		runtimeBroker:        tid("li-broker"),
		runtimeBrokerDel:     tid("li-broker-del"),
		brokerEnvKey:         "LI_BROKER_ENV_VAR",
		brokerSecretKey:      "LI_BROKER_SECRET",
		githubInstallationID: "900000001",
		roleDefinition:       tid("li-role"),
		roleDefinitionDel:    tid("li-role-del"),
		roleBindingDel:       tid("li-role-binding-del"),
		integrationName:      "telegram",
		lifecycleHook:        tid("li-lifecycle-hook"),
		chatTopic:            tid("li-chat-topic"),
		agentLifecycle:       tid("li-agent-lifecycle"),
		agentRestore:         tid("li-agent-restore"),
		agentRestoreProject:  tid("li-agent-restore-project"),
		agentProvisioning:    tid("li-agent-provisioning"),
		agentDelProject:      tid("li-agent-del-project"),
		agentPorts:           tid("li-agent-ports"),
		agentPortsPort:       "18081",
	}

	f.projectSharedDir = "li-shared"
	require.NoError(t, s.CreateProject(ctx, &store.Project{ID: f.project, Name: "LI Project", Slug: "li-project",
		SharedDirs: []api.SharedDir{{Name: f.projectSharedDir}}}))
	require.NoError(t, s.CreateProject(ctx, &store.Project{ID: f.projectDel, Name: "LI Project Del", Slug: "li-project-del"}))

	require.NoError(t, s.CreateAgent(ctx, &store.Agent{ID: f.agent, Slug: "li-agent", Name: "LI Agent", ProjectID: f.project, Phase: string(state.PhaseRunning)}))
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{ID: f.agentDel, Slug: "li-agent-del", Name: "LI Agent Del", ProjectID: f.project, Phase: string(state.PhaseRunning)}))
	require.NoError(t, s.UpdateAgentExposedPorts(ctx, f.agent, []store.ExposedPort{{Port: 18080, Host: "127.0.0.1", Label: "li", Mode: "rw", ExposedAt: now, ExposedBy: "agent"}}))

	// agent.lifecycle.control dispatches start/stop/suspend/restart through
	// handleAgentLifecycle, which persists a real phase change to the store
	// (checkBrokerAvailability lets an agent with no RuntimeBrokerID
	// through, and GetDispatcher() is nil in testServer, so start/stop/
	// restart never reach a broker). A dedicated fixture, not f.agent, keeps
	// that phase churn (and stop's port clearing) off the agent every other
	// family's checks (e.g. agent.portaccess's proxy entries) still depend
	// on.
	//
	// Expected per-form sequence (start, stop, suspend, restart, in catalog
	// declaration order): start/stop/restart each 200. suspend runs third,
	// after stop already set phase=stopped, so its own phase=running guard
	// 400s before it would reach suspendAgent — still a real, non-404/405
	// result read from the seeded row, not a placeholder artifact.
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{ID: f.agentLifecycle, Slug: "li-agent-lifecycle", Name: "LI Agent Lifecycle", ProjectID: f.project, Phase: string(state.PhaseRunning)}))

	// agent.lifecycle.restore requires the target to already be
	// soft-deleted (restoreAgent 400s "Agent is not in deleted state"
	// otherwise); seeded pre-deleted here so the positive check exercises a
	// genuine restore (200) rather than that guard. Two separate instances,
	// one per path form, so the by-id form's restore consuming its fixture's
	// DeletedAt does not turn the project-scoped form's restore into that
	// same 400.
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{ID: f.agentRestore, Slug: "li-agent-restore", Name: "LI Agent Restore", ProjectID: f.project, Phase: string(state.PhaseStopped), DeletedAt: now.Add(-time.Hour)}))
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{ID: f.agentRestoreProject, Slug: "li-agent-restore-project", Name: "LI Agent Restore Project", ProjectID: f.project, Phase: string(state.PhaseStopped), DeletedAt: now.Add(-time.Hour)}))

	// agent.lifecycle.env 409s unless the agent is phase=provisioning
	// or phase=created (submitAgentEnv); shared by both path forms since
	// GetDispatcher() is nil in testServer, so submitAgentEnv always 400s
	// "no runtime broker available" before persisting anything, for either
	// form, leaving this fixture's phase untouched.
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{ID: f.agentProvisioning, Slug: "li-agent-provisioning", Name: "LI Agent Provisioning", ProjectID: f.project, Phase: string(state.PhaseProvisioning)}))

	require.NoError(t, s.CreateAgent(ctx, &store.Agent{ID: f.agentDelProject, Slug: "li-agent-del-project", Name: "LI Agent Del Project", ProjectID: f.project, Phase: string(state.PhaseRunning)}))
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{ID: f.agentPorts, Slug: "li-agent-ports", Name: "LI Agent Ports", ProjectID: f.project, Phase: string(state.PhaseRunning)}))
	require.NoError(t, s.UpdateAgentExposedPorts(ctx, f.agentPorts, []store.ExposedPort{{Port: 18081, Host: "127.0.0.1", Label: "li-ports", Mode: "rw", ExposedAt: now, ExposedBy: "agent"}}))

	require.NoError(t, s.CreateGroup(ctx, &store.Group{ID: f.group, Slug: "li-group", Name: "LI Group"}))
	require.NoError(t, s.CreateGroup(ctx, &store.Group{ID: f.groupDel, Slug: "li-group-del", Name: "LI Group Del"}))

	require.NoError(t, s.CreateUser(ctx, &store.User{ID: f.user, Email: "li-user@test.com", DisplayName: "LI User", Role: "member", Status: "active"}))
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: f.userDel, Email: "li-user-del@test.com", DisplayName: "LI User Del", Role: "member", Status: "active"}))
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: f.member, Email: "li-member@test.com", DisplayName: "LI Member", Role: "member", Status: "active"}))
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{GroupID: f.group, MemberID: f.member, MemberType: store.GroupMemberTypeUser, Role: store.GroupMemberRoleMember}))

	require.NoError(t, s.CreateSkill(ctx, &store.Skill{ID: f.skill, Name: "li-skill", Slug: "li-skill", Scope: store.SkillScopeGlobal, Status: "active", Created: now, Updated: now}))
	require.NoError(t, s.CreateSkill(ctx, &store.Skill{ID: f.skillDel, Name: "li-skill-del", Slug: "li-skill-del", Scope: store.SkillScopeGlobal, Status: "active", Created: now, Updated: now}))
	// CreateSkillRegistry (skill_registry_store.go) ignores a caller-supplied
	// ID and mints its own, writing it back into the passed struct.
	skillRegistry := &store.SkillRegistry{Name: "li-skill-registry", Endpoint: "https://example.invalid/registry", Type: store.SkillRegistryTypeHub, TrustLevel: store.SkillRegistryTrustTrusted, Status: store.SkillRegistryStatusActive, Created: now, Updated: now}
	require.NoError(t, s.CreateSkillRegistry(ctx, skillRegistry))
	f.skillRegistry = skillRegistry.ID
	skillRegistryDel := &store.SkillRegistry{Name: "li-skill-registry-del", Endpoint: "https://example.invalid/registry-del", Type: store.SkillRegistryTypeHub, TrustLevel: store.SkillRegistryTrustTrusted, Status: store.SkillRegistryStatusActive, Created: now, Updated: now}
	require.NoError(t, s.CreateSkillRegistry(ctx, skillRegistryDel))
	f.skillRegistryDel = skillRegistryDel.ID

	require.NoError(t, s.CreateTemplate(ctx, &store.Template{ID: f.template, Name: "li-template", Slug: "li-template", Harness: "claude", Scope: store.TemplateScopeGlobal, Created: now, Updated: now}))
	require.NoError(t, s.CreateTemplate(ctx, &store.Template{ID: f.templateDel, Name: "li-template-del", Slug: "li-template-del", Harness: "claude", Scope: store.TemplateScopeGlobal, Created: now, Updated: now}))

	require.NoError(t, s.CreateHarnessConfig(ctx, &store.HarnessConfig{ID: f.harnessConfig, Name: "li-hc", Slug: "li-hc", Harness: "claude", Scope: store.HarnessConfigScopeGlobal, Status: store.HarnessConfigStatusActive, Created: now, Updated: now}))
	require.NoError(t, s.CreateHarnessConfig(ctx, &store.HarnessConfig{ID: f.harnessConfigDel, Name: "li-hc-del", Slug: "li-hc-del", Harness: "claude", Scope: store.HarnessConfigScopeGlobal, Status: store.HarnessConfigStatusActive, Created: now, Updated: now}))

	require.NoError(t, s.CreateSchedule(ctx, &store.Schedule{ID: f.schedule, ProjectID: f.project, Name: "li-schedule", CronExpr: "0 0 * * *", EventType: "message", Payload: "{}", Status: store.ScheduleStatusActive, CreatedAt: now, UpdatedAt: now}))
	require.NoError(t, s.CreateScheduledEvent(ctx, &store.ScheduledEvent{ID: f.scheduledEvent, ProjectID: f.project, EventType: "message", FireAt: now.Add(time.Hour), Payload: "{}", Status: store.ScheduledEventPending, CreatedAt: now}))

	limitDef, err := s.CreateLimitDefinition(ctx, &store.LimitDefinition{ID: f.limit, Name: "li-limit", ResourceType: "agent", Unit: "count", DefaultValue: 10, CreatedAt: now, UpdatedAt: now})
	require.NoError(t, err)
	_, err = s.CreateLimitDefinition(ctx, &store.LimitDefinition{ID: f.limitUD, Name: "li-limit-ud", ResourceType: "agent", Unit: "count", DefaultValue: 10, CreatedAt: now, UpdatedAt: now})
	require.NoError(t, err)
	_, err = s.CreateEntitlementBinding(ctx, &store.EntitlementBinding{ID: f.entitlement, LimitDefinitionID: limitDef.ID, SubjectType: "user", SubjectID: f.user, ScopeType: "system", Value: 5, CreatedBy: f.user, CreatedAt: now, UpdatedAt: now})
	require.NoError(t, err)
	_, err = s.CreateEntitlementBinding(ctx, &store.EntitlementBinding{ID: f.entitlementUD, LimitDefinitionID: f.limitUD, SubjectType: "user", SubjectID: f.user, ScopeType: "system", Value: 5, CreatedBy: f.user, CreatedAt: now, UpdatedAt: now})
	require.NoError(t, err)

	require.NoError(t, s.CreateGCPServiceAccount(ctx, &store.GCPServiceAccount{ID: f.gcpSA, Scope: store.ScopeHub, ScopeID: "li-hub", Email: "li-gcp-sa@li.iam.gserviceaccount.com", ProjectID: "li-gcp-project", CreatedBy: f.user, CreatedAt: now}))
	require.NoError(t, s.CreateGCPServiceAccount(ctx, &store.GCPServiceAccount{ID: f.gcpSADel, Scope: store.ScopeHub, ScopeID: "li-hub", Email: "li-gcp-sa-del@li.iam.gserviceaccount.com", ProjectID: "li-gcp-project", CreatedBy: f.user, CreatedAt: now}))

	// Scoped to a specific, otherwise-unused principal (never "all_principals"
	// or a group the dev super-admin belongs to): an access constraint is a
	// live authorization restriction, and one that applied hub-wide would
	// poison every other probe in this test, not just the constraint's own
	// entry points.
	// CreateAccessConstraint (access_constraint_store.go) does not honor a
	// caller-supplied ID; the store always mints its own. Capture the
	// returned ID rather than the one this test asked for.
	acSubjectType := "user"
	acSubjectID := f.userDel
	ac, err := s.CreateAccessConstraint(ctx, &store.AccessConstraint{Name: "li-ac", SubjectKind: "principal", SubjectPrincipalType: &acSubjectType, SubjectPrincipalID: &acSubjectID, ScopeType: "system", MaximumPermissions: []string{"agent.read"}, Purpose: "live-inventory test fixture", CreatedBy: f.user, CreatedAt: now, UpdatedAt: now})
	require.NoError(t, err)
	f.accessConstraint = ac.ID
	acUD, err := s.CreateAccessConstraint(ctx, &store.AccessConstraint{Name: "li-ac-ud", SubjectKind: "principal", SubjectPrincipalType: &acSubjectType, SubjectPrincipalID: &acSubjectID, ScopeType: "system", MaximumPermissions: []string{"agent.read"}, Purpose: "live-inventory test fixture (update+delete)", CreatedBy: f.user, CreatedAt: now, UpdatedAt: now})
	require.NoError(t, err)
	f.accessConstraintUD = acUD.ID
	// A finished constraint preview job, so the preview job and result
	// reads address a real job. Preview jobs live in the preview service's
	// memory, not in the store.
	require.NotNil(t, srv.previewService, "the test server wires the preview service")
	srv.previewService.asyncJobs.Store(f.previewJob, &PreviewJob{JobID: f.previewJob, Status: JobStatusSucceeded, Operation: "create", Result: &PreviewResult{}})

	require.NoError(t, s.CreateInviteCode(ctx, &store.InviteCode{ID: f.invite, CodeHash: "li-invite-hash", CodePrefix: "li-invite", MaxUses: 1, ExpiresAt: now.Add(24 * time.Hour), CreatedBy: f.user, Created: now}))

	require.NoError(t, s.AddAllowListEntry(ctx, &store.AllowListEntry{ID: tid("li-allowlist"), Email: f.allowListEmail, AddedBy: f.user, Created: now}))
	// DELETE /admin/allow-list/{email} (deprecated) actually deletes the
	// User(invited) record for that email (admin_allow_list.go:118-126), not
	// the AllowListEntry row above — it needs both to exist.
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: tid("li-allowlist-user"), Email: f.allowListEmail, DisplayName: "LI Allowlist Invitee", Role: "member", Status: store.UserStatusInvited}))

	// scope=user with no explicit scopeId query param resolves to the
	// caller's own user ID (resolveEnvSecretAccess, handlers_env_secrets.go),
	// not an arbitrary user — the caller here is always the dev super-admin.
	require.NoError(t, s.CreateEnvVar(ctx, &store.EnvVar{ID: tid("li-envvar"), Key: f.envVarKey, Value: "1", Scope: store.ScopeUser, ScopeID: DevUserID, Created: now, Updated: now}))

	expiry := now.Add(24 * time.Hour)
	// UserID must be the dev caller (DevUserID), not f.user: RevokeToken and
	// DeleteToken (handlers_auth.go) look up the token by (caller's user ID,
	// token ID), so a token owned by any other user 404s regardless of
	// method or path correctness.
	require.NoError(t, s.CreateUserAccessToken(ctx, &store.UserAccessToken{ID: f.uatRevokePost, UserID: DevUserID, Name: "li-uat-revoke-post", Prefix: "li_p", KeyHash: "li-uat-revoke-post-key-hash", ProjectID: f.project, Scopes: []string{"project:read"}, ExpiresAt: &expiry, Created: now}))
	require.NoError(t, s.CreateUserAccessToken(ctx, &store.UserAccessToken{ID: f.uatRevokeDelete, UserID: DevUserID, Name: "li-uat-revoke-delete", Prefix: "li_d", KeyHash: "li-uat-revoke-delete-key-hash", ProjectID: f.project, Scopes: []string{"project:read"}, ExpiresAt: &expiry, Created: now}))

	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{ID: f.runtimeBroker, Name: "li-broker", Slug: "li-broker", Status: "offline", ConnectionState: "disconnected"}))
	// DELETE /runtime-brokers/{id} deregisters the broker, so it takes its
	// own disposable broker.
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{ID: f.runtimeBrokerDel, Name: "li-broker-del", Slug: "li-broker-del", Status: "offline", ConnectionState: "disconnected"}))
	// A sensitive broker-scoped variable: reads of it return a masked value.
	require.NoError(t, s.CreateEnvVar(ctx, &store.EnvVar{ID: tid("li-broker-envvar"), Key: f.brokerEnvKey, Value: "1", Sensitive: true, Scope: store.ScopeRuntimeBroker, ScopeID: f.runtimeBroker, Created: now, Updated: now}))

	require.NoError(t, s.CreateGitHubInstallation(ctx, &store.GitHubInstallation{InstallationID: 900000001, AccountLogin: "li-account", AccountType: "Organization", AppID: 1, Status: store.GitHubInstallationStatusActive, CreatedAt: now, UpdatedAt: now}))

	_, err = s.CreateRoleDefinition(ctx, &store.RoleDefinition{ID: f.roleDefinition, Name: "li-role", Description: "live inventory test role", ScopeType: store.RoleScopeSystem, Permissions: []string{"agent.read"}, CreatedAt: now, UpdatedAt: now})
	require.NoError(t, err)
	rdDel, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{ID: f.roleDefinitionDel, Name: "li-role-del", Description: "live inventory test role (delete)", ScopeType: store.RoleScopeSystem, Permissions: []string{"agent.read"}, CreatedAt: now, UpdatedAt: now})
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{ID: f.roleBindingDel, RoleDefinitionID: rdDel.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: f.userDel, ScopeType: store.RoleScopeSystem, CreatedBy: f.user, CreatedAt: now})
	require.NoError(t, err)

	// project.membership.update / .remove address a role binding by ID, so
	// they need a real project-scoped binding rather than a fake ID: a
	// binding on the seeded "project-member" role, scoped to f.project.
	projectMemberRole, err := s.GetRoleDefinitionByName(ctx, "project-member", store.RoleScopeProject)
	require.NoError(t, err)
	pm, err := s.CreateRoleBinding(ctx, &store.RoleBinding{RoleDefinitionID: projectMemberRole.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: f.member, ScopeType: store.RoleScopeProject, ScopeID: f.project, CreatedBy: f.user, CreatedAt: now})
	require.NoError(t, err)
	f.projectMembership = pm.ID
	f.projectMemberRoleID = projectMemberRole.ID
	// A separate binding for project.membership.update: UpdateMemberRole
	// (project_membership_service.go) replaces the binding atomically
	// (create new, delete old) even when the role doesn't change, so
	// probing update with a real body must not use the same binding
	// project.membership.remove still needs.
	pmUpdate, err := s.CreateRoleBinding(ctx, &store.RoleBinding{RoleDefinitionID: projectMemberRole.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: f.userDel, ScopeType: store.RoleScopeProject, ScopeID: f.project, CreatedBy: f.user, CreatedAt: now})
	require.NoError(t, err)
	f.projectMembershipUpdate = pmUpdate.ID
	// The members principal DELETE and the project-scoped role-binding
	// DELETE each remove a project binding, so each gets its own
	// principal holding project-member on f.project.
	for _, d := range []struct {
		name  string
		user  *string
		bound *string
	}{
		{"li-member-principal-del", &f.memberPrincipalDel, nil},
		{"li-rb-project-del", nil, &f.roleBindingProjectDel},
	} {
		uid := tid(d.name)
		require.NoError(t, s.CreateUser(ctx, &store.User{ID: uid, Email: d.name + "@test.com", DisplayName: d.name, Role: "member", Status: "active"}))
		b, err := s.CreateRoleBinding(ctx, &store.RoleBinding{RoleDefinitionID: projectMemberRole.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: uid, ScopeType: store.RoleScopeProject, ScopeID: f.project, CreatedBy: f.user, CreatedAt: now})
		require.NoError(t, err)
		if d.user != nil {
			*d.user = uid
		}
		if d.bound != nil {
			*d.bound = b.ID
		}
	}

	// project.visitors.update / .remove address a visitor (a federated
	// account with a project-visitor binding on f.project) by account ID;
	// update gets its own visitor so remove's is still there.
	for _, v := range []struct {
		email string
		dst   *string
	}{
		{"li-visitor@corp.example", &f.projectVisitor},
		{"li-visitor-update@corp.example", &f.projectVisitorUpdate},
	} {
		issuer, email := "https://li-issuer.example.com", v.email
		acct := &store.User{DisplayName: "LI Visitor", Kind: store.UserKindFederated, Status: store.UserStatusPending, HomeIssuer: &issuer, FederatedEmail: &email}
		require.NoError(t, s.CreateFederatedUser(ctx, acct))
		_, err = s.CreateVisitorRoleBinding(ctx, &store.RoleBinding{PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: acct.ID, ScopeType: store.RoleScopeProject, ScopeID: f.project, CreatedBy: f.user})
		require.NoError(t, err)
		*v.dst = acct.ID
	}
	// project.visitors.rules.update / .remove address a visitor rule of
	// f.project by ID; update gets its own rule so remove's is still there.
	for _, v := range []struct {
		domain string
		dst    *string
	}{
		{"li-rule.example", &f.projectVisitorRule},
		{"li-rule-update.example", &f.projectVisitorRuleUpd},
	} {
		rule := &store.ProjectVisitorRule{ProjectID: f.project, Issuer: "https://li-issuer.example.com", Domain: v.domain, CreatedBy: f.user}
		require.NoError(t, s.CreateProjectVisitorRule(ctx, rule))
		*v.dst = rule.ID
	}

	// Maintenance operations and migrations are seeded once, by key, during
	// Migrate() (SeedMaintenanceOperations) — they are a fixed built-in
	// registry, not something a test creates.
	f.maintenanceOpKey = "pull-images"
	f.maintenanceMigrationKey = "secret-hub-id-migration"

	// hub.integrations.read needs a plugin manager that actually has a
	// plugin loaded: handleGetIntegration (handlers_integrations.go) 404s
	// for any name the manager doesn't have, and testServer configures no
	// plugin manager at all. mockIntegrationManager is already defined in
	// this package's test sources (handlers_integrations_test.go).
	mgr := newMockIntegrationManager()
	mgr.plugins[f.integrationName] = map[string]string{}
	srv.pluginManager = mgr

	// chat.access's /chat/prefs entries need a real WebChatStore:
	// handleChatPrefs (handlers_chat_prefs.go) checks webChatStore for nil
	// before its method switch, so with no store configured every method —
	// including the bogus control method — hits the same pre-dispatch 503.
	dbProvider, ok := s.(interface{ DB() *sql.DB })
	require.True(t, ok, "store does not expose DB() *sql.DB; cannot wire a WebChatStore")
	wcs := NewWebChatStore(dbProvider.DB(), "sqlite")
	require.NoError(t, wcs.Init())
	srv.SetWebChatStore(wcs)

	// Wiring a real WebChatStore above (for /chat/prefs) also changes
	// chat.access's /chat/topics/{id} and /chat/conversations/{id}/messages
	// entries: both call WebChatStore.GetTopic(key) and 404 for a key it
	// doesn't have (handleTopicGet, handleConversationHistory), where a nil
	// store previously short-circuited to a graceful empty/200 response. A
	// bare topic row is enough for both GET entries to resolve.
	require.NoError(t, wcs.CreateTopic(ctx, WebChatTopic{
		ID:        f.chatTopic,
		ProjectID: f.project,
		Name:      "li-chat-topic",
		CreatedBy: f.user,
		CreatedAt: now,
	}))

	require.NoError(t, s.CreateLifecycleHook(ctx, &store.LifecycleHook{
		ID:        f.lifecycleHook,
		Name:      "li-lifecycle-hook",
		ScopeType: store.LifecycleHookScopeHub,
		Trigger:   store.LifecycleHookTriggerRunning,
		Action: &store.LifecycleHookAction{
			Type:    store.LifecycleHookActionHTTP,
			Method:  "POST",
			URL:     "https://example.invalid/li-lifecycle-hook",
			OnError: store.LifecycleHookOnErrorLog,
		},
		Enabled: true,
		Created: now,
		Updated: now,
	}))

	// Project-route material and settings records on f.project.
	f.projectEnvKey = "LI_PROJECT_ENV"
	require.NoError(t, s.CreateEnvVar(ctx, &store.EnvVar{ID: tid("li-project-envvar"), Key: f.projectEnvKey, Value: "1", Scope: store.ScopeProject, ScopeID: f.project, Created: now, Updated: now}))
	f.projectSecretKey = "LI_PROJECT_SECRET"
	injection := &store.SkillInjection{Scope: store.SkillInjectionScopeProject, ScopeID: f.project, SkillURI: "skill://li-injected", CreatedAt: now}
	require.NoError(t, s.AddSkillInjection(ctx, injection))
	f.projectSkillInjection = injection.ID
	hook, err := s.CreateProjectPreStartHook(ctx, &store.ProjectPreStartHook{
		Scope: store.PreStartHookScopeProject, ProjectID: f.project, Name: "li-project-pre-start-hook", Slug: "li-project-pre-start-hook",
		Script: "#!/bin/sh\necho li\n", CreatedBy: "li@test.com", UpdatedBy: "li@test.com",
	})
	require.NoError(t, err)
	f.projectPreStartHook = hook.ID

	// Hub-managed workspace and shared-dir files of f.project, under a
	// temporary home so the probes never touch the real one. The workspace
	// download from object storage (cache notify) is replaced by a no-op.
	t.Setenv("HOME", t.TempDir())
	srv.setHubWorkspaceDownloader(func(context.Context, string, string, string) error { return nil })
	liProject, err := s.GetProject(ctx, f.project)
	require.NoError(t, err)
	wsPath, err := srv.hubManagedProjectPath(liProject.Slug)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(wsPath, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(wsPath, "li.txt"), []byte("li\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(wsPath, "li-files.txt"), []byte("li\n"), 0o644))
	sdPath, err := resolveHubProjectSharedDirPath(liProject, f.projectSharedDir)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(sdPath, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(sdPath, "li.txt"), []byte("li\n"), 0o644))
	hubHook, err := s.CreateHubPreStartHook(ctx, &store.ProjectPreStartHook{
		Scope: store.PreStartHookScopeHub, Name: "li-hub-pre-start-hook", Slug: "li-hub-pre-start-hook",
		Script: "#!/bin/sh\necho li\n", CreatedBy: "li@test.com", UpdatedBy: "li@test.com",
	})
	require.NoError(t, err)
	f.hubPreStartHook = hubHook.ID

	// Artifact service: on (experiment, store, blob storage) with one
	// single-file artifact owned by the dev user, homed in f.project.
	artStore, artBlobs := enableArtifactsForTest(t, srv)
	f.artifact = seedLiveInventoryArtifact(t, ctx, artStore, artBlobs, f.project)
	seedLiveInventoryCatalogFiles(t, ctx, s, artBlobs, &f)

	// hub.conduit, hub.agent_delegation and federation.project_visitors on
	// too, so their experiment-gated routes are live: the registry above
	// with all three also defaulting on (no operational settings, which the
	// other entries rely on, are replaced).
	var active []experiments.Experiment
	for _, e := range srv.experimentRegistry().All() {
		if e.Name == conduitExperiment || e.Name == experiments.AgentDelegation || e.Name == experiments.FederationProjectVisitors {
			e.Default = true
		}
		active = append(active, e)
	}
	reg, err := experiments.NewRegistry(active, nil)
	require.NoError(t, err)
	srv.experiments = reg
	require.True(t, srv.experimentEnabled(conduitExperiment))
	require.True(t, srv.experimentEnabled(experiments.Artifacts))
	require.True(t, srv.experimentEnabled(experiments.AgentDelegation))
	require.True(t, srv.experimentEnabled(experiments.FederationProjectVisitors))
	// POST /api/v1/auth/federation-token also answers 404 while the OIDC
	// provider is off: install a test key manager and allow the audience
	// that bodyOverrides sends, so the route reaches its caller checks.
	installTestOIDCKeyManager(t, srv)
	srv.config.OIDCConfig.UserTokenAudiences = []string{liveInventoryFederationAudience}

	// Inbox, conversation and notification records of the dev user.
	f.inbox = seedInboxRecords(t, ctx, s, DevUserID, f.project, f.agent)
	f.chat = seedChatRecords(t, ctx, s, srv.webChatStore, DevUserID, f.project, f.chatTopic, f.agent, f.user)
	f.userSkillInjection = seedUserSkillInjection(t, ctx, s, DevUserID)
	f.userTemplate, f.userTemplateDel = seedUserTemplates(t, ctx, s, DevUserID)
	f.userScopeTemplateDel = tid("li-user-scope-template-del")
	require.NoError(t, s.CreateTemplate(ctx, &store.Template{ID: f.userScopeTemplateDel, Name: "li-user-scope-template-del", Slug: "li-user-scope-template-del", Harness: "claude",
		Scope: store.TemplateScopeUser, ScopeID: DevUserID, OwnerID: DevUserID, CreatedBy: DevUserID, Status: store.TemplateStatusActive, Created: now, Updated: now}))
	f.userHarnessConfig = tid("li-user-hc")
	f.userHarnessConfigDel = tid("li-user-hc-del")
	for _, id := range []string{f.userHarnessConfig, f.userHarnessConfigDel} {
		require.NoError(t, s.CreateHarnessConfig(ctx, &store.HarnessConfig{ID: id, Name: "li-user-hc-" + id[len(id)-6:], Slug: "li-user-hc-" + id[len(id)-6:], Harness: "claude",
			Scope: store.HarnessConfigScopeUser, ScopeID: DevUserID, OwnerID: DevUserID, CreatedBy: DevUserID, Status: store.HarnessConfigStatusActive, Created: now, Updated: now}))
	}
	f.delegation = seedLiveInventoryDelegation(t, ctx, s, f.agent, f.project)

	// Hub test identities: turn the feature on (--test-mode=member)
	// and seed one live identity. Its issuer is the bearer matrix's
	// super-admin (newBearerMatrixFixture), so that user's hub token may
	// re-issue for it; the dev user reaches it as an admin session.
	enableTestIdentitiesForTest(srv)
	enableTestIdentityWebSessionsForTest(srv)
	// Admin-kind test identity routes (admin tier A1): turn the test admin
	// gate on, so they answer past their 404 gate.
	srv.testAdminGate = testAdminGate{hubAdmin: true}
	f.testIdentity = tiStoreFixture(t, s, tid("bdm-super-admin"), now.Add(time.Hour)).ID
	f.testIdentityDel = tiStoreFixture(t, s, tid("bdm-super-admin"), now.Add(time.Hour)).ID
	delegation := &store.TestIdentityDelegation{
		GrantorID: tid("bdm-super-admin"), WorkloadSub: "100000000000000000099", Scope: testIdentityDelegationScope,
		MemberCap: testIdentityPerIssuerCap, Created: now, ExpiresAt: now.Add(time.Hour),
	}
	require.NoError(t, s.CreateTestIdentityDelegation(ctx, delegation))
	f.testIdentityDelegation = delegation.ID

	return f
}

// seedLiveInventoryCatalogFiles creates the global catalog records whose
// file routes the probes address: a skill with a published version 1.0.0
// holding SKILL.md, and a template and a harness config holding README.md.
// Each file is stored in blobs at the record's storage path.
func seedLiveInventoryCatalogFiles(t *testing.T, ctx context.Context, s store.Store, blobs storage.Storage, f *idFixtures) {
	t.Helper()
	now := time.Now()
	content := []byte("li\n")
	sum := sha256.Sum256(content)
	file := func(path string) []store.TemplateFile {
		return []store.TemplateFile{{Path: path, Size: int64(len(content)), Hash: "sha256:" + hex.EncodeToString(sum[:]), Mode: "0644"}}
	}
	put := func(objectPath string) {
		_, err := blobs.Upload(ctx, objectPath, bytes.NewReader(content), storage.UploadOptions{ContentType: "text/plain"})
		require.NoError(t, err)
	}

	f.skillFiles = tid("li-skill-files")
	skillPath := "li-catalog/skills/li-skill-files"
	require.NoError(t, s.CreateSkill(ctx, &store.Skill{ID: f.skillFiles, Name: "li-skill-files", Slug: "li-skill-files", Scope: store.SkillScopeGlobal,
		Status: "active", StoragePath: skillPath, Created: now, Updated: now}))
	f.skillVersion = tid("li-skill-version")
	require.NoError(t, s.CreateSkillVersion(ctx, &store.SkillVersion{ID: f.skillVersion, SkillID: f.skillFiles, Version: "1.0.0",
		Status: store.SkillVersionStatusPublished, Files: file("SKILL.md"), Created: now}))
	put(skillPath + "/1.0.0/SKILL.md")

	f.templateFiles = tid("li-template-files")
	templatePath := "li-catalog/templates/li-template-files"
	require.NoError(t, s.CreateTemplate(ctx, &store.Template{ID: f.templateFiles, Name: "li-template-files", Slug: "li-template-files", Harness: "claude",
		Scope: store.TemplateScopeGlobal, Status: store.TemplateStatusActive, StoragePath: templatePath, Files: file("README.md"), Created: now, Updated: now}))
	put(templatePath + "/README.md")

	f.harnessConfigFiles = tid("li-hc-files")
	hcPath := "li-catalog/harness-configs/li-hc-files"
	require.NoError(t, s.CreateHarnessConfig(ctx, &store.HarnessConfig{ID: f.harnessConfigFiles, Name: "li-hc-files", Slug: "li-hc-files", Harness: "claude",
		Scope: store.HarnessConfigScopeGlobal, Status: store.HarnessConfigStatusActive, StoragePath: hcPath, Files: file("README.md"), Created: now, Updated: now}))
	put(hcPath + "/README.md")
}

// seedUserSkillInjection adds a user-scope injected skill for userID and
// returns its ID.
func seedUserSkillInjection(t *testing.T, ctx context.Context, s store.Store, userID string) string {
	t.Helper()
	si := &store.SkillInjection{Scope: store.SkillInjectionScopeUser, ScopeID: userID, SkillURI: "skill://li-user-injected", CreatedBy: userID}
	require.NoError(t, s.AddSkillInjection(ctx, si))
	return si.ID
}

// seedUserTemplates adds two active user-scope templates owned by userID
// and returns their IDs: one the read and update entries address, and one
// the delete entry removes.
func seedUserTemplates(t *testing.T, ctx context.Context, s store.Store, userID string) (keep, del string) {
	t.Helper()
	now := time.Now()
	ids := []string{tid("li-user-template-" + userID), tid("li-user-template-del-" + userID)}
	for i, name := range []string{"li-user-template", "li-user-template-del"} {
		require.NoError(t, s.CreateTemplate(ctx, &store.Template{ID: ids[i], Name: name, Slug: name, Harness: "claude",
			Scope: store.TemplateScopeUser, ScopeID: userID, OwnerID: userID, CreatedBy: userID, Status: store.TemplateStatusActive, Created: now, Updated: now}))
	}
	return ids[0], ids[1]
}

// overrideKey identifies one (operation, pattern) pair for parameter
// override lookup. Some patterns are declared by more than one operation
// (e.g. GET, PATCH and DELETE all address "/api/v1/agents/{id}"); an entry
// here overrides the pattern-only fallback in patternOverrides for that
// specific operation only — typically to re-point a destructive verb at a
// disposable fixture instance instead of the one longer-lived reads and
// updates on the same pattern still need.
type overrideKey struct {
	OperationID string
	Pattern     string
}

// opPatternOverrides holds every parameter override that must be resolved
// by (operation, pattern) rather than by pattern alone, because the pattern
// is shared with a different operation that needs a different fixture
// instance. This is the only place a destructive verb is re-pointed at a
// disposable twin; an operation not listed here uses whatever
// patternOverrides gives its pattern.
func opPatternOverrides(f idFixtures) map[overrideKey]map[string]string {
	return map[overrideKey]map[string]string{
		// Chat: the DM variants address the caller's DMs; topic delete
		// takes the disposable second thread; message delete its own
		// message.
		{"chat.topic.delete", "/api/v1/chat/topics/{id}"}:                               {"id": f.chat.topicDel},
		{"chat.conversation.history.dm", "/api/v1/chat/conversations/{id}/messages"}:    {"id": f.chat.dmAgent},
		{"chat.conversation.send.dmagent", "/api/v1/chat/conversations/{id}/messages"}:  {"id": f.chat.dmAgent},
		{"chat.conversation.send.dmuser", "/api/v1/chat/conversations/{id}/messages"}:   {"id": f.chat.dmUser},
		{"chat.conversation.readstate.dm", "/api/v1/chat/conversations/{id}/read"}:      {"id": f.chat.dmAgent},
		{"chat.conversation.markread.dm", "/api/v1/chat/conversations/{id}/read"}:       {"id": f.chat.dmAgent},
		{"chat.conversation.markunread.dm", "/api/v1/chat/conversations/{id}/unread"}:   {"id": f.chat.dmAgent},
		{"chat.conversation.mute.dm", "/api/v1/chat/conversations/{id}/mute"}:           {"id": f.chat.dmAgent},
		{"chat.conversation.pin.dm", "/api/v1/chat/conversations/{id}/pin"}:             {"id": f.chat.dmAgent},
		{"chat.conversation.typing.dm", "/api/v1/chat/conversations/{id}/typing"}:       {"id": f.chat.dmAgent},
		{"chat.message.delete", "/api/v1/chat/conversations/{id}/messages/{messageId}"}: {"id": f.chatTopic, "messageId": f.chat.topicMessageDelete},
		{"agent.lifecycle.delete", "/api/v1/agents/{id}"}:                               {"id": f.agentDel},
		{"agent.lifecycle.delete", "/api/v1/projects/{projectId}/agents/{id}"}:          {"projectId": f.project, "id": f.agentDelProject},
		{"agent.port.register", "/api/v1/agents/{id}/ports"}:                            {"id": f.agentPorts},
		{"project.lifecycle.delete", "/api/v1/projects/{id}"}:                           {"id": f.projectDel},
		{"group.delete", "/api/v1/groups/{id}"}:                                         {"id": f.groupDel},
		{"user.admin.delete", "/api/v1/users/{id}"}:                                     {"id": f.userDel},
		{"testidentity.delete", "/api/v1/test-identities/{id}"}:                         {"id": f.testIdentityDel},
		{"skill.delete", "/api/v1/skills/{id}"}:                                         {"id": f.skillDel},
		{"skill.register", "/api/v1/skill-registries/{id}"}:                             {"id": f.skillRegistryDel},
		{"template.delete", "/api/v1/templates/{id}"}:                                   {"id": f.templateDel},
		{"harnessconfig.delete", "/api/v1/harness-configs/{id}"}:                        {"id": f.harnessConfigDel},
		// User-scope branches address the caller's user-scope records; a
		// clone copies the global fixture into the caller's user scope.
		{"template.user.delete", "/api/v1/users/me/templates/{id}"}:               {"id": f.userTemplateDel},
		{"template.user.delete", "/api/v1/templates/{id}"}:                        {"id": f.userScopeTemplateDel},
		{"template.user.clone", "/api/v1/templates/{id}/clone"}:                   {"id": f.template},
		{"template.user.reimport", "/api/v1/templates/{id}/reimport"}:             {"id": f.userTemplate},
		{"harnessconfig.user.clone", "/api/v1/harness-configs/{id}/clone"}:        {"id": f.harnessConfig},
		{"harnessconfig.user.delete", "/api/v1/harness-configs/{id}"}:             {"id": f.userHarnessConfigDel},
		{"harnessconfig.user.reimport", "/api/v1/harness-configs/{id}/reimport"}:  {"id": f.userHarnessConfig},
		{"broker.delete", "/api/v1/runtime-brokers/{id}"}:                         {"id": f.runtimeBrokerDel},
		{"gcp.identity.delete", "/api/v1/gcp-service-accounts/{id}"}:              {"id": f.gcpSADel},
		{"role.definition.delete", "/api/v1/admin/roles/{id}"}:                    {"id": f.roleDefinitionDel},
		{"quota.update", "/api/v1/admin/limits/{id}"}:                             {"id": f.limitUD},
		{"quota.delete", "/api/v1/admin/limits/{id}"}:                             {"id": f.limitUD},
		{"quota.update", "/api/v1/admin/entitlements/{id}"}:                       {"id": f.entitlementUD},
		{"quota.delete", "/api/v1/admin/entitlements/{id}"}:                       {"id": f.entitlementUD},
		{"access.constraint.update", "/api/v1/admin/access-constraints/{id}"}:     {"id": f.accessConstraintUD},
		{"access.constraint.delete", "/api/v1/admin/access-constraints/{id}"}:     {"id": f.accessConstraintUD},
		{"project.membership.update", "/api/v1/projects/{id}/members/{memberId}"}: {"id": f.project, "memberId": f.projectMembershipUpdate},
		{"project.visitors.update", "/api/v1/projects/{id}/visitors/{visitorId}"}: {"id": f.project, "visitorId": f.projectVisitorUpdate},
		// Visitor rule update takes its own rule; remove takes the other.
		{"project.visitors.rules.update", "/api/v1/projects/{id}/visitors/rules/{ruleId}"}: {"id": f.project, "ruleId": f.projectVisitorRuleUpd},
		// Grant revoke and grant read share their pattern; revoke takes
		// the disposable twin grant.
		{"agent.delegation.revoke", "/api/v1/agents/{id}/delegations/{grantId}"}: {"id": f.agent, "grantId": f.delegation.grantRevoke},

		// Group and direct conversation reads share their patterns.
		{"project.conversation.read", "/api/v1/conversations/{id}"}:                           {"id": f.inbox.groupConversation},
		{"project.conversation.read", "/api/v1/conversations/{id}/messages"}:                  {"id": f.inbox.groupConversation},
		{"project.conversation.read", "/api/v1/conversations/{id}/messages/{messageId}"}:      {"id": f.inbox.groupConversation, "messageId": f.inbox.groupMessage},
		{"inbox.conversation.direct.read", "/api/v1/conversations/{id}"}:                      {"id": f.inbox.directConversation},
		{"inbox.conversation.direct.read", "/api/v1/conversations/{id}/messages"}:             {"id": f.inbox.directConversation},
		{"inbox.conversation.direct.read", "/api/v1/conversations/{id}/messages/{messageId}"}: {"id": f.inbox.directConversation, "messageId": f.inbox.directMessage},

		// Thread group delete uses its disposable twin.
		{"inbox.threadgroup.delete", "/api/v1/thread-groups/{id}"}: {"id": f.inbox.threadGroupDel},

		// The project-scoped role-binding DELETE removes its own project
		// binding; the hub-path DELETE keeps the system-scoped fixture.
		{"project.membership.rolebinding.delete", "/api/v1/admin/role-bindings/{id}"}: {"id": f.roleBindingProjectDel},
	}
}

// patternOverrides maps a literal EntryPoint Pattern to the placeholder
// values it needs, built once real fixture IDs are known. A pattern absent
// from this map, and from opPatternOverrides, is substituted with the
// generic placeholder for every "{name}" segment, which is correct for
// every entry whose live handler dispatches on method (and, where
// applicable, resolves its target) without an existence check that runs
// before the method switch.
func patternOverrides(f idFixtures) map[string]map[string]string {
	return map[string]map[string]string{
		// --- artifact family ---
		"/api/v1/artifacts/{id}":                             {"id": f.artifact},
		"/api/v1/artifacts/{id}/files/{path}":                {"id": f.artifact, "path": "live.md"},
		"/api/v1/artifacts/{id}/versions/{seq}/files/{path}": {"id": f.artifact, "seq": "1", "path": "live.md"},
		"/api/v1/artifacts/{id}/grants":                      {"id": f.artifact},
		"/api/v1/artifacts/{id}/grants/{grantId}":            {"id": f.artifact},
		"/api/v1/artifacts/{id}/links":                       {"id": f.artifact},
		"/api/v1/artifacts/{id}/links/{linkId}":              {"id": f.artifact},
		"/api/v1/artifacts/{id}/versions":                    {"id": f.artifact},
		"/api/v1/artifacts/{id}/versions/{seq}":              {"id": f.artifact, "seq": "1"},
		"/api/v1/artifacts/{id}/versions/{seq}/finalize":     {"id": f.artifact, "seq": "1"},
		"/api/v1/artifacts/{id}/versions/{seq}/view":         {"id": f.artifact, "seq": "1"},

		// --- test identity family ---
		"/api/v1/test-identities/{id}/token":          {"id": f.testIdentity},
		"/api/v1/test-identities/{id}/web-login-code": {"id": f.testIdentity},
		"/api/v1/test-identity-delegations/{id}":      {"id": f.testIdentityDelegation},

		// --- agent family ---
		"/api/v1/agents/{id}":                                                  {"id": f.agent},
		"/api/v1/agents/{id}/ports":                                            {"id": f.agent},
		"/api/v1/agents/{id}/ports/{port}/proxy":                               {"id": f.agent, "port": f.agentPort},
		"/api/v1/agents/{id}/ports/{port}/proxy/{subpath}":                     {"id": f.agent, "port": f.agentPort, "subpath": "x"},
		"/api/v1/agents/{id}/set_message_mode":                                 {"id": f.agent},
		"/api/v1/agents/{id}/delegations":                                      {"id": f.agent},
		"/api/v1/agents/{id}/delegations/{grantId}/exchange":                   {"id": f.agent, "grantId": f.delegation.grant},
		"/api/v1/agents/{id}/delegations/{grantId}":                            {"id": f.agent, "grantId": f.delegation.grant},
		"/api/v1/agents/{id}/delegations/{grantId}/credentials/{credentialId}": {"id": f.agent, "grantId": f.delegation.grant, "credentialId": f.delegation.credential},
		"/api/v1/projects/{projectId}/agents/{id}/set_message_mode":            {"projectId": f.project, "id": f.agent},
		"/api/v1/agents/{id}/ports/{port}":                                     {"id": f.agentPorts, "port": f.agentPortsPort},
		"/api/v1/projects/{projectId}/agents":                                  {"projectId": f.project},
		"/api/v1/projects/{projectId}/agents/stop-all":                         {"projectId": f.project},
		"/api/v1/projects/{projectId}/agents/{id}":                             {"projectId": f.project, "id": f.agent},
		"/api/v1/agents/{id}/status":                                           {"id": f.agent},
		"/api/v1/projects/{projectId}/agents/{id}/status":                      {"projectId": f.project, "id": f.agent},
		"/api/v1/agents/{id}/workspace":                                        {"id": f.agent},
		"/api/v1/agents/{id}/workspace/sync-from":                              {"id": f.agent},
		"/api/v1/agents/{id}/workspace/sync-to":                                {"id": f.agent},
		"/api/v1/agents/{id}/workspace/sync-to/finalize":                       {"id": f.agent},
		"/api/v1/agents/{id}/logs":                                             {"id": f.agent},
		"/api/v1/projects/{projectId}/agents/{id}/logs":                        {"projectId": f.project, "id": f.agent},
		"/api/v1/agents/{id}/cloud-logs":                                       {"id": f.agent},
		"/api/v1/projects/{projectId}/agents/{id}/cloud-logs":                  {"projectId": f.project, "id": f.agent},
		"/api/v1/agents/{id}/messages":                                         {"id": f.agent},
		"/api/v1/agents/{id}/message-logs":                                     {"id": f.agent},
		"/api/v1/projects/{projectId}/agents/{id}/message-logs":                {"projectId": f.project, "id": f.agent},
		"/api/v1/agents/{id}/metrics/summary":                                  {"id": f.agent},
		"/api/v1/agents/{id}/keys":                                             {"id": f.agent},
		"/api/v1/projects/{projectId}/agents/{id}/keys":                        {"projectId": f.project, "id": f.agent},
		"/api/v1/agents/{id}/message":                                          {"id": f.agent},
		"/api/v1/projects/{projectId}/agents/{id}/message":                     {"projectId": f.project, "id": f.agent},

		// --- agent self-access family (agent JWT routes) ---
		"/api/v1/agents/{id}/token/refresh":                         {"id": f.agent},
		"/api/v1/agents/{id}/refresh-token":                         {"id": f.agent},
		"/api/v1/agents/{id}/outbound-message":                      {"id": f.agent},
		"/api/v1/agents/{id}/metrics":                               {"id": f.agent},
		"/api/v1/projects/{projectId}/agents/{id}/outbound-message": {"projectId": f.project, "id": f.agent},

		// --- agent lifecycle family ---
		"/api/v1/agents/{id}/start":                            {"id": f.agentLifecycle},
		"/api/v1/agents/{id}/stop":                             {"id": f.agentLifecycle},
		"/api/v1/agents/{id}/suspend":                          {"id": f.agentLifecycle},
		"/api/v1/agents/{id}/restart":                          {"id": f.agentLifecycle},
		"/api/v1/projects/{projectId}/agents/{id}/start":       {"projectId": f.project, "id": f.agentLifecycle},
		"/api/v1/projects/{projectId}/agents/{id}/stop":        {"projectId": f.project, "id": f.agentLifecycle},
		"/api/v1/projects/{projectId}/agents/{id}/suspend":     {"projectId": f.project, "id": f.agentLifecycle},
		"/api/v1/projects/{projectId}/agents/{id}/restart":     {"projectId": f.project, "id": f.agentLifecycle},
		"/api/v1/agents/{id}/restore":                          {"id": f.agentRestore},
		"/api/v1/projects/{projectId}/agents/{id}/restore":     {"projectId": f.project, "id": f.agentRestoreProject},
		"/api/v1/agents/{id}/exec":                             {"id": f.agent},
		"/api/v1/projects/{projectId}/agents/{id}/exec":        {"projectId": f.project, "id": f.agent},
		"/api/v1/agents/{id}/env":                              {"id": f.agentProvisioning},
		"/api/v1/projects/{projectId}/agents/{id}/env":         {"projectId": f.project, "id": f.agentProvisioning},
		"/api/v1/agents/{id}/reset-auth":                       {"id": f.agent},
		"/api/v1/projects/{projectId}/agents/{id}/reset-auth":  {"projectId": f.project, "id": f.agent},
		"/api/v1/agents/{id}/reincarnate":                      {"id": f.agent},
		"/api/v1/projects/{projectId}/agents/{id}/reincarnate": {"projectId": f.project, "id": f.agent},

		// --- project family ---
		"/api/v1/projects/{id}":                      {"id": f.project},
		"/api/v1/projects/{id}/agents":               {"id": f.project},
		"/api/v1/projects/{id}/members":              {"id": f.project},
		"/api/v1/projects/{id}/members/{memberId}":   {"id": f.project, "memberId": f.projectMembership},
		"/api/v1/projects/{id}/transfer-ownership":   {"id": f.project},
		"/api/v1/projects/{id}/visitors":             {"id": f.project},
		"/api/v1/projects/{id}/visitors/{visitorId}": {"id": f.project, "visitorId": f.projectVisitor},
		// Visitor rules (the item pattern addresses the rule remove takes).
		"/api/v1/projects/{id}/visitors/rules":          {"id": f.project},
		"/api/v1/projects/{id}/visitors/rules/{ruleId}": {"id": f.project, "ruleId": f.projectVisitorRule},

		// The principal PUT (an empty body) and DELETE address a principal
		// that holds a project binding the DELETE removes.
		"/api/v1/projects/{id}/members/principals/{type}/{principalId}": {"id": f.project, "type": "user", "principalId": f.memberPrincipalDel},

		// --- project-route operations family ---
		"/api/v1/projects/{id}/env":                               {"id": f.project},
		"/api/v1/projects/{id}/env/{key}":                         {"id": f.project, "key": f.projectEnvKey},
		"/api/v1/projects/{id}/secrets":                           {"id": f.project},
		"/api/v1/projects/{id}/secrets/{key}":                     {"id": f.project, "key": f.projectSecretKey},
		"/api/v1/projects/{id}/providers":                         {"id": f.project},
		"/api/v1/projects/{id}/shared-dirs":                       {"id": f.project},
		"/api/v1/projects/{id}/shared-dirs/{name}":                {"id": f.project, "name": f.projectSharedDir},
		"/api/v1/projects/{id}/shared-dirs/{name}/archive":        {"id": f.project, "name": f.projectSharedDir},
		"/api/v1/projects/{id}/shared-dirs/{name}/files":          {"id": f.project, "name": f.projectSharedDir},
		"/api/v1/projects/{id}/shared-dirs/{name}/files/{path}":   {"id": f.project, "name": f.projectSharedDir, "path": "li.txt"},
		"/api/v1/projects/{id}/injected-skills":                   {"id": f.project},
		"/api/v1/projects/{id}/injected-skills/{entryId}":         {"id": f.project, "entryId": f.projectSkillInjection},
		"/api/v1/users/me/injected-skills/{id}":                   {"id": f.userSkillInjection},
		"/api/v1/users/me/templates/{id}":                         {"id": f.userTemplate},
		"/api/v1/users/me/templates/{id}/upload":                  {"id": f.userTemplate},
		"/api/v1/users/me/templates/{id}/finalize":                {"id": f.userTemplate},
		"/api/v1/users/me/templates/{id}/download":                {"id": f.userTemplate},
		"/api/v1/projects/{id}/gcp-service-accounts":              {"id": f.project},
		"/api/v1/projects/{id}/message-logs":                      {"id": f.project},
		"/api/v1/projects/{id}/broadcast":                         {"id": f.project},
		"/api/v1/projects/{id}/metrics-summary":                   {"id": f.project},
		"/api/v1/projects/{id}/metrics/summary":                   {"id": f.project},
		"/api/v1/projects/{id}/metrics":                           {"id": f.project},
		"/api/v1/projects/{id}/pre-start-hooks":                   {"id": f.project},
		"/api/v1/projects/{id}/pre-start-hooks/{hookId}":          {"id": f.project, "hookId": f.projectPreStartHook},
		"/api/v1/projects/{id}/pre-start-hooks/{hookId}/activate": {"id": f.project, "hookId": f.projectPreStartHook},
		"/api/v1/projects/{id}/settings":                          {"id": f.project},
		"/api/v1/projects/{id}/settings/resolved":                 {"id": f.project},
		"/api/v1/projects/{id}/messaging-policy":                  {"id": f.project},
		"/api/v1/projects/{id}/set-template":                      {"id": f.project},
		"/api/v1/projects/{id}/clone":                             {"id": f.project},
		"/api/v1/projects/{id}/discover-templates":                {"id": f.project},
		"/api/v1/projects/{id}/import-templates":                  {"id": f.project},
		"/api/v1/projects/{id}/discover-harness-configs":          {"id": f.project},
		"/api/v1/projects/{id}/import-harness-configs":            {"id": f.project},
		"/api/v1/projects/{id}/dav/{path}":                        {"id": f.project, "path": "li.txt"},
		"/api/v1/projects/{id}/sync/status":                       {"id": f.project},
		"/api/v1/projects/{id}/workspace/archive":                 {"id": f.project},
		"/api/v1/projects/{id}/workspace/cache/notify":            {"id": f.project},
		"/api/v1/projects/{id}/workspace/cache/refresh":           {"id": f.project},
		"/api/v1/projects/{id}/workspace/cache/status":            {"id": f.project},
		"/api/v1/projects/{id}/workspace/files":                   {"id": f.project},
		"/api/v1/projects/{id}/workspace/files/{path}":            {"id": f.project, "path": "li-files.txt"},
		"/api/v1/projects/{id}/workspace/pull":                    {"id": f.project},
		"/api/v1/projects/{id}/github-installation":               {"id": f.project},
		"/api/v1/projects/{id}/github-status":                     {"id": f.project},
		"/api/v1/projects/{id}/github-permissions":                {"id": f.project},
		"/api/v1/projects/{id}/git-identity":                      {"id": f.project},
		"/api/v1/projects/{id}/members/assignable-roles":          {"id": f.project},

		// --- group family ---
		"/api/v1/groups/{id}":                                 {"id": f.group},
		"/api/v1/groups/{id}/members":                         {"id": f.group},
		"/api/v1/groups/{id}/members/{memberType}/{memberId}": {"id": f.group, "memberType": "user", "memberId": f.member},

		// --- user family ---
		"/api/v1/users/{id}":                 {"id": f.user},
		"/api/v1/users/{id}/revoke-sessions": {"id": f.user},

		// --- skill family ---
		"/api/v1/skills/{id}":                          {"id": f.skill},
		"/api/v1/skills/{id}/versions":                 {"id": f.skillFiles},
		"/api/v1/skills/{id}/versions/{vid}":           {"id": f.skillFiles, "vid": f.skillVersion},
		"/api/v1/skills/{id}/versions/{vid}/deprecate": {"id": f.skillFiles, "vid": f.skillVersion},
		"/api/v1/skills/{id}/download":                 {"id": f.skillFiles},
		"/api/v1/skills/{id}/resolve":                  {"id": f.skillFiles},
		"/api/v1/skills/{id}/upload":                   {"id": f.skillFiles},
		"/api/v1/skills/{id}/finalize":                 {"id": f.skillFiles},
		"/api/v1/skills/{id}/files/{path}":             {"id": f.skillFiles, "path": "SKILL.md"},

		// --- skill registry family ---
		"/api/v1/skill-registries/{id}":       {"id": f.skillRegistry},
		"/api/v1/skill-registries/{id}/pin":   {"id": f.skillRegistry},
		"/api/v1/skill-registries/{id}/pins":  {"id": f.skillRegistry},
		"/api/v1/skill-registries/{id}/unpin": {"id": f.skillRegistry},

		// --- template family ---
		"/api/v1/templates/{id}":              {"id": f.template},
		"/api/v1/templates/{id}/download":     {"id": f.templateFiles},
		"/api/v1/templates/{id}/validate":     {"id": f.templateFiles},
		"/api/v1/templates/{id}/upload":       {"id": f.templateFiles},
		"/api/v1/templates/{id}/finalize":     {"id": f.templateFiles},
		"/api/v1/templates/{id}/files":        {"id": f.templateFiles},
		"/api/v1/templates/{id}/files/{path}": {"id": f.templateFiles, "path": "README.md"},
		"/api/v1/templates/{id}/clone":        {"id": f.template},
		"/api/v1/templates/{id}/reimport":     {"id": f.template},

		// --- harness config family ---
		"/api/v1/harness-configs/{id}":              {"id": f.harnessConfig},
		"/api/v1/harness-configs/{id}/download":     {"id": f.harnessConfigFiles},
		"/api/v1/harness-configs/{id}/validate":     {"id": f.harnessConfigFiles},
		"/api/v1/harness-configs/{id}/image-status": {"id": f.harnessConfigFiles},
		"/api/v1/harness-configs/{id}/check-image":  {"id": f.harnessConfigFiles},
		"/api/v1/harness-configs/{id}/upload":       {"id": f.harnessConfigFiles},
		"/api/v1/harness-configs/{id}/finalize":     {"id": f.harnessConfigFiles},
		"/api/v1/harness-configs/{id}/files":        {"id": f.harnessConfigFiles},
		"/api/v1/harness-configs/{id}/files/{path}": {"id": f.harnessConfigFiles, "path": "README.md"},
		"/api/v1/harness-configs/{id}/clone":        {"id": f.harnessConfig},
		"/api/v1/harness-configs/{id}/reimport":     {"id": f.harnessConfig},

		// --- schedule family ---
		"/api/v1/projects/{projectId}/scheduled-events/{id}": {"projectId": f.project, "id": f.scheduledEvent},
		"/api/v1/projects/{projectId}/schedules/{id}":        {"projectId": f.project, "id": f.schedule},
		"/api/v1/projects/{projectId}/scheduled-events":      {"projectId": f.project},
		"/api/v1/projects/{projectId}/schedules":             {"projectId": f.project},

		// Schedule sub-actions on the seeded schedule.
		"/api/v1/projects/{projectId}/schedules/{id}/history": {"projectId": f.project, "id": f.schedule},
		"/api/v1/projects/{projectId}/schedules/{id}/pause":   {"projectId": f.project, "id": f.schedule},
		"/api/v1/projects/{projectId}/schedules/{id}/resume":  {"projectId": f.project, "id": f.schedule},

		// --- quota family (read defaults; quota.update/.delete are
		// re-pointed at the disposable UD instances by opPatternOverrides) ---
		"/api/v1/admin/limits/{id}":              {"id": f.limit},
		"/api/v1/admin/limits/{id}/entitlements": {"id": f.limit},
		"/api/v1/admin/entitlements/{id}":        {"id": f.entitlement},
		"/api/v1/admin/usage/{limit}":            {"limit": f.limit},

		// --- GCP identity family (gcp.identity.delete is re-pointed at the
		// disposable instance by opPatternOverrides) ---
		"/api/v1/gcp-service-accounts/{id}":        {"id": f.gcpSA},
		"/api/v1/gcp-service-accounts/{id}/verify": {"id": f.gcpSA},

		// --- access constraint family (read default; update/.delete are
		// re-pointed at the disposable UD instance by opPatternOverrides) ---
		"/api/v1/admin/access-constraints/{id}":                     {"id": f.accessConstraint},
		"/api/v1/admin/access-constraints/{id}/affected-principals": {"id": f.accessConstraint},
		"/api/v1/admin/access-constraints/{id}/audit":               {"id": f.accessConstraint},
		"/api/v1/admin/access-constraint-previews/{jobId}":          {"jobId": f.previewJob},
		"/api/v1/admin/access-constraint-previews/{jobId}/result":   {"jobId": f.previewJob},

		// --- credential token family ---
		"/api/v1/auth/tokens/{id}/revoke": {"id": f.uatRevokePost},
		"/api/v1/auth/tokens/{id}":        {"id": f.uatRevokeDelete},

		// --- invite family ---
		"/api/v1/admin/invites/{id}":        {"id": f.invite},
		"/api/v1/admin/invites/{id}/revoke": {"id": f.invite},
		"/api/v1/admin/allow-list/{email}":  {"email": f.allowListEmail},

		// --- env var family ---
		"/api/v1/env/{key}": {"key": f.envVarKey},

		// --- runtime broker family ---
		"/api/v1/runtime-brokers/{id}":                         {"id": f.runtimeBroker},
		"/api/v1/runtime-brokers/{id}/settings":                {"id": f.runtimeBroker},
		"/api/v1/runtime-brokers/{id}/heartbeat":               {"id": f.runtimeBroker},
		"/api/v1/runtime-brokers/{id}/projects":                {"id": f.runtimeBroker},
		"/api/v1/runtime-brokers/{id}/env":                     {"id": f.runtimeBroker},
		"/api/v1/runtime-brokers/{id}/env/{key}":               {"id": f.runtimeBroker, "key": f.brokerEnvKey},
		"/api/v1/runtime-brokers/{id}/secrets":                 {"id": f.runtimeBroker},
		"/api/v1/runtime-brokers/{id}/secrets/{key}":           {"id": f.runtimeBroker, "key": f.brokerSecretKey},
		"/api/v1/runtime-brokers/{id}/message-failures":        {"id": f.runtimeBroker},
		"/api/v1/runtime-brokers/{id}/agents/{agentId}/launch": {"id": f.runtimeBroker, "agentId": f.agent},

		// --- github app family ---
		"/api/v1/github-app/installations/{id}": {"id": f.githubInstallationID},

		// --- role family (role.definition.delete is re-pointed at the
		// disposable instance by opPatternOverrides) ---
		"/api/v1/admin/roles/{id}":           {"id": f.roleDefinition},
		"/api/v1/admin/roles/{id}/export":    {"id": f.roleDefinition},
		"/api/v1/admin/roles/{id}/duplicate": {"id": f.roleDefinition},
		"/api/v1/admin/role-bindings/{id}":   {"id": f.roleBindingDel},

		// --- maintenance family: run entries deliberately use the OTHER
		// category's key, so executeOperation/executeMigration's own
		// category check rejects the request with 400 before anything
		// executes (admin_maintenance.go:345-350, :138-142) — never a fake
		// key (which would just 404, telling us nothing) and never a
		// same-category real key (which would actually run it).
		"/api/v1/admin/maintenance/operations/{id}":     {"id": f.maintenanceOpKey},
		"/api/v1/admin/maintenance/operations/{id}/run": {"id": f.maintenanceMigrationKey},
		"/api/v1/admin/maintenance/migrations/{id}/run": {"id": f.maintenanceOpKey},

		// --- chat family ---
		"/api/v1/chat/spaces/{id}/threads":                     {"id": f.project},
		"/api/v1/chat/spaces/{id}/members":                     {"id": f.project},
		"/api/v1/chat/spaces/{id}/read":                        {"id": f.project},
		"/api/v1/chat/spaces/{id}/emoji":                       {"id": f.project},
		"/api/v1/chat/topics/{id}":                             {"id": f.chatTopic},
		"/api/v1/chat/conversations/{id}/messages":             {"id": f.chatTopic},
		"/api/v1/chat/conversations/{id}/messages/{messageId}": {"id": f.chatTopic, "messageId": f.chat.topicMessageEdit},
		"/api/v1/chat/conversations/{id}/read":                 {"id": f.chatTopic},
		"/api/v1/chat/conversations/{id}/unread":               {"id": f.chatTopic},
		"/api/v1/chat/conversations/{id}/mute":                 {"id": f.chatTopic},
		"/api/v1/chat/conversations/{id}/pin":                  {"id": f.chatTopic},
		"/api/v1/chat/conversations/{id}/typing":               {"id": f.chatTopic},
		"/api/v1/chat/conversations/{id}/interagent":           {"id": f.chat.dmAgent},
		"/api/v1/chat/conversations/{id}/promote":              {"id": f.chat.dmAgent},

		// --- integrations family ---
		"/api/v1/admin/integrations/{name}":         {"name": f.integrationName},
		"/api/v1/admin/integrations/{name}/health":  {"name": f.integrationName},
		"/api/v1/admin/integrations/{name}/config":  {"name": f.integrationName},
		"/api/v1/admin/integrations/{name}/restart": {"name": f.integrationName},
		"/api/v1/admin/integrations/{name}/update":  {"name": f.integrationName},

		// --- lifecycle hooks family ---
		"/api/v1/admin/lifecycle-hooks/{id}": {"id": f.lifecycleHook},

		// --- inbox, conversation and notification family ---
		"/api/v1/messages/{id}":                    {"id": f.inbox.message},
		"/api/v1/messages/{id}/read":               {"id": f.inbox.message},
		"/api/v1/notifications/{id}/ack":           {"id": f.inbox.notification},
		"/api/v1/notifications/subscriptions/{id}": {"id": f.inbox.subscription},
		"/api/v1/notifications/templates/{id}":     {"id": f.inbox.template},
		"/api/v1/conversations/{id}/default-agent": {"id": f.inbox.groupConversation},
		"/api/v1/conversations/{id}/participants":  {"id": f.inbox.groupConversation},
		"/api/v1/conversations/{id}/leave":         {"id": f.inbox.leaveConversation},

		// --- chat thread group family ---
		"/api/v1/thread-groups/{id}":                          {"id": f.inbox.threadGroup},
		"/api/v1/thread-groups/{id}/threads":                  {"id": f.inbox.threadGroup},
		"/api/v1/thread-groups/{id}/threads/{conversationId}": {"id": f.inbox.threadGroup, "conversationId": f.inbox.groupConversation},

		// --- hub pre-start hooks family ---
		"/api/v1/pre-start-hooks/{id}":          {"id": f.hubPreStartHook},
		"/api/v1/pre-start-hooks/{id}/activate": {"id": f.hubPreStartHook},
	}
}

// queryOverrides holds a literal query string to append to a pattern's
// substituted path. Only /chat/prefs needs one today: handleChatPrefs
// (handlers_chat_prefs.go) requires an agentId query parameter to resolve
// which agent's thread preferences to read or write, before it does
// anything else (including its method switch).
func queryOverrides(f idFixtures) map[string]string {
	return map[string]string{
		"/api/v1/chat/prefs": "agentId=" + f.agent,
		// The single-file publish is selected by ?name= (pkg/artifacts).
		"/api/v1/artifacts": "name=live-inventory.txt&scope=" + f.project,
		// handleConversationResolve 400s without a reference.
		"/api/v1/conversations/resolve": "reference=conv:" + f.inbox.groupConversation,
		// handleAdminEffectiveAccess 400s without a principal.
		"/api/v1/admin/effective-access": "principalType=user&principalId=" + f.user,
		// handleListThreadGroups 400s without a project for a user.
		"/api/v1/thread-groups": "projectId=" + f.project,
		// Skill file reads and writes name the version.
		"/api/v1/skills/{id}/files/{path}": "version=1.0.0",
	}
}

// bodyOverrides holds a real request body for a (operation, pattern) pair
// whose handler validates the body before using the path's ID for anything,
// so this test's default generic body (an empty JSON object) would 400
// before the suffix or control check could observe path- or method-related
// behavior at all. An entry not listed here uses the empty object.
func bodyOverrides(f idFixtures) map[overrideKey]map[string]interface{} {
	return map[overrideKey]map[string]interface{}{
		// Agent delegation issuance and exchange: valid bodies, so the
		// probes reach the credential checks (the dev token is refused
		// there with 403 credential_not_admitted) instead of a body 400.
		{"agent.delegation.create", "/api/v1/agents/{id}/delegations"}: {
			"boundary": map[string]string{"kind": "hub"}, "permissions": []string{"agent:read"}, "name": "live-inventory",
		},
		{"agent.delegation.exchange", "/api/v1/agents/{id}/delegations/{grantId}/exchange"}: {"audience": "scion-hub"},
		// User access credential for a peer hub: an allowed audience, so
		// an admitted caller gets 200 instead of a body 400
		// (seedLiveInventoryFixtures allows it).
		{"federation.token.mint", "/api/v1/auth/federation-token"}: {"audience": liveInventoryFederationAudience},
		// updateProjectMemberRole (handlers_project_members.go) 400s
		// "roleDefinitionId is required" on an empty body before the
		// binding ID is used for anything; with a real role ID, the
		// handler actually looks up the binding, which is what both the
		// control and the suffix check need to observe.
		{"project.membership.update", "/api/v1/projects/{id}/members/{memberId}"}: {"roleDefinitionId": f.projectMemberRoleID},
		// updateLimitDefinition (handlers_quota.go) overwrites
		// resourceType and unit from the request unconditionally
		// (handlers_quota.go:370-371), so an override missing either
		// field 500s on the store update, on the bare path as much as the
		// suffixed one. All four fields make the positive check exercise
		// a real update (200); the suffix check stays excluded because
		// the suffixed PUT also returns 200 — the suffix is discarded by
		// handleAdminLimitByID's parts[0] extraction (see the exclusion
		// reason above), not rejected.
		{"quota.update", "/api/v1/admin/limits/{id}"}: {"name": "li-limit-ud-updated", "resourceType": "agent", "unit": "count", "defaultValue": 10},
		// handleAgentExec (handlers_agents_core.go) 400s "command is
		// required" on the generic empty body before its own agent lookup
		// and dispatcher check; a real command lets the positive check
		// reach GetDispatcher()'s nil check (503) instead. Both callers
		// (handleAgentAction, handleProjectAgentAction) resolve the agent
		// before calling handleAgentExec, so this override is about the
		// body check, not the agent lookup.
		{"agent.lifecycle.exec", "/api/v1/agents/{id}/exec"}:                      {"command": []interface{}{"echo", "li-inventory-probe"}},
		{"agent.lifecycle.exec", "/api/v1/projects/{projectId}/agents/{id}/exec"}: {"command": []interface{}{"echo", "li-inventory-probe"}},
		// submitAgentEnv (handlers_agents_core.go) 400s "env map is required"
		// on the generic empty body before its own agent lookup (both
		// callers have already resolved the agent); a real env map lets the
		// positive check reach the phase gate and then GetDispatcher()'s nil
		// check (400 "no runtime broker available").
		{"agent.lifecycle.env", "/api/v1/agents/{id}/env"}:                      {"env": map[string]interface{}{"LI_ENV_VAR": "1"}},
		{"agent.lifecycle.env", "/api/v1/projects/{projectId}/agents/{id}/env"}: {"env": map[string]interface{}{"LI_ENV_VAR": "1"}},
		// createGCPServiceAccount (handlers_gcp_identity.go) validates the
		// body before it loads the project or checks authorization; a real
		// email lets every probe reach those steps.
		{"project.gcpsa.create", "/api/v1/projects/{id}/gcp-service-accounts"}: {"email": "li-project-sa@li-gcp-project.iam.gserviceaccount.com"},
		// User-scope writes: valid bodies, so the probes reach the
		// user-scope branch past body validation.
		{"template.user.create", "/api/v1/users/me/templates"}:             {"name": "li-user-template-created"},
		{"template.user.update", "/api/v1/users/me/templates/{id}"}:        {"name": "li-user-template-renamed"},
		{"template.user.clone", "/api/v1/templates/{id}/clone"}:            {"name": "li-user-template-clone", "scope": "user"},
		{"harnessconfig.user.clone", "/api/v1/harness-configs/{id}/clone"}: {"name": "li-user-hc-clone", "scope": "user"},
		{"skill.user.create", "/api/v1/skills"}:                            {"name": "li-user-skill", "scope": "user"},
		{"template.user.import", "/api/v1/resources/import"}:               {"kind": "template", "scope": "user", "sourceUrl": "https://example.invalid/li-user"},
		{"template.user.import", "/api/v1/resources/discover"}:             {"kind": "template", "scope": "user", "sourceUrl": "https://example.invalid/li-user"},
		{"harnessconfig.user.import", "/api/v1/resources/import"}:          {"kind": "harness-config", "scope": "user", "sourceUrl": "https://example.invalid/li-user"},
		{"harnessconfig.user.import", "/api/v1/resources/discover"}:        {"kind": "harness-config", "scope": "user", "sourceUrl": "https://example.invalid/li-user"},
		// handleWorkspaceSyncTo and handleWorkspaceSyncToFinalize validate
		// the file list and the manifest before they look up the agent or
		// check authorization.
		{"agent.workspace.sync", "/api/v1/agents/{id}/workspace/sync-to"}: {
			"files": []interface{}{map[string]interface{}{"path": "li.txt", "size": 3, "hash": "li-hash"}},
		},
		{"agent.workspace.sync", "/api/v1/agents/{id}/workspace/sync-to/finalize"}: {
			"manifest": map[string]interface{}{"version": "1", "files": []interface{}{}},
		},
		// beginAgentKeysRequest validates the body before the agent lookup
		// and the authorization check.
		{"agent.lifecycle.keys", "/api/v1/agents/{id}/keys"}:                      {"keys": "li"},
		{"agent.lifecycle.keys", "/api/v1/projects/{projectId}/agents/{id}/keys"}: {"keys": "li"},
		// Global catalog and project-scope branches: valid bodies, so the
		// probes reach the branch past body validation. A discovery or
		// import source that is not a GitHub URL stops after the check.
		{"skill.create", "/api/v1/skills"}:                              {"name": "li-project-skill", "scope": "project", "scopeId": f.project},
		{"skill.global.create", "/api/v1/skills"}:                       {"name": "li-global-skill", "scope": "global"},
		{"skill.discover.project", "/api/v1/skills/discover-directory"}: {"projectId": f.project, "sourceUrl": "https://example.invalid/li-skills"},
		{"skill.discover.global", "/api/v1/skills/discover-directory"}:  {"sourceUrl": "https://example.invalid/li-skills"},
		{"template.create", "/api/v1/templates"}:                        {"name": "li-created-template", "harness": "claude", "scope": "global"},
		{"harnessconfig.create", "/api/v1/harness-configs"}:             {"name": "li-created-hc", "harness": "claude", "scope": "global"},
		{"template.import.global", "/api/v1/resources/import"}:          {"kind": "template", "scope": "global", "sourceUrl": "https://example.invalid/li-global"},
		{"template.import.global", "/api/v1/resources/discover"}:        {"kind": "template", "scope": "global", "sourceUrl": "https://example.invalid/li-global"},
		{"template.import.project", "/api/v1/resources/import"}:         {"kind": "template", "scope": "project", "scopeId": f.project, "sourceUrl": "https://example.invalid/li-project"},
		{"template.import.project", "/api/v1/resources/discover"}:       {"kind": "template", "scope": "project", "scopeId": f.project, "sourceUrl": "https://example.invalid/li-project"},
		{"harnessconfig.import.global", "/api/v1/resources/import"}:     {"kind": "harness-config", "scope": "global", "sourceUrl": "https://example.invalid/li-global"},
		{"harnessconfig.import.global", "/api/v1/resources/discover"}:   {"kind": "harness-config", "scope": "global", "sourceUrl": "https://example.invalid/li-global"},
		{"harnessconfig.import.project", "/api/v1/resources/import"}:    {"kind": "harness-config", "scope": "project", "scopeId": f.project, "sourceUrl": "https://example.invalid/li-project"},
		{"harnessconfig.import.project", "/api/v1/resources/discover"}:  {"kind": "harness-config", "scope": "project", "scopeId": f.project, "sourceUrl": "https://example.invalid/li-project"},
		{"skill.resolve.local", "/api/v1/skills/resolve"}:               {"skills": []map[string]string{{"uri": "skill://li-skill-files"}}},
		{"skill.resolve.github.project", "/api/v1/skills/resolve"}:      {"projectId": f.project, "skills": []map[string]string{{"uri": "gh://li-invalid"}}},
		{"skill.resolve.github.public", "/api/v1/skills/resolve"}:       {"skills": []map[string]string{{"uri": "gh://li-invalid"}}},
	}
}

// substituteLiveInventoryParams replaces every "{name}" placeholder in an
// authzop EntryPoint pattern with a caller-supplied override for that name,
// or a fixed, syntactically valid generic placeholder otherwise, so the
// resulting path can be dispatched through the real server mux.
func substituteLiveInventoryParams(pattern string, overrides map[string]string) string {
	var b strings.Builder
	for i := 0; i < len(pattern); {
		if pattern[i] == '{' {
			end := strings.IndexByte(pattern[i:], '}')
			if end < 0 {
				b.WriteString(pattern[i:])
				break
			}
			name := pattern[i+1 : i+end]
			if v, ok := overrides[name]; ok {
				b.WriteString(v)
			} else {
				b.WriteString("live-inventory-placeholder")
			}
			i += end + 1
			continue
		}
		b.WriteByte(pattern[i])
		i++
	}
	return b.String()
}

func decodeErrorCode(t *testing.T, body []byte) string {
	t.Helper()
	var resp map[string]interface{}
	if err := json.Unmarshal(body, &resp); err != nil {
		return ""
	}
	return rs4ExtractError(resp)
}

// inboxRecords are messaging records owned by one user in one project: a
// message, a notification with its subscription, a subscription template,
// a group conversation (with a message) the user takes part in, a second
// group conversation for the leave entry, and a direct conversation (with
// a message) between the user and an agent.
type inboxRecords struct {
	message             string
	notification        string
	subscription        string
	template            string
	groupConversation   string
	groupMessage        string
	leaveConversation   string
	directConversation  string
	directMessage       string
	createConversations string // display name prefix for conversations a test creates
	threadGroup         string // a chat thread group in the project (ptone/scion#4424)
	threadGroupDel      string // a disposable chat thread group for the delete probes
}

// seedLiveInventoryArtifact publishes a one-file artifact directly through
// the store and blob storage, the way the single-file publish endpoint
// writes it, and returns its id.
func seedLiveInventoryArtifact(t *testing.T, ctx context.Context, st artifacts.Store, blobs storage.Storage, projectID string) string {
	t.Helper()
	content := []byte("# live inventory\n")
	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])
	_, err := blobs.Upload(ctx, artifacts.BlobPath("test-hub-id", digest), bytes.NewReader(content), storage.UploadOptions{ContentType: "text/markdown"})
	require.NoError(t, err)
	now := time.Now()
	a := &artifacts.Artifact{ID: uuid.NewString(), ScopeKind: artifacts.ScopeKindProject, ScopeRef: projectID,
		OwnerKind: artifacts.PrincipalKindUser, OwnerRef: DevUserID, Title: "live", CreatedAt: now, UpdatedAt: now}
	v := &artifacts.Version{ID: uuid.NewString(), ArtifactID: a.ID, Seq: 1, Kind: artifacts.VersionKindPublish,
		EntryPath: "live.md", TotalBytes: int64(len(content)), FileCount: 1, CreatedAt: now, State: artifacts.VersionStateReady}
	files := []artifacts.File{{VersionID: v.ID, Path: "live.md", Size: int64(len(content)), SHA256: digest, MediaType: "text/markdown"}}
	require.NoError(t, st.CreatePublished(ctx, a, v, files, nil))
	return a.ID
}

// liveInventoryDelegation is the agent delegation fixture of the live
// method inventory: a grant bound to the fixture agent, a disposable twin
// grant for the revoke entry, and an active credential of the first grant.
type liveInventoryDelegation struct {
	grant       string
	grantRevoke string
	credential  string
}

// seedLiveInventoryDelegation writes the live inventory's grants and
// credential directly to the store. The dev user is the issuer; the dev
// token reaches them as hub authority.
func seedLiveInventoryDelegation(t *testing.T, ctx context.Context, s store.Store, agentID, projectID string) liveInventoryDelegation {
	t.Helper()
	grant := func(name string) string {
		g := &store.AgentDelegationGrant{
			AgentID: agentID, AgentProjectID: projectID, IssuerUserID: DevUserID,
			BoundaryKind: "hub", CeilingVersion: 1, CeilingPermissionIDs: []string{"agent.read"},
			Name: name, MaxCredentialTTLSeconds: agentDelegationCredentialDefaultTTL,
			ExpiresAt: time.Now().Add(time.Hour), IssuanceAuditID: uuid.New().String(),
		}
		require.NoError(t, s.CreateAgentDelegationGrant(ctx, g))
		return g.ID
	}
	out := liveInventoryDelegation{grant: grant("li-grant"), grantRevoke: grant("li-grant-revoke")}
	_, hash, err := newDelegatedCredentialToken()
	require.NoError(t, err)
	cred := &store.AgentDelegatedCredential{
		GrantID: out.grant, AgentID: agentID, KeyHash: hash, Prefix: delegatedCredentialPrefix,
		Audience: "scion-hub", CeilingPermissionIDs: []string{"agent.read"},
		ExchangeAgentCredentialID: uuid.New().String(), ExpiresAt: time.Now().Add(30 * time.Minute),
	}
	require.NoError(t, s.CreateAgentDelegatedCredential(ctx, cred))
	out.credential = cred.ID
	return out
}
