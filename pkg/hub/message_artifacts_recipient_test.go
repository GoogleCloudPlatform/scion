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

// Tests for the send-time recipient warning (ptone/scion#4575): the sender
// is told, as a count, when attached artifact references may not be
// readable by the message's direct recipient. The check is warning-only.

import (
	"context"
	"fmt"
	"go/ast"
	"go/token"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recipientWarningSetup is paritySetup with artifacts on, a token-backed
// sender, and two artifacts the sender owns: one homed in its project and
// one homed in another project (readable by the sender as owner only).
func recipientWarningSetup(t *testing.T) (srv *Server, s store.Store, st artifacts.Store, project *store.Project,
	sender, target *store.Agent, senderCtx context.Context, own, elsewhere string) {
	t.Helper()
	srv, s, project, sender, target, _, _, _ = paritySetup(t)
	st, _ = enableArtifactsForTest(t, srv)
	senderCtx = requestAuthCtx(context.Background(), tokenBackedSender(t, s, sender))
	own = seedMessageArtifact(t, st, project.ID, artifacts.PrincipalKindAgent, sender.ID, "Mine")
	elsewhere = seedMessageArtifact(t, st, tid("recipient-other-project"), artifacts.PrincipalKindAgent, sender.ID, "Mine, elsewhere")
	return
}

// readyRecipientAgent gives agent a the baseline role and a recorded
// delegation edge, so its next token would carry project:artifact:read.
func readyRecipientAgent(t *testing.T, s store.Store, a *store.Agent) {
	t.Helper()
	a.AppliedConfig = &store.AgentAppliedConfig{AgentRole: string(AgentRoleBaseline)}
	require.NoError(t, s.UpdateAgent(context.Background(), a))
	tokenBackedSender(t, s, a) // records the edge; the identity is not used
}

func refsMD(refs ...artifacts.MessageRef) map[string]string {
	return map[string]string{artifacts.MessageMetadataKey: refsValue(refs...)}
}

// TestArtifactRecipientWarning_AgentRecipient: an agent recipient that can
// read the home-project artifact but not the one homed elsewhere gets a
// count of 1; both are still admitted; the warning names no reference.
// A group member or no direct recipient gets no recipient check.
func TestArtifactRecipientWarning_AgentRecipient(t *testing.T) {
	srv, s, _, _, _, target, ctx, own, elsewhere := recipientWarningSetup(t)
	readyRecipientAgent(t, s, target)
	md := refsMD(artifacts.MessageRef{ArtifactID: own}, artifacts.MessageRef{ArtifactID: elsewhere})

	_, admitted, warning := srv.admitMessageArtifacts(ctx, md, directAgentRecipient(target, ""))
	assert.Len(t, admitted, 2, "the recipient check never changes what is admitted")
	assert.Equal(t, fmt.Sprintf(artifactRefsRecipientWarning, 1, target.Slug), warning)
	assert.NotContains(t, warning, own)
	assert.NotContains(t, warning, elsewhere)

	// A member of a group send, and a send with no direct recipient: no check.
	_, admitted, warning = srv.admitMessageArtifacts(ctx, md, directAgentRecipient(target, "group-1"))
	assert.Len(t, admitted, 2)
	assert.Empty(t, warning)
	_, _, warning = srv.admitMessageArtifacts(ctx, md, artifactRecipient{})
	assert.Empty(t, warning)

	// Only the home-project artifact: nothing to warn about.
	_, _, warning = srv.admitMessageArtifacts(ctx, refsMD(artifacts.MessageRef{ArtifactID: own}), directAgentRecipient(target, ""))
	assert.Empty(t, warning)
}

// TestArtifactRecipientWarning_AgentWithoutArtifactScope is the case that
// prompted ptone/scion#4575: the recipient agent's token would not carry
// project:artifact:read (role none), so even a home-project artifact it
// would otherwise be allowed to read is counted.
func TestArtifactRecipientWarning_AgentWithoutArtifactScope(t *testing.T) {
	srv, _, _, _, _, target, ctx, own, elsewhere := recipientWarningSetup(t)
	require.Nil(t, target.AppliedConfig, "paritySetup agents carry no role: none")
	_, admitted, warning := srv.admitMessageArtifacts(ctx,
		refsMD(artifacts.MessageRef{ArtifactID: own}, artifacts.MessageRef{ArtifactID: elsewhere}),
		directAgentRecipient(target, ""))
	assert.Len(t, admitted, 2)
	assert.Equal(t, fmt.Sprintf(artifactRefsRecipientWarning, 2, target.Slug), warning)
}

// TestArtifactRecipientWarning_PrincipalGrant: a read grant to the
// recipient on the artifact homed elsewhere clears the warning.
func TestArtifactRecipientWarning_PrincipalGrant(t *testing.T) {
	srv, s, st, _, _, target, ctx, own, elsewhere := recipientWarningSetup(t)
	readyRecipientAgent(t, s, target)
	_, err := st.PutGrant(context.Background(), &artifacts.Grant{ID: uuid.NewString(), ArtifactID: elsewhere,
		SubjectKind: artifacts.SubjectPrincipal, SubjectRef: artifacts.PrincipalRef(artifacts.PrincipalKindAgent, target.ID),
		Permission: artifacts.GrantRead, CreatedAt: time.Now()}, 100, false)
	require.NoError(t, err)
	_, _, warning := srv.admitMessageArtifacts(ctx,
		refsMD(artifacts.MessageRef{ArtifactID: own}, artifacts.MessageRef{ArtifactID: elsewhere}),
		directAgentRecipient(target, ""))
	assert.Empty(t, warning)
}

// TestArtifactRecipientWarning_UserRecipient: a project member can read the
// home-project artifact, a hub user outside the project cannot, and an
// inactive member counts as unable to read.
func TestArtifactRecipientWarning_UserRecipient(t *testing.T) {
	srv, s, _, project, _, _, _, own, _ := recipientWarningSetup(t)
	ctx := context.Background()
	member := tid("recipient-member")
	createTestUserWithProjectRole(t, s, member, "recipient-member@test.example", project.ID, store.ProjectRoleMember)
	stranger := hubMemberUser(t, s, tid("recipient-stranger"))
	refs := []artifacts.MessageRef{{ArtifactID: own}}
	svc := srv.artifactRefResolver()

	n, err := svc.RecipientMayNotRead(ctx, refs, artifacts.PrincipalKindUser, member)
	require.NoError(t, err)
	assert.Zero(t, n, "project member")
	n, err = svc.RecipientMayNotRead(ctx, refs, artifacts.PrincipalKindUser, stranger.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "hub user outside the project")
	n, err = svc.RecipientMayNotRead(ctx, refs, artifacts.PrincipalKindUser, tid("recipient-nobody"))
	require.NoError(t, err)
	assert.Equal(t, 1, n, "unknown user")

	u, err := s.GetUser(ctx, member)
	require.NoError(t, err)
	u.Status = "suspended"
	require.NoError(t, s.UpdateUser(ctx, u))
	n, err = svc.RecipientMayNotRead(ctx, refs, artifacts.PrincipalKindUser, member)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "inactive member")
}

