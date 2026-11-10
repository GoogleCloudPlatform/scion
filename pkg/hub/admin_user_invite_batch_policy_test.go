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

package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Batch invite tests that need no store: the stop policy, the
// per-project membership state machine, the address checks and the body
// cap.

// inviteBatchPolicySources are the files SetMemberRoles draws its
// MembershipDecision values from (design §5.3).
var inviteBatchPolicySources = []string{
	"project_membership_set.go",
	"project_membership_service.go",
	"project_membership_role_decision.go",
	"members_group_principal.go",
}

// inviteBatchDynamicDenialCodes lists the DenialCode values the parser
// cannot resolve to a constant, keyed by "function expression", with every
// code each one can take.
var inviteBatchDynamicDenialCodes = map[string][]string{
	// code := role_assignment_forbidden, or target_role_protected for a
	// protected role.
	"checkBuiltInChangeGovernance code": {ErrCodeRoleAssignmentForbidden, ErrCodeTargetRoleProtected},
	"checkGovernance code":              {ErrCodeRoleAssignmentForbidden, ErrCodeTargetRoleProtected},
	// 404 -> not_found, 409 -> conflict, otherwise role_assignment_forbidden.
	"governanceDenial code": {ErrCodeNotFound, ErrCodeConflict, ErrCodeRoleAssignmentForbidden},
	// Returns only for ge.Code == constraint_admin_lockout.
	"constraintAdminLockoutDecision ge.Code": {ErrCodeConstraintAdminLockout},
}

// inviteBatchDenialCodeConstants resolves the ErrCode* identifiers used as
// DenialCode values.
var inviteBatchDenialCodeConstants = map[string]string{
	"ErrCodeMembershipCredentialInsufficient": ErrCodeMembershipCredentialInsufficient,
	"ErrCodeForbidden":                        ErrCodeForbidden,
	"ErrCodeRoleAssignmentForbidden":          ErrCodeRoleAssignmentForbidden,
	"ErrCodeTargetRoleProtected":              ErrCodeTargetRoleProtected,
	"ErrCodeInvalidRoleSet":                   ErrCodeInvalidRoleSet,
	"ErrCodeEmptyRoleSet":                     ErrCodeEmptyRoleSet,
	"ErrCodeMembershipChanged":                ErrCodeMembershipChanged,
	"ErrCodePrincipalIneligible":              ErrCodePrincipalIneligible,
	"ErrCodeInvalidRequest":                   ErrCodeInvalidRequest,
	"ErrCodeConflict":                         ErrCodeConflict,
	"ErrCodeInternalError":                    ErrCodeInternalError,
	"ErrCodeNotFound":                         ErrCodeNotFound,
	"ErrCodeLastOwner":                        ErrCodeLastOwner,
	"ErrCodeConstraintAdminLockout":           ErrCodeConstraintAdminLockout,
}

// collectSetMemberRolesDenialCodes parses the membership sources and
// returns every DenialCode a MembershipDecision literal in them can carry.
func collectSetMemberRolesDenialCodes(t *testing.T) map[string]bool {
	t.Helper()
	codes := map[string]bool{}
	fset := token.NewFileSet()
	for _, name := range inviteBatchPolicySources {
		f, err := parser.ParseFile(fset, name, nil, 0)
		require.NoError(t, err, "parse %s", name)
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if !ok {
					return true
				}
				if id, ok := lit.Type.(*ast.Ident); !ok || id.Name != "MembershipDecision" {
					return true
				}
				for _, elt := range lit.Elts {
					kv, ok := elt.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					if key, ok := kv.Key.(*ast.Ident); !ok || key.Name != "DenialCode" {
						continue
					}
					switch v := kv.Value.(type) {
					case *ast.BasicLit:
						s, err := strconv.Unquote(v.Value)
						require.NoError(t, err)
						codes[s] = true
						continue
					case *ast.Ident:
						if c, ok := inviteBatchDenialCodeConstants[v.Name]; ok {
							codes[c] = true
							continue
						}
					}
					expr := exprString(kv.Value)
					dyn, ok := inviteBatchDynamicDenialCodes[fn.Name.Name+" "+expr]
					require.True(t, ok, "%s: DenialCode %s in %s is not a known constant and not listed in inviteBatchDynamicDenialCodes; resolve it there, then classify its codes in batchMembershipStopPolicy and design §5.3",
						fset.Position(kv.Pos()), expr, fn.Name.Name)
					for _, c := range dyn {
						codes[c] = true
					}
				}
				return true
			})
		}
	}
	return codes
}

