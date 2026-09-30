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
// localAncestryProvenanceIdentity or explicitIdentityClassification. It
// exists to prove that both ancestry attestation and principal/credential
// classification require their explicit markers rather than following from
// Type() == "agent", a plausible-looking ancestry chain, or satisfying the
// AgentIdentity interface's method set.
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

// userShapedMockIdentity is an Identity with the minimal ID()/Type() method
// set, returning Type() == "user", but it is not *AuthenticatedUser (or any
// other concrete type principalContextForIdentity/credentialContextForIdentity
// recognize) and does not implement explicitIdentityClassification. It exists
// to prove classification keys on concrete type: a caller-controlled type
// returning a familiar-looking Type() string must not be admitted as if it
// were the classified type that string names.
type userShapedMockIdentity struct {
	id string
}

func (m *userShapedMockIdentity) ID() string   { return m.id }
func (m *userShapedMockIdentity) Type() string { return "user" }

// recognizedPrincipalEmptyCredentialMockIdentity opts into
// explicitIdentityClassification with a recognized PrincipalKind but an
// empty (unrecognized) CredentialKind — the shape credentialContextForIdentity
// produces for a concrete type whose classifier arm was added for the
// principal side but never for the credential side. It pins the rejection
// block's credential-side arm independently of the principal-side one.
type recognizedPrincipalEmptyCredentialMockIdentity struct {
	id string
}

func (m *recognizedPrincipalEmptyCredentialMockIdentity) ID() string   { return m.id }
func (m *recognizedPrincipalEmptyCredentialMockIdentity) Type() string { return "user" }
func (m *recognizedPrincipalEmptyCredentialMockIdentity) authzClassification() (PrincipalKind, CredentialKind) {
	return PrincipalKindUser, ""
}

// =============================================================================
// Source-scan drift guard
// =============================================================================

// identityInventoryExpectation is the classified inventory of every non-test
// pkg/hub concrete type that implements Identity, keyed by the type's
// declared name. attested records AncestryIsHubAttested's planned outcome. A
// new identity type added to non-test pkg/hub source without a row here — or
// without a localAncestryProvenance() implementation matching its row —
// fails TestIdentityClassification_EveryTypeHasExplicitOutcome/SourceScan.
// That is the point: it forces classification to be a deliberate, explicitly
// classified edit. See the AST-scan limitations noted on scanIdentitySource
// below.
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
//
// This is an AST-level scan, not a go/types-based one: it never resolves
// imports or type-checks, so it detects an Identity implementer only through
// a directly declared ID()+Type() method pair, or by embedding a field named
// literally "UserIdentity", "AgentIdentity", or "Identity" declared in this
// package. It has two known blind spots as a result: (1) a type that
// satisfies Identity only by embedding some other struct that itself embeds
// one of those interfaces (double indirection) is not detected, since the
// scan does not follow embedded structs transitively; and (2) a method
// declared in a different package (or promoted from an interface type this
// scan doesn't recognize by name) is invisible to it. Both are acceptable for
// today's inventory, where every production Identity implementer declares
// ID()+Type() directly or embeds one of the three interfaces above, but a
// refactor that introduces either pattern would silently escape this guard —
// it would not fail loudly, it would simply stop checking the new type.
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

// hasClassificationEscape reports whether typeName declares authzClassification,
// the explicitIdentityClassification opt-in. It is a test-only escape hatch:
// production types are classified by concrete type in
// principalContextForIdentity/credentialContextForIdentity, never through this
// marker.
func (inv identitySourceInventory) hasClassificationEscape(typeName string) bool {
	return inv.methods[typeName]["authzClassification"]
}