// TestArtifactRecipientWarning_FailureIsSilent: when the recipient check
// cannot complete (here, a grant read fails), the sender gets no recipient
// warning and admission is unchanged: a failure is not evidence.
func TestArtifactRecipientWarning_FailureIsSilent(t *testing.T) {
	srv, s, st, _, _, target, ctx, own, elsewhere := recipientWarningSetup(t)
	readyRecipientAgent(t, s, target)
	srv.SetArtifactStore(failGrantsArtifactStore{st})
	_, admitted, warning := srv.admitMessageArtifacts(ctx,
		refsMD(artifacts.MessageRef{ArtifactID: own}, artifacts.MessageRef{ArtifactID: elsewhere}),
		directAgentRecipient(target, ""))
	assert.Len(t, admitted, 2, "the sender owns both and needs no grants")
	assert.Empty(t, warning)
}

// TestArtifactRecipientWarning_ExecuteAgentDM: the warning reaches the
// sender through the agent DM operation's ArtifactWarning.
func TestArtifactRecipientWarning_ExecuteAgentDM(t *testing.T) {
	srv, _, _, project, sender, target, ctx, _, elsewhere := recipientWarningSetup(t)
	res, dmErr := srv.ExecuteAgentDM(ctx, &AgentDMInput{
		SenderAgent:    sender,
		SenderIdentity: GetIdentityFromContext(ctx),
		TargetAgent:    target,
		Msg:            "see this",
		Type:           messages.TypeInstruction,
		Metadata:       refsMD(artifacts.MessageRef{ArtifactID: elsewhere}),
		ProjectID:      project.ID,
	})
	require.Nil(t, dmErr)
	assert.Equal(t, fmt.Sprintf(artifactRefsRecipientWarning, 1, target.Slug), res.ArtifactWarning)
}

