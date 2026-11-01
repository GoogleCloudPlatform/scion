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
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Agent mint census (ptone/scion#4240, admin tier A3; advisor requirement
// 4, review R1). It parses every non-test Go file under pkg/hub,
// subpackages included (go/ast), and finds every function that decides an
// agent token's or a synthetic agent identity's scope list:
//
//   - a composite literal of AgentTokenGrant or AgentTokenClaims (also
//     package-qualified) that sets Scopes;
//   - any assignment to a field named Scopes, whatever the receiver.
//
// Each must call ceilingFilteredAgentScopes (which applies the test-admin
// clamp) or clampTestAdminRootedScopes (an actual call; a comment does not
// count), or be classified in agentScopeCensusExempt. A stale exemption
// fails. The unclamped chain filter (chainFilteredAgentScopes) is pinned to
// its two callers.

// agentScopeCensusExempt lists the scope sites ("<path under pkg/hub>:
// <func>") that neither filter through ceilingFilteredAgentScopes nor
// clamp, each with its reason.
var agentScopeCensusExempt = map[string]string{
	"agenttoken.go:SignAgentToken":                     "copies grant.Scopes; every AgentTokenGrant with scopes is built by AuthorizeAgentToken (pinned below); the empty-grant fallback agent:status:update is allowed (TestTestAdminA3_ScopeCensus)",
	"conduit_relay_admit.go:writeRelayAdmission":       "relay admission claims of a USER principal: copies a scoped user credential's own scopes (only a user access token reaches relay admission: relayUserAuthTypes admits no workload delegation), never an agent token's (the agent branch copies only the credential ID); every later relay operation rebuilds the user and is decided with the test-admin policy applied to that user, so the agent clamp is not the control here (pinned by TestTestAdminA3_RelayUserIdentityIgnoresClaimScopes)",
	"agent_scope_reissue.go:planAgentDelegatorReissue": "scopes from AuthorizeAgentToken (parentGrant)",
	"delivery_census.go:launchDeliveryUATFacts":        "read-only census identity for the user secret parent ceiling, built exactly as startSecretResolveOpts builds its owner-path identity (stored role scopes); never signed, and the ceiling evaluation it feeds reads no scope",
	"httpdispatcher.go:startSecretResolveOpts":         "dispatch-time secret resolution identity (resolveSecrets and resolveAsNeededForKeys), never signed; unchanged by A3 by decision (ti-adm-em 11:41Z): dispatch-time project-scope injection stays under A2's membership rule (provenance-root membership, SystemAuthorityProof)",
}

// agentScopeSite is one function that decides an agent scope list.
type agentScopeSite struct {
	key     string
	clamped bool // calls ceilingFilteredAgentScopes or clampTestAdminRootedScopes
}

// a3TypeName returns the type name of a composite literal type (T, pkg.T,
// *T).
func a3TypeName(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return x.Sel.Name
	case *ast.StarExpr:
		return a3TypeName(x.X)
	}
	return ""
}

// a3CallName returns the called function or method name.
func a3CallName(call *ast.CallExpr) string {
	switch f := call.Fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	}
	return ""
}

// agentScopeSitesInFile returns the scope sites of one source file.
func agentScopeSitesInFile(rel string, src []byte) ([]agentScopeSite, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, rel, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	var out []agentScopeSite
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		site, clamped := false, false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CallExpr:
				if name := a3CallName(x); name == "ceilingFilteredAgentScopes" || name == "clampTestAdminRootedScopes" {
					clamped = true
				}
			case *ast.CompositeLit:
				if t := a3TypeName(x.Type); t == "AgentTokenGrant" || t == "AgentTokenClaims" {
					for _, el := range x.Elts {
						if kv, ok := el.(*ast.KeyValueExpr); ok {
							if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "Scopes" {
								site = true
							}
						}
					}
				}
			case *ast.AssignStmt:
				for _, lhs := range x.Lhs {
					if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "Scopes" {
						site = true
					}
				}
			}
			return true
		})
		if site {
			out = append(out, agentScopeSite{key: rel + ":" + fn.Name.Name, clamped: clamped})
		}
	}
	return out, nil
}