// TestIdentityClassification_EveryTypeHasExplicitOutcome pairs a
// source-level drift guard (subtest SourceScan) that fails if a new non-test
// pkg/hub Identity type appears without a classified row in
// identityInventoryExpectation or without localAncestryProvenance matching
// that row, with a runtime table (subtest ClassifierOutcomes) that exercises
// principalContextForIdentity, credentialContextForIdentity and
// AncestryIsHubAttested against a constructed instance of every inventory
// row, plus nil and two kinds of unrecognized identity: a concrete type this
// package has not classified, and a type whose Type() string merely
// resembles a recognized one without being the type that string names.
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
			"a new non-test pkg/hub Identity type (or a removed one) needs a classified row in identityInventoryExpectation")

		for name := range found {
			assert.Falsef(t, inv.hasClassificationEscape(name),
				"%s: authzClassification is a test-only escape hatch; no non-test pkg/hub Identity type may implement it", name)
		}

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
				// A concrete type this package has not classified is
				// unclassified — including principal/credential kind — even
				// when it fully implements AgentIdentity and carries a
				// plausible-looking ancestry chain naming a real user. Only
				// the explicit marker (or an explicitly classified concrete
				// type) classifies.
				name:               "agent-shaped type without the ancestry or classification marker",
				identity:           &unattestedMockAgentIdentity{id: tid("classify-unattested-agent"), projectID: tid("classify-project"), ancestry: []string{tid("classify-user")}},
				wantPrincipalKind:  "",
				wantCredentialKind: "",
				wantAttested:       false,
			},
			{
				// A minimal Identity returning Type() == "user" is not
				// thereby treated as an AuthenticatedUser: classification is
				// keyed on concrete type, not on a string a caller's type
				// happens to return.
				name:               "user-shaped type with only Type()==\"user\"",
				identity:           &userShapedMockIdentity{id: tid("classify-user-shaped")},
				wantPrincipalKind:  "",
				wantCredentialKind: "",
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
// Decide fail-closed classification
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

// TestDecide_UnrecognizedDerivedPrincipalKindDenied: Decide denies a nil
// identity with "missing principal", and denies an identity of an
// unrecognized concrete type, an agent-shaped identity that hasn't opted into
// ancestry attestation or classification, and a user-shaped identity whose
// only resemblance to AuthenticatedUser is its Type() string, with
// "unrecognized principal kind" — because classification, not attestation and
// not Type(), gates entry. Exactly one audit record is emitted per call in
// every case, decorated with the (empty, for the nil case) derived
// classification.
func TestDecide_UnrecognizedDerivedPrincipalKindDenied(t *testing.T) {
	cases := []struct {
		name       string
		identity   Identity
		wantReason string
	}{
		{"nil identity", nil, "missing principal"},
		{"unrecognized concrete type", &unclassifiedMockIdentity{id: tid("decide-unknown")}, "unrecognized principal kind"},
		{"agent-shaped type without the ancestry or classification marker", &unattestedMockAgentIdentity{id: tid("decide-unattested-agent"), projectID: tid("decide-project"), ancestry: []string{tid("decide-user")}}, "unrecognized principal kind"},
		{"user-shaped type with only Type()==\"user\"", &userShapedMockIdentity{id: tid("decide-user-shaped")}, "unrecognized principal kind"},
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
			assert.Equal(t, tc.wantReason, decision.Reason)
			assert.Equal(t, 1, emitter.calls, "exactly one audit record must be emitted")
		})
	}
}