// TestDirectUserRecipient: an outbound message is checked against a user
// only when the resolved direct conversation's key names the sending agent
// and that user, never from the request's recipient fields alone.
func TestDirectUserRecipient(t *testing.T) {
	agentID, userID := uuid.NewString(), uuid.NewString()
	key, err := messages.DMConversationKey("agent", agentID, "user", userID)
	require.NoError(t, err)
	direct := &messaging.ConversationResult{Kind: "direct", ExternalRef: key}
	base := routingResult{Recipient: "user:pat@example.com", RecipientID: userID, ConvResult: direct}

	r := base
	assert.Equal(t, artifactRecipient{kind: artifacts.PrincipalKindUser, id: userID, label: "pat@example.com"}, directUserRecipient(&r, agentID))

	cases := map[string]func(r *routingResult) string{
		"recipient id not in the key": func(r *routingResult) string { r.RecipientID = uuid.NewString(); return agentID },
		"sender not in the key":       func(r *routingResult) string { return uuid.NewString() },
		"group id":                    func(r *routingResult) string { r.GroupID = "g"; return agentID },
		"group recipients":            func(r *routingResult) string { r.Recipients = "{}"; return agentID },
		"group conversation": func(r *routingResult) string {
			r.ConvResult = &messaging.ConversationResult{Kind: "group", ExternalRef: key}
			return agentID
		},
		"no conversation": func(r *routingResult) string { r.ConvResult = nil; return agentID },
		"not a user":      func(r *routingResult) string { r.Recipient = "agent:x"; return agentID },
	}
	for name, mutate := range cases {
		r := base
		sender := mutate(&r)
		if name == "not a user" {
			// The key still names a user; the label falls back but the
			// user is the key's.
			assert.Equal(t, userID, directUserRecipient(&r, sender).id, name)
			continue
		}
		assert.Equal(t, artifactRecipient{}, directUserRecipient(&r, sender), name)
	}
	agentKey, err := messages.DMConversationKey("agent", agentID, "agent", userID)
	require.NoError(t, err)
	r = base
	r.ConvResult = &messaging.ConversationResult{Kind: "direct", ExternalRef: agentKey}
	assert.Equal(t, artifactRecipient{}, directUserRecipient(&r, agentID), "agent peer")
}

// recipientCheckSymbols are the warning-only recipient check entry points,
// each with the only pkg/hub functions that may reference it.
var recipientCheckSymbols = map[string]map[stripSiteKey]bool{
	"RecipientMayNotRead": {{"message_artifacts.go", "artifactRecipientWarning"}: true},
	"recipientIdentity": {
		{"artifacts_recipient.go", "RecipientPermits"}:   true,
		{"artifacts_recipient.go", "RecipientAuthorize"}: true,
	},
	"artifactRecipientWarning": {{"message_artifacts.go", "admitMessageArtifacts"}: true},
}