// a3HubSources calls visit for every non-test Go file under pkg/hub,
// subpackages included, with its path relative to pkg/hub.
func a3HubSources(t *testing.T, visit func(rel string, src []byte)) {
	t.Helper()
	// pkgmove:scan-covers pkg/hub/...
	// recursive walk of pkg/hub (non-test files, no SkipDir): covers every directory below it.
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		visit(filepath.ToSlash(path), src)
		return nil
	})
	require.NoError(t, err)
}

func TestTestAdminA3_AgentMintCensus(t *testing.T) {
	sites := map[string]bool{}
	var unclamped []string
	used := map[string]bool{}
	a3HubSources(t, func(rel string, src []byte) {
		got, err := agentScopeSitesInFile(rel, src)
		require.NoError(t, err, rel)
		for _, s := range got {
			sites[s.key] = true
			if s.clamped {
				continue
			}
			if _, ok := agentScopeCensusExempt[s.key]; ok {
				used[s.key] = true
				continue
			}
			unclamped = append(unclamped, s.key)
		}
	})
	require.NotEmpty(t, sites)
	sort.Strings(unclamped)
	assert.Empty(t, unclamped, "agent scope sites that neither filter through ceilingFilteredAgentScopes nor clamp: clamp, or classify in agentScopeCensusExempt")
	for key := range agentScopeCensusExempt {
		assert.True(t, used[key], "stale agent mint exemption %s", key)
	}
	for _, want := range []string{"agent_token_mint.go:AuthorizeAgentToken", "scheduled_authority.go:resolveScheduledAgent",
		"skill_dispatch_resolve.go:creatorIdentityForAgent", "artifacts_recipient.go:recipientIdentity"} {
		assert.True(t, sites[want], "scope site %s found", want)
	}

	// The unclamped chain filter is called only by the clamped wrapper and
	// by the child edge coverage (see chainFilteredAgentScopes).
	var callers []string
	a3HubSources(t, func(rel string, src []byte) {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, rel, src, parser.SkipObjectResolution)
		require.NoError(t, err, rel)
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if c, ok := n.(*ast.CallExpr); ok && a3CallName(c) == "chainFilteredAgentScopes" {
					callers = append(callers, rel+":"+fn.Name.Name)
				}
				return true
			})
		}
	})
	assert.ElementsMatch(t, []string{"authz_effect_ceiling.go:ceilingFilteredAgentScopes", "authz_effect_ceiling.go:agentRowEffectCeiling"}, callers)
}

// TestTestAdminA3_AgentMintCensus_Detects plants each scope-setting form the
// census must find (review R1), including in a subpackage-style file, and
// checks that only a real call counts as clamped.
func TestTestAdminA3_AgentMintCensus_Detects(t *testing.T) {
	cases := map[string]struct {
		body    string
		clamped bool
	}{
		"claims literal":           {body: `_ = &AgentTokenClaims{Scopes: s}`},
		"qualified claims literal": {body: `_ = hub.AgentTokenClaims{ProjectID: p, Scopes: s}`},
		"grant literal":            {body: `_ = AgentTokenGrant{Scopes: s}`},
		"copy then assign":         {body: "c := *claims\n\tc.Scopes = extra"},
		"assign on any receiver":   {body: `tok.Scopes = append(tok.Scopes, ScopeProjectSecretRead)`},
		"comment does not clamp":   {body: "// ceilingFilteredAgentScopes(ctx, a, s) runs elsewhere\n\t_ = &AgentTokenClaims{Scopes: s}"},
		"really clamped":           {body: "s, _ := a.clampTestAdminRootedScopes(ctx, ag, s)\n\t_ = &AgentTokenClaims{Scopes: s}", clamped: true},
	}
	for name, tc := range cases {
		src := "package x\n\nfunc planted() {\n\t" + tc.body + "\n}\n"
		sites, err := agentScopeSitesInFile("internal/x/planted.go", []byte(src))
		require.NoError(t, err, name)
		require.Len(t, sites, 1, "%s: the planted site is found", name)
		assert.Equal(t, tc.clamped, sites[0].clamped, name)
	}
	sites, err := agentScopeSitesInFile("planted.go", []byte("package x\n\nfunc planted() {\n\t_ = AgentTokenClaims{ProjectID: p}\n}\n"))
	require.NoError(t, err)
	assert.Empty(t, sites, "a literal without Scopes decides no scope list")
}