// exprString renders an identifier or selector expression.
func exprString(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return exprString(v.X) + "." + v.Sel.Name
	}
	return fmt.Sprintf("%T", e)
}

// TestInviteBatchStopPolicy_CoversAllSetMemberRolesCodes: every denial
// code SetMemberRoles can return is classified in
// batchMembershipStopPolicy, and each entry has the action design §5.3
// gives it.
func TestInviteBatchStopPolicy_CoversAllSetMemberRolesCodes(t *testing.T) {
	codes := collectSetMemberRolesDenialCodes(t)
	require.NotEmpty(t, codes)
	var missing []string
	for c := range codes {
		if _, ok := batchMembershipStopPolicy[c]; !ok {
			missing = append(missing, c)
		}
	}
	sort.Strings(missing)
	assert.Empty(t, missing, "SetMemberRoles can return these denial codes, which batchMembershipStopPolicy does not classify; add each to the table and to design §5.3 as stop or continue")

	want := map[string]bool{
		"credential_insufficient":   true,
		"forbidden":                 true,
		"role_assignment_forbidden": true,
		"target_role_protected":     true,
		"invalid_role_set":          true,
		"empty_role_set":            true,
		"membership_changed":        false,
		"principal_ineligible":      false,
		"invalid_request":           false,
		"conflict":                  false,
		"internal_error":            false,
		"not_found":                 true,
		"last_owner":                true,
		"constraint_admin_lockout":  true,
	}
	got := map[string]bool{}
	for c := range batchMembershipStopPolicy {
		got[c] = batchStopsProject(c)
	}
	assert.Equal(t, want, got, "each classified code has the design §5.3 action")
}

// TestInviteBatchStopPolicy_UnclassifiedCodeStops: a code the table does
// not list stops the project, through batchStopsProject and in a run.
func TestInviteBatchStopPolicy_UnclassifiedCodeStops(t *testing.T) {
	assert.True(t, batchStopsProject("some_new_code"), "an unclassified denial code must stop the project")
	assert.True(t, batchStopsProject(""), "an empty denial code must stop the project")

	setter := &scriptedMemberSetter{decisions: []*MembershipDecision{
		{DenialCode: "some_new_code", Reason: "new refusal", HTTPStatus: 403},
	}}
	st := &inviteBatchProjectState{}
	p := inviteBatchProject{ProjectID: "p1", RoleDefinitionIDs: []string{"r1"}}
	first := applyInviteBatchMembership(context.Background(), setter, nil, "u1", p, st)
	assert.Equal(t, InviteBatchMembership{ProjectID: "p1", Status: inviteBatchMembershipFailed, Code: "some_new_code"}, first)
	second := applyInviteBatchMembership(context.Background(), setter, nil, "u2", p, st)
	assert.Equal(t, InviteBatchMembership{ProjectID: "p1", Status: inviteBatchMembershipFailed, Code: "some_new_code"}, second,
		"the later address reports the same code")
	assert.Equal(t, 1, setter.calls, "an unclassified code stops the project: no further SetMemberRoles call")
}

// scriptedMemberSetter returns its decisions in order (nil = applied), and
// then applied for every further call. It counts the calls.
type scriptedMemberSetter struct {
	decisions []*MembershipDecision
	calls     int
	requests  []SetMemberRolesRequest
}

func (s *scriptedMemberSetter) SetMemberRoles(_ context.Context, req SetMemberRolesRequest) (*SetMemberRolesResult, *MembershipDecision) {
	s.calls++
	s.requests = append(s.requests, req)
	if len(s.decisions) > 0 {
		d := s.decisions[0]
		s.decisions = s.decisions[1:]
		if d != nil {
			return nil, d
		}
	}
	return &SetMemberRolesResult{Created: true, Changed: true}, nil
}

func internalErrorDecision() *MembershipDecision {
	return &MembershipDecision{DenialCode: ErrCodeInternalError, Reason: "store fault", HTTPStatus: 500}
}

