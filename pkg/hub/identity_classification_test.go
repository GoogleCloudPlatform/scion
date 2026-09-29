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
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// Test-only identity fakes for classification tests (ptone/scion#2123)
// =============================================================================

// unclassifiedMockIdentity is an Identity of a type that principalContextForIdentity,
// credentialContextForIdentity and AncestryIsHubAttested have never heard of. It
// does not implement UserIdentity, AgentIdentity or FederatedIdentity, and it does
// not opt into localAncestryProvenanceIdentity. Every classifier must fail closed
// on it.
type unclassifiedMockIdentity struct {
	id string
}

func (m *unclassifiedMockIdentity) ID() string   { return m.id }
func (m *unclassifiedMockIdentity) Type() string { return "unclassified_mock_type" }

// unattestedMockAgentIdentity implements AgentIdentity in full, including an
// ancestry chain that names a real user, but deliberately does NOT implement
// localAncestryProvenanceIdentity. It exists to prove that ancestry attestation
// requires the explicit marker rather than following from Type() == "agent" or
// from carrying a plausible-looking ancestry chain.
type unattestedMockAgentIdentity struct {
	id        string
	projectID string
	ancestry  []string
}

func (m *unattestedMockAgentIdentity) ID() string                    { return m.id }
func (m *unattestedMockAgentIdentity) Type() string                  { return "agent" }
func (m *unattestedMockAgentIdentity) ProjectID() string             { return m.projectID }
func (m *unattestedMockAgentIdentity) Scopes() []AgentTokenScope     { return nil }
func (m *unattestedMockAgentIdentity) HasScope(AgentTokenScope) bool { return false }
func (m *unattestedMockAgentIdentity) Ancestry() []string            { return m.ancestry }
func (m *unattestedMockAgentIdentity) TokenID() string               { return "" }
func (m *unattestedMockAgentIdentity) OriginUserID() string {
	if len(m.ancestry) > 0 {
		return m.ancestry[0]
	}
	return ""
}

// =============================================================================
// T-ID-1: source-scan drift guard
// =============================================================================

// identityInventoryExpectation is the D.1 inventory of every non-test pkg/hub
// concrete type that implements Identity (ruling
// D/notes/ruling-identity-fail-closed.md), keyed by the type's declared name.
// attested records AncestryIsHubAttested's planned outcome. A new identity
// type added to non-test pkg/hub source without a row here — or without a
// localAncestryProvenance() implementation matching its row — fails
// TestIdentityClassification_EveryTypeHasExplicitOutcome/SourceScan. That is
// the point: it forces classification to be a deliberate, reviewed edit.
var identityInventoryExpectation = map[string]bool{
	"AuthenticatedUser":        true,
	"ScopedUserIdentity":       true,
	"DevUser":                  true,
	"agentIdentityWrapper":     true,
	"storedAgentIdentity":      true,
	"peerAgentIdentity":        true,
	"explainAgentIdentity":     true,
	"brokerIdentityImpl":       false,
	"FederatedUserIdentity":    false,
	"FederatedAgentIdentity":   false,
	"FederatedServiceIdentity": false,
}

// identitySourceInventory is a structural (AST-level) description of the
// identity-relevant types declared in a set of parsed Go files.
type identitySourceInventory struct {
	// methods maps type name -> set of method names declared with that type
	// (or *type) as receiver.
	methods map[string]map[string]bool
	// embeds maps type name -> the unqualified type names of its embedded
	// (anonymous) struct fields declared in the same package.
	embeds map[string][]string
}

// exprTypeName returns the unqualified local type name referenced by expr,
// unwrapping a leading pointer. It returns "" for anything that isn't a
// plain (possibly pointer) identifier local to the package, such as a
// selector expression naming a type in another package.
func exprTypeName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return exprTypeName(t.X)
	case *ast.Ident:
		return t.Name
	default:
		return ""
	}
}