// TestArtifactRecipientCheckIsWarningOnly pins that the recipient check
// only ever feeds the sender's warning: RecipientMayNotRead is called only
// by artifactRecipientWarning, whose result admitMessageArtifacts only
// appends to the warning; recipientIdentity only by the two
// RecipientAuthority methods; and no other pkg/hub code calls
// RecipientPermits or RecipientAuthorize (the artifact service reaches
// them only through RecipientMayNotRead).
func TestArtifactRecipientCheckIsWarningOnly(t *testing.T) {
	seen := map[string]int{}
	forEachHubFile(t, func(name string, _ *token.FileSet, f *ast.File) {
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sym := extractCallSymbol(call)
				if sym == "RecipientPermits" || sym == "RecipientAuthorize" {
					t.Errorf("%s:%s calls %s; only the artifact service may", name, fd.Name.Name, sym)
				}
				allowed, pinned := recipientCheckSymbols[sym]
				if !pinned {
					return true
				}
				seen[sym]++
				if !allowed[stripSiteKey{name, fd.Name.Name}] {
					t.Errorf("%s:%s calls %s; it is warning-only and may be called only from %v", name, fd.Name.Name, sym, allowed)
				}
				return true
			})
		}
	})
	for sym := range recipientCheckSymbols {
		if seen[sym] == 0 {
			t.Errorf("no call to %s found: scanner or table broken", sym)
		}
	}

	// admitMessageArtifacts uses artifactRecipientWarning's result only to
	// extend the warning it returns.
	fd := hubFuncDecl(t, "message_artifacts.go", "admitMessageArtifacts")
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Rhs) != 1 {
			return true
		}
		if call, ok := as.Rhs[0].(*ast.CallExpr); ok && extractCallSymbol(call) == "artifactRecipientWarning" {
			for _, lhs := range as.Lhs {
				id, ok := lhs.(*ast.Ident)
				if !ok {
					t.Errorf("artifactRecipientWarning result assigned to a non-identifier")
					continue
				}
				checkOnlyFeedsWarning(t, fd, id.Name)
			}
		}
		return true
	})
}

// checkOnlyFeedsWarning fails unless every use of variable name in fd is a
// comparison with "" or an operand of an assignment to warning.
func checkOnlyFeedsWarning(t *testing.T, fd *ast.FuncDecl, name string) {
	t.Helper()
	allowed := map[*ast.Ident]bool{}
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.BinaryExpr:
			if id, ok := v.X.(*ast.Ident); ok && id.Name == name {
				if lit, ok := v.Y.(*ast.BasicLit); ok && lit.Value == `""` {
					allowed[id] = true
				}
			}
		case *ast.AssignStmt:
			if len(v.Lhs) == 1 {
				if lhs, ok := v.Lhs[0].(*ast.Ident); ok && lhs.Name == "warning" {
					for _, rhs := range v.Rhs {
						if id, ok := rhs.(*ast.Ident); ok && id.Name == name {
							allowed[id] = true
						}
					}
				}
			}
			for _, lhs := range v.Lhs {
				if id, ok := lhs.(*ast.Ident); ok && id.Name == name {
					allowed[id] = true // its own definition
				}
			}
		}
		return true
	})
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Name == name && !allowed[id] {
			t.Errorf("admitMessageArtifacts uses the recipient warning %q other than to extend warning", name)
		}
		return true
	})
}

// TestRecipientIdentityNeverEntersAContext pins that the identity built for
// a recipient check stays a policy subject: nothing in
// artifacts_recipient.go puts any identity into a context, so the artifact
// service, which learns its caller only from ctx, can never take a
// recipient for the caller.
func TestRecipientIdentityNeverEntersAContext(t *testing.T) {
	forbidden := map[string]bool{
		"WithValue":                    true,
		"contextWithIdentity":          true,
		"contextWithCredentialContext": true,
		"WithIdentity":                 true,
		"withIdentity":                 true,
		"ContextWithIdentity":          true,
	}
	calls := 0
	forEachHubFile(t, func(name string, _ *token.FileSet, f *ast.File) {
		if name != "artifacts_recipient.go" {
			return
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			calls++
			if sym := extractCallSymbol(call); forbidden[sym] {
				t.Errorf("artifacts_recipient.go calls %s: a recipient identity must never enter a context", sym)
			}
			return true
		})
	})
	if calls == 0 {
		t.Fatal("artifacts_recipient.go not found or has no calls: scanner broken")
	}
}

// hubFuncDecl returns the declaration of function fn in pkg/hub file.
func hubFuncDecl(t *testing.T, file, fn string) *ast.FuncDecl {
	t.Helper()
	var out *ast.FuncDecl
	forEachHubFile(t, func(name string, _ *token.FileSet, f *ast.File) {
		if name != file {
			return
		}
		for _, decl := range f.Decls {
			if fd, ok := decl.(*ast.FuncDecl); ok && fd.Name.Name == fn && fd.Body != nil {
				out = fd
			}
		}
	})
	if out == nil {
		t.Fatalf("%s:%s not found", file, fn)
	}
	return out
}