// TestInviteBatch_InternalErrorSingleBlipContinues: one internal_error
// result does not stop the project; the next address is attempted and
// applied, and a success resets the run of failures.
func TestInviteBatch_InternalErrorSingleBlipContinues(t *testing.T) {
	decisions := []*MembershipDecision{internalErrorDecision(), nil}
	for i := 0; i < inviteBatchInternalErrorStopAfter-1; i++ {
		decisions = append(decisions, internalErrorDecision())
	}
	setter := &scriptedMemberSetter{decisions: decisions}
	st := &inviteBatchProjectState{}
	p := inviteBatchProject{ProjectID: "p1", RoleDefinitionIDs: []string{"r1"}}

	m := applyInviteBatchMembership(context.Background(), setter, nil, "u0", p, st)
	assert.Equal(t, inviteBatchMembershipFailed, m.Status)
	assert.Equal(t, ErrCodeInternalError, m.Code)
	m = applyInviteBatchMembership(context.Background(), setter, nil, "u1", p, st)
	assert.Equal(t, inviteBatchMembershipApplied, m.Status, "the address after a single internal_error is attempted")
	for i := 0; i < inviteBatchInternalErrorStopAfter-1; i++ {
		applyInviteBatchMembership(context.Background(), setter, nil, fmt.Sprintf("v%d", i), p, st)
	}
	assert.False(t, st.stopped, "a success resets the count: %d failures after it do not stop", inviteBatchInternalErrorStopAfter-1)
	m = applyInviteBatchMembership(context.Background(), setter, nil, "w", p, st)
	assert.Equal(t, inviteBatchMembershipApplied, m.Status)
	assert.Equal(t, inviteBatchInternalErrorStopAfter+2, setter.calls)
}

// TestInviteBatch_InternalErrorStreakStopsProject: after
// inviteBatchInternalErrorStopAfter internal_error results in a row on one
// project, the batch stops calling SetMemberRoles for it and reports the
// same code for the remaining addresses.
func TestInviteBatch_InternalErrorStreakStopsProject(t *testing.T) {
	var decisions []*MembershipDecision
	for i := 0; i < inviteBatchInternalErrorStopAfter; i++ {
		decisions = append(decisions, internalErrorDecision())
	}
	setter := &scriptedMemberSetter{decisions: decisions}
	st := &inviteBatchProjectState{}
	p := inviteBatchProject{ProjectID: "p1", RoleDefinitionIDs: []string{"r1"}}
	for i := 0; i < inviteBatchInternalErrorStopAfter; i++ {
		require.False(t, st.stopped, "not stopped before failure %d", i+1)
		m := applyInviteBatchMembership(context.Background(), setter, nil, fmt.Sprintf("u%d", i), p, st)
		assert.Equal(t, ErrCodeInternalError, m.Code)
	}
	assert.True(t, st.stopped, "%d internal_error results in a row stop the project", inviteBatchInternalErrorStopAfter)
	for i := 0; i < 3; i++ {
		m := applyInviteBatchMembership(context.Background(), setter, nil, fmt.Sprintf("later%d", i), p, st)
		assert.Equal(t, InviteBatchMembership{ProjectID: "p1", Status: inviteBatchMembershipFailed, Code: ErrCodeInternalError}, m)
	}
	assert.Equal(t, inviteBatchInternalErrorStopAfter, setter.calls, "no SetMemberRoles call after the stop")
}

// TestInviteBatch_MembershipClassification covers the membership
// outcomes of one call: already_member and conflict from a
// principal_roles_changed refusal, actor_authority_changed as a failed
// continue, and the request shape (ExpectedRoleIDs = []).
func TestInviteBatch_MembershipClassification(t *testing.T) {
	changed := func(cause string, current ...string) *MembershipDecision {
		return &MembershipDecision{DenialCode: ErrCodeMembershipChanged, HTTPStatus: 409,
			Details: map[string]interface{}{"cause": cause, "currentRoleDefinitionIds": current}}
	}
	p := inviteBatchProject{ProjectID: "p1", RoleDefinitionIDs: []string{"r1", "r2"}}
	setter := &scriptedMemberSetter{decisions: []*MembershipDecision{
		changed(causePrincipalRolesChanged, "r2", "r1"),
		changed(causePrincipalRolesChanged, "r3"),
		changed(causeActorAuthorityChanged),
		nil,
	}}
	st := &inviteBatchProjectState{}
	ctx := context.Background()
	assert.Equal(t, InviteBatchMembership{ProjectID: "p1", Status: inviteBatchMembershipAlreadyMember},
		applyInviteBatchMembership(ctx, setter, nil, "u1", p, st))
	assert.Equal(t, InviteBatchMembership{ProjectID: "p1", Status: inviteBatchMembershipConflict, Code: inviteBatchCodeMemberWithOtherRoles, CurrentRoleDefinitionIDs: []string{"r3"}},
		applyInviteBatchMembership(ctx, setter, nil, "u2", p, st))
	assert.Equal(t, InviteBatchMembership{ProjectID: "p1", Status: inviteBatchMembershipFailed, Code: ErrCodeMembershipChanged, Cause: causeActorAuthorityChanged},
		applyInviteBatchMembership(ctx, setter, nil, "u3", p, st))
	assert.Equal(t, InviteBatchMembership{ProjectID: "p1", Status: inviteBatchMembershipApplied},
		applyInviteBatchMembership(ctx, setter, nil, "u4", p, st))
	assert.False(t, st.stopped)
	assert.True(t, st.changed)
	require.Len(t, setter.requests, 4)
	for _, req := range setter.requests {
		assert.Equal(t, store.RoleBindingPrincipalUser, req.PrincipalType)
		require.NotNil(t, req.ExpectedRoleIDs, "the batch is additive only")
		assert.Empty(t, *req.ExpectedRoleIDs)
		assert.False(t, req.RemoveAll)
		assert.Equal(t, []string{"r1", "r2"}, req.DesiredRoleIDs)
	}

	// A context that has ended reports not_processed without a call.
	done, cancel := context.WithCancel(ctx)
	cancel()
	before := setter.calls
	assert.Equal(t, InviteBatchMembership{ProjectID: "p1", Status: inviteBatchMembershipFailed, Code: inviteBatchCodeNotProcessed},
		applyInviteBatchMembership(done, setter, nil, "u5", p, &inviteBatchProjectState{}))
	assert.Equal(t, before, setter.calls)
}