// scanIdentitySource parses every non-test *.go file in the current
// directory (package hub's own directory, since tests run with that as the
// working directory) and extracts method and embedded-field information.
func scanIdentitySource(t *testing.T) identitySourceInventory {
	t.Helper()

	files, err := filepath.Glob("*.go")
	require.NoError(t, err)

	inv := identitySourceInventory{
		methods: map[string]map[string]bool{},
		embeds:  map[string][]string{},
	}

	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		require.NoError(t, err, "parsing %s", name)

		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Recv == nil || len(d.Recv.List) != 1 {
					continue
				}
				recvType := exprTypeName(d.Recv.List[0].Type)
				if recvType == "" {
					continue
				}
				if inv.methods[recvType] == nil {
					inv.methods[recvType] = map[string]bool{}
				}
				inv.methods[recvType][d.Name.Name] = true
			case *ast.GenDecl:
				if d.Tok != token.TYPE {
					continue
				}
				for _, spec := range d.Specs {
					ts, ok := spec.(*ast.TypeSpec)
					if !ok {
						continue
					}
					st, ok := ts.Type.(*ast.StructType)
					if !ok || st.Fields == nil {
						continue
					}
					for _, field := range st.Fields.List {
						if len(field.Names) != 0 {
							continue // not an embedded field
						}
						if embedded := exprTypeName(field.Type); embedded != "" {
							inv.embeds[ts.Name.Name] = append(inv.embeds[ts.Name.Name], embedded)
						}
					}
				}
			}
		}
	}
	return inv
}

// identityImplementingTypes returns every type name that satisfies the
// Identity interface, either by declaring ID() and Type() directly, or by
// embedding an interface (UserIdentity/AgentIdentity) that itself extends
// Identity and is declared in this package.
func (inv identitySourceInventory) identityImplementingTypes() map[string]bool {
	result := map[string]bool{}
	for typeName, methods := range inv.methods {
		if methods["ID"] && methods["Type"] {
			result[typeName] = true
		}
	}
	for typeName, embedded := range inv.embeds {
		for _, e := range embedded {
			if e == "UserIdentity" || e == "AgentIdentity" || e == "Identity" {
				result[typeName] = true
			}
		}
	}
	return result
}

func (inv identitySourceInventory) hasMarker(typeName string) bool {
	return inv.methods[typeName]["localAncestryProvenance"]
}

func (inv identitySourceInventory) isFederated(typeName string) bool {
	return inv.methods[typeName]["IssuerURL"]
}