// TestDecide_SuppliedKindCannotReclassifyIdentity covers the credential
// compatibility rule: a supplied Principal.Kind that
// does not match the identity's own classification always denies — an
// unrecognized identity can never be upgraded into a recognized one by
// supplied context. A supplied Credential.Kind is admitted only through the
// explicit compatibility predicate (suppliedCredentialCompatible): equal
// kinds, or a local user's own interactive classification narrowed to UAT.
// Every other combination denies, including every non-user principal
// presented with a kind other than its own, dev presented with UAT (dev is
// deliberately not in the exception — a used UAT represents its local user
// owner, not dev's token-issuing power), and a recognized identity presented
// with a supplied credential kind that isn't one of the enum's recognized
// values at all (distinct from a same-enum mismatch: see the "unrecognized
// credential kind" cases below). Broker on-behalf-of is covered separately
// in the broker OBO tests, since it depends on context provenance this table
// does not set up.
func TestDecide_SuppliedKindCannotReclassifyIdentity(t *testing.T) {
	unknown := &unclassifiedMockIdentity{id: tid("mismatch-unknown")}
	interactiveUser := NewAuthenticatedUser(tid("mismatch-interactive-user"), "u@example.com", "U", "member", "cli")
	scopedUAT := NewScopedUserIdentity(
		NewAuthenticatedUser(tid("mismatch-uat-user"), "u@example.com", "U", "member", "cli"),
		tid("mismatch-project"), []string{"agent:read"})
	devUser := NewDevUser(DevUserConfig{Username: "dev", DisplayName: "Dev", Email: "dev@localhost"})
	agentJWT := &agentIdentityWrapper{&AgentTokenClaims{Claims: jwt.Claims{Subject: "mismatch-agent"}}}
	fedUser := NewFederatedUserIdentity("https://issuer.example", "mismatch-fed", "f@example.com", "F", "member", nil)

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
			// The identity's own derived principal kind is empty
			// (unrecognized), so the principal-mismatch check above never
			// fires (no supplied Principal.Kind here) and the
			// unrecognized-kind rejection denies first, regardless of the
			// supplied credential.
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
		{
			name: "interactive user with an unrecognized supplied credential kind string",
			request: AuthzRequest{
				Principal:  PrincipalContext{Identity: interactiveUser},
				Credential: CredentialContext{Kind: CredentialKind("bogus")},
				Resource:   Resource{Type: "agent", ID: tid("mismatch-target")},
				Action:     ActionRead,
			},
			wantDeny: "unrecognized credential kind",
		},
		{
			name: "dev identity with supplied UAT credential kind (dev is not in the exception)",
			request: AuthzRequest{
				Principal:  PrincipalContext{Identity: devUser},
				Credential: CredentialContext{Kind: CredentialKindUAT},
				Resource:   Resource{Type: "agent", ID: tid("mismatch-target")},
				Action:     ActionRead,
			},
			wantDeny: "credential kind does not match identity",
		},
		{
			name: "agent identity with supplied UAT credential kind",
			request: AuthzRequest{
				Principal:  PrincipalContext{Identity: agentJWT},
				Credential: CredentialContext{Kind: CredentialKindUAT},
				Resource:   Resource{Type: "agent", ID: tid("mismatch-target")},
				Action:     ActionRead,
			},
			wantDeny: "credential kind does not match identity",
		},
		{
			name: "agent identity with supplied broker credential kind",
			request: AuthzRequest{
				Principal:  PrincipalContext{Identity: agentJWT},
				Credential: CredentialContext{Kind: CredentialKindBroker, ID: agentJWT.ID(), Type: "broker"},
				Resource:   Resource{Type: "agent", ID: tid("mismatch-target")},
				Action:     ActionRead,
			},
			wantDeny: "credential kind does not match identity",
		},
		{
			name: "federated user identity with supplied interactive credential kind",
			request: AuthzRequest{
				Principal:  PrincipalContext{Identity: fedUser},
				Credential: CredentialContext{Kind: CredentialKindInteractive},
				Resource:   Resource{Type: "agent", ID: tid("mismatch-target")},
				Action:     ActionRead,
			},
			wantDeny: "credential kind does not match identity",
		},
		{
			name: "interactive user with supplied broker credential kind but no OBO context",
			request: AuthzRequest{
				Principal:  PrincipalContext{Identity: interactiveUser},
				Credential: CredentialContext{Kind: CredentialKindBroker, ID: "some-broker", Type: "broker"},
				Resource:   Resource{Type: "agent", ID: tid("mismatch-target")},
				Action:     ActionRead,
			},
			wantDeny: "credential kind does not match identity",
		},
		{
			name: "interactive user with a supplied principal ID that does not match",
			request: AuthzRequest{
				Principal: PrincipalContext{ID: "some-other-id", Identity: interactiveUser},
				Resource:  Resource{Type: "agent", ID: tid("mismatch-target")},
				Action:    ActionRead,
			},
			wantDeny: "principal id does not match identity",
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

// TestDecide_EntryDenyAuditsDerivedClassification asserts that every entry
// deny decorates the Decision, and the emitted audit record, with the
// DERIVED principal and credential classification — never the caller's
// rejected supplied claim — and emits exactly one audit record.
func TestDecide_EntryDenyAuditsDerivedClassification(t *testing.T) {
	interactiveUserID := tid("audit-derived-user")
	interactiveUser := NewAuthenticatedUser(interactiveUserID, "u@example.com", "U", "member", "cli")
	scopedUAT := NewScopedUserIdentityWithCredentialID(interactiveUser, tid("audit-derived-project"), []string{"agent:read"}, tid("audit-derived-cred"))
	unknownID := tid("audit-unknown")

	cases := []struct {
		name              string
		request           AuthzRequest
		wantReason        string
		wantPrincipalKind PrincipalKind
		wantCredKind      string
		wantPrincipalID   string
		wantCredID        string
	}{
		{
			name: "principal kind mismatch decorates the derived kind, not the supplied one",
			request: AuthzRequest{
				Principal: PrincipalContext{Kind: PrincipalKindAgent, Identity: interactiveUser},
				Resource:  Resource{Type: "agent", ID: tid("audit-target")},
				Action:    ActionRead,
			},
			wantReason:        "principal kind does not match identity",
			wantPrincipalKind: PrincipalKindUser,
			wantCredKind:      string(CredentialKindInteractive),
			wantPrincipalID:   interactiveUserID,
		},
		{
			name: "credential kind mismatch decorates the derived kind, not the supplied one",
			request: AuthzRequest{
				Principal:  PrincipalContext{Identity: scopedUAT},
				Credential: CredentialContext{Kind: CredentialKindInteractive},
				Resource:   Resource{Type: "agent", ID: tid("audit-target")},
				Action:     ActionRead,
			},
			wantReason:        "credential kind does not match identity",
			wantPrincipalKind: PrincipalKindUser,
			wantCredKind:      string(CredentialKindUAT),
			wantPrincipalID:   interactiveUserID,
			wantCredID:        tid("audit-derived-cred"),
		},
		{
			name: "unrecognized identity decorates the empty derived kind",
			request: AuthzRequest{
				Principal: PrincipalContext{Identity: &unclassifiedMockIdentity{id: unknownID}},
				Resource:  Resource{Type: "agent", ID: tid("audit-target")},
				Action:    ActionRead,
			},
			wantReason:        "unrecognized principal kind",
			wantPrincipalKind: "",
			wantCredKind:      "",
			wantPrincipalID:   unknownID,
		},
		{
			name: "nil identity decorates the empty derived kind with its own reason",
			request: AuthzRequest{
				Resource: Resource{Type: "agent", ID: tid("audit-target")},
				Action:   ActionRead,
			},
			wantReason:        "missing principal",
			wantPrincipalKind: "",
			wantCredKind:      "",
			wantPrincipalID:   "",
		},
		{
			name: "unrecognized supplied credential kind string decorates the derived kind, not the rejected string",
			request: AuthzRequest{
				Principal:  PrincipalContext{Identity: interactiveUser},
				Credential: CredentialContext{Kind: CredentialKind("bogus")},
				Resource:   Resource{Type: "agent", ID: tid("audit-target")},
				Action:     ActionRead,
			},
			wantReason:        "unrecognized credential kind",
			wantPrincipalKind: PrincipalKindUser,
			wantCredKind:      string(CredentialKindInteractive),
			wantPrincipalID:   interactiveUserID,
		},
		{
			name: "recognized principal with an unrecognized derived credential kind denies at entry",
			request: AuthzRequest{
				Principal: PrincipalContext{Identity: &recognizedPrincipalEmptyCredentialMockIdentity{id: tid("audit-recognized-principal-empty-cred")}},
				Resource:  Resource{Type: "agent", ID: tid("audit-target")},
				Action:    ActionRead,
			},
			wantReason:        "unrecognized credential kind",
			wantPrincipalKind: PrincipalKindUser,
			wantCredKind:      "",
			wantPrincipalID:   tid("audit-recognized-principal-empty-cred"),
		},
		{
			// A nil identity's derived principal ID is empty; the record
			// must carry that derived "", never the caller's supplied claim.
			name: "nil identity with a supplied principal ID still records the derived empty ID",
			request: AuthzRequest{
				Principal: PrincipalContext{ID: "supplied-claim"},
				Resource:  Resource{Type: "agent", ID: tid("audit-target")},
				Action:    ActionRead,
			},
			wantReason:        "missing principal",
			wantPrincipalKind: "",
			wantCredKind:      "",
			wantPrincipalID:   "",
		},
		{
			// The identity is unrecognized and its own ID() is empty, so the
			// derived principal ID is "". A supplied, non-empty Principal.ID
			// differs from that derived "" and denies on the supplied-ID
			// check, ahead of the unrecognized-kind case. Either way the
			// record must carry the derived "" ID, never the supplied claim.
			name: "unrecognized identity with an empty ID and a supplied principal ID records the derived empty ID",
			request: AuthzRequest{
				Principal: PrincipalContext{ID: "supplied-claim", Identity: &unclassifiedMockIdentity{id: ""}},
				Resource:  Resource{Type: "agent", ID: tid("audit-target")},
				Action:    ActionRead,
			},
			wantReason:        "principal id does not match identity",
			wantPrincipalKind: "",
			wantCredKind:      "",
			wantPrincipalID:   "",
		},
		{
			name: "supplied principal ID mismatch decorates the derived ID, not the rejected claim",
			request: AuthzRequest{
				Principal: PrincipalContext{ID: "some-other-id", Identity: interactiveUser},
				Resource:  Resource{Type: "agent", ID: tid("audit-target")},
				Action:    ActionRead,
			},
			wantReason:        "principal id does not match identity",
			wantPrincipalKind: PrincipalKindUser,
			wantCredKind:      string(CredentialKindInteractive),
			wantPrincipalID:   interactiveUserID,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			emitter := &capturingAuditEmitter{}
			authz := &AuthzService{decisionAuditEmitter: emitter}
			decision := authz.Decide(context.Background(), tc.request)
			assert.False(t, decision.Allowed)
			assert.Equal(t, tc.wantReason, decision.Reason)
			assert.Equal(t, tc.wantPrincipalKind, decision.PrincipalKind, "decision must carry the derived principal kind")
			assert.Equal(t, tc.wantCredKind, decision.CredentialKind, "decision must carry the derived credential kind")

			require.Len(t, emitter.records, 1, "exactly one audit record must be emitted")
			record := emitter.records[0]
			assert.Equal(t, tc.wantReason, record.Reason, "audit record must carry the deny reason")
			assert.Equal(t, string(tc.wantPrincipalKind), record.PrincipalKind, "audit record must carry the derived principal kind, not the supplied one")
			assert.Equal(t, tc.wantCredKind, record.CredentialType, "audit record must carry the derived credential kind, not the supplied one")
			assert.Equal(t, tc.wantPrincipalID, record.PrincipalID, "audit record must carry the derived principal's ID, not the caller-supplied Principal.ID")
			assert.Equal(t, tc.wantCredID, record.CredentialID, "audit record must carry the derived credential's ID")
		})
	}
}

// TestDecide_AuditRecordsDerivedPrincipalIDWhenRequestOmitsIt moved to
// identity_classification_sqlite_test.go: it needs a real, store-backed test
// server, which carries a !no_sqlite constraint this file does not have.

// TestSuppliedCredentialCompatible_PairMatrix is the full compatibility
// matrix behind Decide's credential-mismatch check: every derived
// (PrincipalKind, CredentialKind) pair the classifiers can produce, crossed
// with every recognized CredentialKind plus one unrecognized string,
// asserting exactly which supplied kind may stand in for the identity's own
// derived classification.
// The broker on-behalf-of exception is exercised separately in the
// broker OBO tests, since it depends on ctx provenance this matrix's plain
// context.Background() never sets: with no OBO markers, "user/interactive"
// row's "broker" column below is (correctly) not admitted, matching "a
// plain user + broker credential without the OBO marker denies".
func TestSuppliedCredentialCompatible_PairMatrix(t *testing.T) {
	suppliedKinds := []CredentialKind{
		CredentialKindInteractive, CredentialKindUAT, CredentialKindAgentJWT,
		CredentialKindFederation, CredentialKindBroker, CredentialKindDev,
		CredentialKind("bogus"),
	}

	rows := []struct {
		name      string
		principal PrincipalKind
		derived   CredentialKind
		// admitted names every supplied kind that must be compatible IN
		// ADDITION to the derived kind itself (always compatible via
		// equality, asserted unconditionally below).
		admitted map[CredentialKind]bool
	}{
		{"user/interactive (AuthenticatedUser): the narrowing overlay admits UAT only", PrincipalKindUser, CredentialKindInteractive, map[CredentialKind]bool{CredentialKindUAT: true}},
		{"user/uat (ScopedUserIdentity): a real UAT follows equality only, not the exception", PrincipalKindUser, CredentialKindUAT, nil},
		{"dev/dev (DevUser): dev is deliberately excluded from the UAT exception", PrincipalKindDev, CredentialKindDev, nil},
		{"agent/agent_jwt", PrincipalKindAgent, CredentialKindAgentJWT, nil},
		{"federated_user/federation", PrincipalKindFederatedUser, CredentialKindFederation, nil},
		{"federated_agent/federation", PrincipalKindFederatedAgent, CredentialKindFederation, nil},
		{"federated_service/federation", PrincipalKindFederatedService, CredentialKindFederation, nil},
		{"broker/broker", PrincipalKindBroker, CredentialKindBroker, nil},
		{"unknown (empty principal/empty credential): any supplied kind denies", PrincipalKind(""), CredentialKind(""), nil},
	}

	ctx := context.Background() // no broker/OBO markers set: see the broker OBO tests for that dimension.

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			for _, supplied := range suppliedKinds {
				want := supplied == row.derived || row.admitted[supplied]
				got := suppliedCredentialCompatible(ctx, PrincipalContext{Kind: row.principal}, CredentialContext{Kind: row.derived}, CredentialContext{Kind: supplied})
				assert.Equalf(t, want, got, "principal=%q derived=%q supplied=%q", row.principal, row.derived, supplied)
			}
		})
	}
}