// TestInviteBatch_ReservedAddressFilterCoversGranteeRefusal ties the
// per-address check (design §3.4) to the project grantee check
// SetMemberRoles runs (refuseTestAdminMemberGrantee, through
// testAdminGranteeRefusal): every address the grantee check refuses by
// its shape is reported invalid before any write, so it can never reach
// SetMemberRoles, where its "forbidden" code would stop the whole project.
// Both sides use granteeAddressRefusal.
func TestInviteBatch_ReservedAddressFilterCoversGranteeRefusal(t *testing.T) {
	corpus := []string{
		"a@" + store.TestHubAdminEmailDomain,
		"B@" + strings.ToUpper(store.TestHubAdminEmailDomain),
		"c@" + store.TestSuperAdminEmailDomain,
		" d@" + store.TestSuperAdminEmailDomain + " ",
		"e@" + store.TestFixtureEmailDomain,
		"fed+abc@issuer.example.invalid",
		"FED+xyz@other.INVALID",
		"fed+q@issuer.invalid.",
		"plain@example.com",
		"fed@example.invalid",
		"x+fed@example.com",
	}
	srv := &Server{}
	sawRefused := 0
	for _, addr := range corpus {
		reason := testAdminGranteeRefusal(context.Background(), nil, nil, store.RoleBindingPrincipalUser, addr)
		shapeRefused := reason == testAdminGranteeReasonRefused || reason == federatedGranteeReasonRefused
		assert.Equal(t, shapeRefused, granteeAddressRefusal(addr) != "",
			"%q: testAdminGranteeRefusal refuses by shape exactly when granteeAddressRefusal does", addr)
		got := srv.validateInviteBatchAddresses("actor", []inviteBatchEntry{{raw: addr}})
		require.Len(t, got, 1)
		if shapeRefused {
			sawRefused++
			assert.Equal(t, inviteBatchStatusInvalid, got[0].result.Status,
				"%q: the grantee check refuses it, so the batch must report it invalid before any write", addr)
			assert.False(t, got[0].pending)
		}
	}
	assert.GreaterOrEqual(t, sawRefused, 6, "the corpus exercises the refused shapes")
}