// TestIdentityClassification_EveryTypeHasExplicitOutcome is the D.1 T-ID-1/T-ID-2
// pair: a source-level drift guard (subtest SourceScan) that fails if a new
// non-test pkg/hub Identity type appears without a reviewed row in
// identityInventoryExpectation or without localAncestryProvenance matching
// that row, plus a runtime table (subtest ClassifierOutcomes) that exercises
// principalContextForIdentity, credentialContextForIdentity and
// AncestryIsHubAttested against a constructed instance of every inventory
// row, plus nil and an unrecognized concrete type.
func TestIdentityClassification_EveryTypeHasExplicitOutcome(t *testing.T) {
	t.Run("SourceScan", func(t *testing.T) {
		inv := scanIdentitySource(t)
		found := inv.identityImplementingTypes()

		foundNames := make([]string, 0, len(found))
		for name := range found {
			foundNames = append(foundNames, name)
		}
		wantNames := make([]string, 0, len(identityInventoryExpectation))
		for name := range identityInventoryExpectation {
			wantNames = append(wantNames, name)
		}
		assert.ElementsMatch(t, wantNames, foundNames,
			"a new non-test pkg/hub Identity type (or a removed one) needs a reviewed "+
				"row in identityInventoryExpectation, per D/notes/ruling-identity-fail-closed.md")

		for name, wantAttested := range identityInventoryExpectation {
			if !found[name] {
				continue // already reported by the ElementsMatch failure above
			}
			federated := inv.isFederated(name)
			marker := inv.hasMarker(name)
			gotAttested := marker && !federated
			assert.Equalf(t, wantAttested, gotAttested,
				"%s: AncestryIsHubAttested outcome must match the inventory (marker=%v, federated=%v)",
				name, marker, federated)
			if federated {
				assert.Falsef(t, marker, "%s: a FederatedIdentity type must never carry localAncestryProvenance", name)
			}
		}
	})

	t.Run("ClassifierOutcomes", func(t *testing.T) {
		agentClaims := &agentIdentityWrapper{&AgentTokenClaims{Claims: jwt.Claims{Subject: "agent-1", ID: "jti-1"}}}

		cases := []struct {
			name               string
			identity           Identity
			wantPrincipalKind  PrincipalKind
			wantCredentialKind CredentialKind
			wantAttested       bool
		}{
			{
				name:               "AuthenticatedUser",
				identity:           NewAuthenticatedUser(tid("classify-user"), "u@example.com", "U", "member", "cli"),
				wantPrincipalKind:  PrincipalKindUser,
				wantCredentialKind: CredentialKindInteractive,
				wantAttested:       true,
			},
			{
				name: "ScopedUserIdentity",
				identity: NewScopedUserIdentity(
					NewAuthenticatedUser(tid("classify-scoped-user"), "u@example.com", "U", "member", "cli"),
					tid("classify-project"), []string{"agent:read"}),
				wantPrincipalKind:  PrincipalKindUser,
				wantCredentialKind: CredentialKindUAT,
				wantAttested:       true,
			},
			{
				name:               "DevUser",
				identity:           NewDevUser(DevUserConfig{Username: "dev", DisplayName: "Dev", Email: "dev@localhost"}),
				wantPrincipalKind:  PrincipalKindDev,
				wantCredentialKind: CredentialKindDev,
				wantAttested:       true,
			},
			{
				name:               "agentIdentityWrapper",
				identity:           agentClaims,
				wantPrincipalKind:  PrincipalKindAgent,
				wantCredentialKind: CredentialKindAgentJWT,
				wantAttested:       true,
			},
			{
				name:               "storedAgentIdentity",
				identity:           &storedAgentIdentity{agent: &store.Agent{ID: tid("classify-stored-agent"), ProjectID: tid("classify-project"), Ancestry: []string{tid("classify-user")}}},
				wantPrincipalKind:  PrincipalKindAgent,
				wantCredentialKind: CredentialKindAgentJWT,
				wantAttested:       true,
			},
			{
				name:               "peerAgentIdentity",
				identity:           &peerAgentIdentity{agent: &store.Agent{ID: tid("classify-peer-agent"), ProjectID: tid("classify-project"), Ancestry: []string{tid("classify-user")}}},
				wantPrincipalKind:  PrincipalKindAgent,
				wantCredentialKind: CredentialKindAgentJWT,
				wantAttested:       true,
			},
			{
				name:               "explainAgentIdentity",
				identity:           newAgentIdentityFromStore(&store.Agent{ID: tid("classify-explain-agent"), ProjectID: tid("classify-project"), Ancestry: []string{tid("classify-user")}}),
				wantPrincipalKind:  PrincipalKindAgent,
				wantCredentialKind: CredentialKindAgentJWT,
				wantAttested:       true,
			},
			{
				name:               "brokerIdentityImpl",
				identity:           NewBrokerIdentity(tid("classify-broker")),
				wantPrincipalKind:  PrincipalKindBroker,
				wantCredentialKind: CredentialKindBroker,
				wantAttested:       false,
			},
			{
				name:               "FederatedUserIdentity",
				identity:           NewFederatedUserIdentity("https://issuer.example", "sub", "u@example.com", "U", "member", nil),
				wantPrincipalKind:  PrincipalKindFederatedUser,
				wantCredentialKind: CredentialKindFederation,
				wantAttested:       false,
			},
			{
				name:               "FederatedAgentIdentity",
				identity:           NewFederatedAgentIdentity("https://issuer.example", "remote-agent", "remote-project", "Remote Agent", tid("classify-user"), []string{tid("classify-user")}, nil),
				wantPrincipalKind:  PrincipalKindFederatedAgent,
				wantCredentialKind: CredentialKindFederation,
				wantAttested:       false,
			},
			{
				name:               "FederatedServiceIdentity",
				identity:           NewFederatedServiceIdentity("https://issuer.example", "sub", "sa@example.com", nil),
				wantPrincipalKind:  PrincipalKindFederatedService,
				wantCredentialKind: CredentialKindFederation,
				wantAttested:       false,
			},
			{
				name:               "nil identity",
				identity:           nil,
				wantPrincipalKind:  "",
				wantCredentialKind: "",
				wantAttested:       false,
			},
			{
				name:               "unrecognized concrete type",
				identity:           &unclassifiedMockIdentity{id: tid("classify-unknown")},
				wantPrincipalKind:  "",
				wantCredentialKind: "",
				wantAttested:       false,
			},
			{
				name:               "agent-shaped type without the ancestry marker",
				identity:           &unattestedMockAgentIdentity{id: tid("classify-unattested-agent"), projectID: tid("classify-project"), ancestry: []string{tid("classify-user")}},
				wantPrincipalKind:  PrincipalKindAgent,
				wantCredentialKind: CredentialKindAgentJWT,
				wantAttested:       false,
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				principal := principalContextForIdentity(tc.identity)
				assert.Equal(t, tc.wantPrincipalKind, principal.Kind, "principal kind")

				credential := credentialContextForIdentity(tc.identity)
				assert.Equal(t, tc.wantCredentialKind, credential.Kind, "credential kind")

				assert.Equal(t, tc.wantAttested, AncestryIsHubAttested(tc.identity), "ancestry attestation")
			})
		}
	})
}

// =============================================================================
// T-ID-4/T-ID-5: Decide fail-closed classification
// =============================================================================

// countingAuditEmitter counts DecisionAuditEmitter calls without touching a
// store, so Decide's fail-closed entry paths can be exercised without a
// backing AuthzService.store.
type countingAuditEmitter struct {
	calls int
}