// =============================================================================
// Session-only gates deny non-session credentials
// =============================================================================

// TestSessionGates_DenyNonSessionCredentials: the token-management
// and project-deletion session gates deny an unrecognized identity, as they
// deny a UAT or agent JWT, and admit interactive/dev sessions.
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
// Ancestry attestation requires local provenance
// =============================================================================

// federatedWithAncestryMarkerTestIdentity implements BOTH FederatedIdentity
// (IssuerURL) and localAncestryProvenanceIdentity. It exists only to prove
// AncestryIsHubAttested's ordering: the FederatedIdentity rejection must run
// BEFORE the marker check, so this type — which would otherwise attest via
// the marker — is still denied. Nothing in production or in the other test
// fakes exercises this order (no existing fake is both federated and
// marked), so a regression that reordered the two checks would pass
// unnoticed without this type.
type federatedWithAncestryMarkerTestIdentity struct {
	id       string
	ancestry []string
}

func (f *federatedWithAncestryMarkerTestIdentity) ID() string   { return f.id }
func (f *federatedWithAncestryMarkerTestIdentity) Type() string { return "federated_agent" }
func (f *federatedWithAncestryMarkerTestIdentity) IssuerURL() string {
	return "https://remote-hub.example.com"
}
func (f *federatedWithAncestryMarkerTestIdentity) ProjectID() string             { return "" }
func (f *federatedWithAncestryMarkerTestIdentity) Scopes() []AgentTokenScope     { return nil }
func (f *federatedWithAncestryMarkerTestIdentity) HasScope(AgentTokenScope) bool { return false }
func (f *federatedWithAncestryMarkerTestIdentity) Ancestry() []string            { return f.ancestry }
func (f *federatedWithAncestryMarkerTestIdentity) TokenID() string               { return "" }
func (f *federatedWithAncestryMarkerTestIdentity) OriginUserID() string {
	if len(f.ancestry) > 0 {
		return f.ancestry[0]
	}
	return ""
}