// TestInviteBatch_AddressValidationAndDedup covers every pure address
// status and reason, the dedup with duplicateOf, and the input echo
// bound.
func TestInviteBatch_AddressValidationAndDedup(t *testing.T) {
	srv := &Server{proxyDrivers: newDriverPrincipalSet([]string{"driver@example.com"})}
	long := strings.Repeat("x", 400) + "@"
	entries := []inviteBatchEntry{
		{raw: "Alice@Example.com", note: "first"},                                    // 0 pending
		{raw: "not-an-email"},                                                        // 1 invalid_email
		{raw: "t@" + store.TestFixtureEmailDomain},                                   // 2 reserved
		{raw: "driver@example.com"},                                                  // 3 email_not_allowed (proxy driver)
		{raw: "fed+u@issuer.invalid"},                                                // 4 email_not_allowed (grantee shape)
		{raw: "bob@example.com", note: strings.Repeat("n", provisionNoteMaxRunes+1)}, // 5 note_too_long
		{raw: "carol@example.com", note: "bad\x07note"},                              // 6 note_invalid_characters
		{raw: " alice@EXAMPLE.com ", note: "other"},                                  // 7 duplicate of 0
		{raw: "bob@example.com", note: "fine\r\nlines"},                              // 8 pending (5 was invalid)
		{raw: long}, // 9 invalid_email, echo truncated
		{raw: ""},   // 10 invalid_email
	}
	got := srv.validateInviteBatchAddresses("actor", entries)
	require.Len(t, got, len(entries))
	type row struct{ status, reason string }
	want := []row{
		{"", ""},
		{inviteBatchStatusInvalid, inviteBatchReasonInvalidEmail},
		{inviteBatchStatusInvalid, provisionReasonReservedTestIdentity},
		{inviteBatchStatusInvalid, provisionReasonEmailNotAllowed},
		{inviteBatchStatusInvalid, provisionReasonEmailNotAllowed},
		{inviteBatchStatusInvalid, inviteBatchReasonNoteTooLong},
		{inviteBatchStatusInvalid, inviteBatchReasonNoteInvalidCharacters},
		{inviteBatchStatusDuplicate, inviteBatchReasonDuplicateInBatch},
		{"", ""},
		{inviteBatchStatusInvalid, inviteBatchReasonInvalidEmail},
		{inviteBatchStatusInvalid, inviteBatchReasonInvalidEmail},
	}
	for i, w := range want {
		assert.Equal(t, i, got[i].result.Index)
		assert.Equal(t, w.status, got[i].result.Status, "entry %d", i)
		assert.Equal(t, w.reason, got[i].result.Reason, "entry %d", i)
		assert.Equal(t, w.status == "", got[i].pending, "entry %d pending", i)
	}
	assert.Equal(t, "alice@example.com", got[0].email)
	require.NotNil(t, got[0].note)
	assert.Equal(t, "first", *got[0].note)
	require.NotNil(t, got[7].result.DuplicateOf)
	assert.Equal(t, 0, *got[7].result.DuplicateOf)
	assert.Equal(t, "alice@example.com", got[7].result.Email)
	assert.Equal(t, "not-an-email", got[1].result.Input)
	assert.Len(t, got[9].result.Input, inviteBulkInputEchoMaxBytes)
	assert.Equal(t, "bob@example.com", got[8].email)

	// The echo is cut on a rune boundary.
	multi := strings.Repeat("é", 200) // 400 bytes
	cut := truncateInviteInput(multi)
	assert.LessOrEqual(t, len(cut), inviteBulkInputEchoMaxBytes)
	assert.True(t, strings.HasPrefix(multi, cut))
	assert.Equal(t, 0, len(cut)%2, "no half rune")
}

// TestAdminUserInviteBulk_BodyCapFitsWorstCase computes the largest valid
// JSON request as encoding/json writes it and checks that
// inviteBulkMaxBodyBytes admits it: 1000 entries, each a 254-byte address
// of characters encoding/json escapes to 6 bytes and a 500-rune note of
// such characters, plus 20 projects of 10 role IDs.
func TestAdminUserInviteBulk_BodyCapFitsWorstCase(t *testing.T) {
	email := strings.Repeat("&", 254-len("@example.com")) + "@example.com"
	require.Len(t, email, 254)
	note := strings.Repeat("<", provisionNoteMaxRunes)
	req := UserInviteBulkRequest{}
	for i := 0; i < inviteBulkMaxEmails; i++ {
		req.Emails = append(req.Emails, UserInviteRequest{Email: email, Note: note})
	}
	for i := 0; i < inviteBulkMaxProjects; i++ {
		p := UserInviteBulkProject{ProjectID: fmt.Sprintf("%08d-0000-0000-0000-000000000000", i)}
		for j := 0; j < inviteBulkMaxRolesPerProject; j++ {
			p.RoleDefinitionIDs = append(p.RoleDefinitionIDs, fmt.Sprintf("%08d-0000-0000-0000-%012d", i, j))
		}
		req.Projects = append(req.Projects, p)
	}
	b, err := json.Marshal(req)
	require.NoError(t, err)
	t.Logf("worst-case valid body: %d bytes; cap %d", len(b), inviteBulkMaxBodyBytes)
	assert.LessOrEqual(t, len(b), inviteBulkMaxBodyBytes, "the cap admits the worst valid request")
	assert.Greater(t, len(b), inviteBulkMaxBodyBytes*3/4, "the cap stays close to the worst case")
}