func (e *countingAuditEmitter) EmitDecisionAudit(context.Context, *store.DecisionAuditRecord) {
	e.calls++
}

// TestDecide_UnrecognizedDerivedPrincipalKindDenied pins T-ID-4: Decide denies
// a nil identity, an identity of an unrecognized concrete type, and an
// agent-shaped identity that hasn't opted into ancestry attestation carries no
// special exemption here — all deny with "unrecognized principal kind" because
// classification, not attestation, gates entry. Exactly one audit record is
// emitted per call.
func TestDecide_UnrecognizedDerivedPrincipalKindDenied(t *testing.T) {
	cases := []struct {
		name     string
		identity Identity
	}{
		{"nil identity", nil},
		{"unrecognized concrete type", &unclassifiedMockIdentity{id: tid("decide-unknown")}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			emitter := &countingAuditEmitter{}
			authz := &AuthzService{decisionAuditEmitter: emitter}

			decision := authz.Decide(context.Background(), AuthzRequest{
				Principal: PrincipalContext{Identity: tc.identity},
				Resource:  Resource{Type: "agent", ID: tid("decide-target")},
				Action:    ActionRead,
			})

			assert.False(t, decision.Allowed)
			assert.Equal(t, "unrecognized principal kind", decision.Reason)
			assert.Equal(t, 1, emitter.calls, "exactly one audit record must be emitted")
		})
	}
}

// TestDecide_SuppliedKindCannotReclassifyIdentity pins T-ID-5. A supplied
// Principal.Kind that does not match the identity's own classification is
// always denied: an unrecognized identity can never be upgraded into a
// recognized one by supplied context. A supplied Credential.Kind is denied
// only in the one direction that would discard the caveats it is supposed
// to enforce: a *ScopedUserIdentity (whose own classification is UAT)
// cannot be presented with a different supplied credential kind, because
// both the step 1 UAT gate and the kernel's credential_scope restriction
// apply their UAT-specific caveats only when Credential.Kind ==
// CredentialKindUAT. Layering a narrower CredentialContext onto some other
// recognized identity — as other pre-existing tests in this package do, to
// drive the credential_scope restriction without constructing a full
// ScopedUserIdentity — is not a mismatch this rule rejects, because it can
// only narrow authority.
func TestDecide_SuppliedKindCannotReclassifyIdentity(t *testing.T) {
	unknown := &unclassifiedMockIdentity{id: tid("mismatch-unknown")}
	scopedUAT := NewScopedUserIdentity(
		NewAuthenticatedUser(tid("mismatch-uat-user"), "u@example.com", "U", "member", "cli"),
		tid("mismatch-project"), []string{"agent:read"})

	cases := []struct {
		name     string
		request  AuthzRequest
		wantDeny string
	}{
		{
			name: "unknown identity with supplied user principal kind",
			request: AuthzRequest{
				Principal: PrincipalContext{Kind: PrincipalKindUser, Identity: unknown},
				Resource:  Resource{Type: "agent", ID: tid("mismatch-target")},
				Action:    ActionRead,
			},
			wantDeny: "principal kind does not match identity",
		},
		{
			// The identity's own derived credential kind is empty (unrecognized),
			// not UAT, so the credential-mismatch check does not apply here — this
			// denies via the unrecognized-principal-kind rejection instead, which
			// fires first and denies regardless of the supplied credential.
			name: "unknown identity with supplied interactive credential kind",
			request: AuthzRequest{
				Principal:  PrincipalContext{Identity: unknown},
				Credential: CredentialContext{Kind: CredentialKindInteractive},
				Resource:   Resource{Type: "agent", ID: tid("mismatch-target")},
				Action:     ActionRead,
			},
			wantDeny: "unrecognized principal kind",
		},
		{
			name: "UAT identity with supplied interactive credential kind",
			request: AuthzRequest{
				Principal:  PrincipalContext{Identity: scopedUAT},
				Credential: CredentialContext{Kind: CredentialKindInteractive},
				Resource:   Resource{Type: "agent", ID: tid("mismatch-target")},
				Action:     ActionRead,
			},
			wantDeny: "credential kind does not match identity",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			authz := &AuthzService{}
			decision := authz.Decide(context.Background(), tc.request)
			assert.False(t, decision.Allowed)
			assert.Equal(t, tc.wantDeny, decision.Reason)
		})
	}
}

// =============================================================================
// T-ID-6: session-only gates deny non-session credentials
// =============================================================================