// localAncestryProvenance is implemented deliberately, to prove the
// FederatedIdentity check still wins even when a type also carries this
// marker — which no real production or other test type ever does together.
func (f *federatedWithAncestryMarkerTestIdentity) localAncestryProvenance() ancestryProvenance {
	return ancestryProvenanceAgentJWT
}

// TestAncestryIsHubAttested_FederatedRejectionPrecedesMarkerCheck kills the
// mutation where AncestryIsHubAttested's marker check runs before (or
// instead of) the FederatedIdentity rejection: a type that is both federated
// and carries the ancestry marker must still be denied.
func TestAncestryIsHubAttested_FederatedRejectionPrecedesMarkerCheck(t *testing.T) {
	creatorID := tid("marker-order-creator")
	identity := &federatedWithAncestryMarkerTestIdentity{
		id: tid("marker-order-agent"), ancestry: []string{creatorID},
	}

	require.False(t, AncestryIsHubAttested(identity),
		"a federated identity must be rejected before its ancestry marker is ever consulted")

	result := EvaluateProgenyGrant(identity, RelProgenySecretRead, "secret-123", "secret", creatorID, true)
	assert.False(t, result.Allowed, "a federated identity's ancestry marker must not earn a progeny relationship grant")
}

// TestAncestryIsHubAttested_ScopedUserIdentityDelegatesToWrappedIdentity
// asserts that a *ScopedUserIdentity is not attested unconditionally — if the
// UserIdentity it wraps is itself federated, AncestryIsHubAttested must
// still deny it, even though IssuerURL is not promoted through the
// UserIdentity interface and so the top-level FederatedIdentity check alone
// cannot see it. Production never constructs this combination today
// (useraccesstoken.go only wraps *AuthenticatedUser), but the predicate must
// not depend on that being true forever.
func TestAncestryIsHubAttested_ScopedUserIdentityDelegatesToWrappedIdentity(t *testing.T) {
	wrappedLocal := NewAuthenticatedUser(tid("scoped-wraps-local"), "u@example.com", "U", "member", "cli")
	scopedLocal := NewScopedUserIdentity(wrappedLocal, tid("scoped-wraps-project"), []string{"agent:read"})
	assert.True(t, AncestryIsHubAttested(scopedLocal), "a UAT wrapping a local user stays attested")

	wrappedFederated := NewFederatedUserIdentity("https://issuer.example", "scoped-wraps-federated", "f@example.com", "F", "member", nil)
	scopedFederated := NewScopedUserIdentity(wrappedFederated, tid("scoped-wraps-project"), []string{"agent:read"})
	assert.False(t, AncestryIsHubAttested(scopedFederated), "a UAT wrapping a federated identity must not be attested")
}