// TestSessionGates_DenyNonSessionCredentials pins T-ID-6: the token-management
// and project-deletion session gates deny an unrecognized identity exactly as
// they deny a UAT or agent JWT today, and keep admitting interactive/dev
// sessions unchanged.
func TestSessionGates_DenyNonSessionCredentials(t *testing.T) {
	userID := tid("session-gate-user")
	interactiveUser := NewAuthenticatedUser(userID, "u@example.com", "U", "member", "cli")
	devUser := NewDevUser(DevUserConfig{Username: "dev", DisplayName: "Dev", Email: "dev@localhost"})
	scopedUAT := NewScopedUserIdentityWithCredentialID(interactiveUser, tid("session-gate-project"), []string{"agent:read"}, tid("session-gate-cred"))
	agentJWT := &agentIdentityWrapper{&AgentTokenClaims{Claims: jwt.Claims{Subject: "agent-1"}}}
	unknown := &unclassifiedMockIdentity{id: tid("session-gate-unknown")}

	svc := &UserAccessTokenService{}

	ctxFor := func(identity Identity) context.Context {
		ctx := contextWithIdentity(context.Background(), identity)
		return contextWithCredentialContext(ctx, credentialContextForIdentity(identity))
	}

	cases := []struct {
		name     string
		identity Identity
		matchID  string // the userID enforceSessionCredential's identity-match step expects
		wantDeny bool
	}{
		{"interactive session", interactiveUser, userID, false},
		{"dev session", devUser, DevUserID, false},
		{"UAT credential", scopedUAT, userID, true},
		{"agent JWT credential", agentJWT, "agent-1", true},
		{"unrecognized identity", unknown, tid("session-gate-unknown"), true},
		{"nil identity", nil, tid("session-gate-nonexistent"), true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := ctxFor(tc.identity)

			err := requireSessionCredential(ctx)
			if tc.wantDeny {
				assert.Error(t, err, "requireSessionCredential")
			} else {
				assert.NoError(t, err, "requireSessionCredential")
			}

			// enforceSessionCredential additionally requires the context
			// identity to match the target user; matchID supplies the ID that
			// satisfies that check so the credential-kind check above is what
			// each case actually pins.
			err = svc.enforceSessionCredential(ctx, tc.matchID)
			if tc.wantDeny {
				assert.Error(t, err, "enforceSessionCredential")
			} else {
				assert.NoError(t, err, "enforceSessionCredential")
			}
		})
	}
}

// =============================================================================
// T-ID-7: ancestry attestation requires local provenance
// =============================================================================

// TestAncestryAttestation_RequiresLocalProvenance pins T-ID-7: an identity
// that merely returns Type() == "agent" and carries an ancestry chain naming
// a real user is not attested without the explicit marker, and a relationship
// grant consumer denies it exactly as it denies a federated agent — for a
// different reason, but with the same outcome.
func TestAncestryAttestation_RequiresLocalProvenance(t *testing.T) {
	creatorID := tid("provenance-creator")

	federatedAgent := NewFederatedAgentIdentity(
		"https://remote-hub.example.com", "remote-agent-1", "remote-project",
		"Remote Agent", creatorID, []string{creatorID}, nil)
	unattested := &unattestedMockAgentIdentity{
		id: tid("provenance-mock-agent"), projectID: tid("provenance-project"),
		ancestry: []string{creatorID},
	}
	attested := &agentIdentityWrapper{&AgentTokenClaims{
		Claims:   jwt.Claims{Subject: "attested-agent"},
		Ancestry: []string{creatorID},
	}}

	assert.False(t, AncestryIsHubAttested(federatedAgent), "federated ancestry must never be attested")
	assert.False(t, AncestryIsHubAttested(unattested), "an unmarked agent-shaped identity must not be attested")
	assert.True(t, AncestryIsHubAttested(attested), "a hub-signed agent JWT must remain attested")

	for _, tc := range []struct {
		name  string
		agent AgentIdentity
	}{
		{"federated agent claiming local ancestry", federatedAgent},
		{"unattested mock agent naming the real creator", unattested},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := EvaluateProgenyGrant(tc.agent, RelProgenySecretRead, "secret-123", "secret", creatorID, true)
			assert.False(t, result.Allowed, "a non-attested ancestry chain must not earn a progeny relationship grant")
		})
	}

	// The attested agent, with matching ancestry and AllowProgeny, does earn
	// the grant — establishing that the two mock cases above were denied for
	// lack of attestation, not for some unrelated reason.
	allowed := EvaluateProgenyGrant(attested, RelProgenySecretRead, "secret-123", "secret", creatorID, true)
	assert.True(t, allowed.Allowed, "an attested agent with matching ancestry should earn the progeny grant")
}