// TestAncestryAttestation_RequiresLocalProvenance: an identity
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

// TestDecide_UnmarkedAgentMockDeniesBeforeRelationshipOrDelegationChecks
// extends TestAncestryAttestation_RequiresLocalProvenance, which
// exercised only EvaluateProgenyGrant directly: with classification by
// concrete type, an agent-shaped mock without the explicit classification
// marker denies at Decide's entry rejection block —
// before candidate gathering, before checkRelationshipGrants (the ancestor
// secret/envvar/skill-injection progeny path) and before
// checkDelegationCeiling (the agent-creates-agent depth-0 path) ever run.
// Both call sites read a.store, so this bare-store AuthzService would panic,
// not merely answer wrong, if classification let the identity reach them —
// making this a meaningful proof that the deny happens at entry.
func TestDecide_UnmarkedAgentMockDeniesBeforeRelationshipOrDelegationChecks(t *testing.T) {
	unmarked := &unattestedMockAgentIdentity{
		id: tid("t7-unmarked-agent"), projectID: tid("t7-project"),
		ancestry: []string{tid("t7-creator")},
	}
	authz := &AuthzService{}

	t.Run("relationship grant via Decide (ancestor secret read)", func(t *testing.T) {
		decision := authz.Decide(context.Background(), AuthzRequest{
			Principal: PrincipalContext{Identity: unmarked},
			Resource: Resource{
				Type:     "secret",
				ID:       tid("t7-secret"),
				OwnerID:  tid("t7-creator"),
				Ancestry: []string{tid("t7-creator")},
			},
			Action: ActionRead,
		})
		assert.False(t, decision.Allowed)
		assert.Equal(t, "unrecognized principal kind", decision.Reason)
	})

	t.Run("delegation ceiling (agent creates agent)", func(t *testing.T) {
		decision := authz.Decide(context.Background(), AuthzRequest{
			Principal: PrincipalContext{Identity: unmarked},
			Resource:  Resource{Type: "agent", ParentType: "project", ParentID: tid("t7-project")},
			Action:    ActionCreate,
		})
		assert.False(t, decision.Allowed)
		assert.Equal(t, "unrecognized principal kind", decision.Reason)
	})
}
