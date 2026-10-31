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
	"go/token"
	"go/types"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// Classes of the federation credential census
// (TestFederationPrincipalCheckCensus).
const (
	// fedCensusAcceptor accepts federation credentials from requests: it
	// must verify them through the single acceptance point,
	// AuthenticateWithOptions (directly or through the Authenticate
	// wrapper), and pass this hub's platform service account and proxy
	// driver principals in the options.
	fedCensusAcceptor = "acceptor"
	// fedCensusVerifier is the one verifier: it must run
	// federationPrincipalRefusal.
	fedCensusVerifier = "verifier"
	// fedCensusWrapper delegates to AuthenticateWithOptions, so it inherits
	// the check.
	fedCensusWrapper = "wrapper"
	// fedCensusCheck is federationPrincipalRefusal itself: it must apply
	// each principal rule.
	fedCensusCheck = "check"
	// fedCensusRefuses refuses any request carrying the federation header.
	fedCensusRefuses = "refuses"
	// fedCensusNoCredential handles configuration, construction or claims
	// already verified by the verifier, and never verifies a credential
	// itself.
	fedCensusNoCredential = "no-credential"
)

// fedCredentialCensus classifies every pkg/hub function that touches
// federation credential acceptance (see fedCensusSites). Adding a function
// that accepts federation credentials without the principal check fails
// TestFederationPrincipalCheckCensus until it is classified and carries it.
var fedCredentialCensus = map[string]string{
	"auth.go:UnifiedAuthMiddleware":                                                fedCensusAcceptor,
	"federation_auth.go:(*FederationAuthenticator).AuthenticateWithOptions":        fedCensusVerifier,
	"federation_auth.go:(*FederationAuthenticator).Authenticate":                   fedCensusWrapper,
	"federation_auth.go:federationPrincipalRefusal":                                fedCensusCheck,
	"test_identity_workload_issuer.go:(*workloadIssuerAuthenticator).authenticate": fedCensusRefuses,
	// Reads the Google issuer configuration for the external bearer path;
	// that path never receives the federation header and verifies nothing here.
	"auth_external_bearer.go:googleTrust":                        fedCensusNoCredential,
	"federation_auth.go:(*FederationAuthenticator).IssuerConfig": fedCensusNoCredential,
	"federation_auth.go:NewFederationAuthenticator":              fedCensusNoCredential,
	"federation_auth.go:federationAuthOptionsArg":                fedCensusNoCredential,
	"federation_auth.go:extractHubTokenIdentity":                 fedCensusNoCredential,
	"federation_auth.go:extractHubUserClaims":                    fedCensusNoCredential,
	"federation_auth.go:extractHubClaims":                        fedCensusNoCredential,
	"federation_auth.go:extractServiceAccountClaims":             fedCensusNoCredential,
	"federation_auth.go:extractUserClaims":                       fedCensusNoCredential,
	"federation_auth.go:hasHubAgentClaims":                       fedCensusNoCredential,
	"handlers_project_visitors.go:(*Server).visitorGrantIssuer":  fedCensusNoCredential,
	"operational_settings.go:ApplySnapshot":                      fedCensusNoCredential,
	// Names the federation header only to remove it from a port proxy
	// request before the request reaches the agent's app; verifies nothing.
	"port_forward_handlers.go:sensitiveHeader": fedCensusNoCredential,
	"server.go:New": fedCensusNoCredential,
	"test_admin_gate.go:(*Server).VerifyTestAdminFederationOff": fedCensusNoCredential,
	// Reads the federation header only to leave such a request unchanged;
	// it verifies a proxy driver's Google ID token, never a federation
	// credential.
	"test_identity_web_driver.go:(*WebServer).fixtureSessionBearer": fedCensusNoCredential,
}

// fedCensusMarkers are the identifiers whose use makes a function a census
// site: the federation header, the verifier and its options, the claims
// type, and the authenticator and its holders.
var fedCensusMarkers = []string{
	"FederationTokenHeader",
	"AuthenticateWithOptions",
	"FederationAuthOptions",
	"federationClaims",
	"FederationAuthenticator",
	"federationAuth",
	"FederationAuth",
}

// fedCensusKey names fn as "file:Name" or "file:(Recv).Name".
func fedCensusKey(file string, fn *ast.FuncDecl) string {
	if fn.Recv != nil && len(fn.Recv.List) > 0 {
		return file + ":(" + types.ExprString(fn.Recv.List[0].Type) + ")." + fn.Name.Name
	}
	return file + ":" + fn.Name.Name
}

// fedCensusSite reports whether fn is a census site: its signature or body
// uses a fedCensusMarkers identifier or the federation header literal, or
// it is a method of FederationAuthenticator.
func fedCensusSite(fn *ast.FuncDecl) bool {
	found := false
	ast.Inspect(fn, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.Ident:
			for _, m := range fedCensusMarkers {
				if x.Name == m {
					found = true
				}
			}
		case *ast.BasicLit:
			if x.Kind == token.STRING && strings.Contains(strings.ToLower(x.Value), strings.ToLower(FederationTokenHeader)) {
				found = true
			}
		}
		return !found
	})
	return found
}

// setsOptionsField reports whether fn builds a FederationAuthOptions
// literal that sets field.
func setsOptionsField(fn *ast.FuncDecl, field string) bool {
	found := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok || types.ExprString(lit.Type) != "FederationAuthOptions" {
			return true
		}
		for _, elt := range lit.Elts {
			if kv, ok := elt.(*ast.KeyValueExpr); ok {
				if k, ok := kv.Key.(*ast.Ident); ok && k.Name == field {
					found = true
				}
			}
		}
		return true
	})
	return found
}

// verifiesWithOptions reports whether fn calls Authenticate or
// AuthenticateWithOptions with a credential and an options argument (the
// proxy authenticator's Authenticate takes the request alone).
func verifiesWithOptions(fn *ast.FuncDecl) bool {
	found := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && len(call.Args) == 2 &&
			(sel.Sel.Name == "Authenticate" || sel.Sel.Name == "AuthenticateWithOptions") {
			found = true
		}
		return !found
	})
	return found
}

// TestFederationPrincipalCheckCensus: every pkg/hub function that touches
// federation credential acceptance is classified, and its class holds:
// acceptors verify through AuthenticateWithOptions (directly or through
// the Authenticate wrapper) with this hub's platform service account and
// proxy driver principals, the verifier runs federationPrincipalRefusal,
// the check applies each principal rule, and no other function verifies a
// federation credential. No entry is stale.
func TestFederationPrincipalCheckCensus(t *testing.T) {
	type site struct {
		fn    *ast.FuncDecl
		calls map[string]int
		refs  map[string]bool
	}
	seen := map[string]site{}
	walkPkgHubFuncs(t, func(file string, fn *ast.FuncDecl, _ *token.FileSet) {
		if !fedCensusSite(fn) {
			return
		}
		seen[fedCensusKey(file, fn)] = site{fn: fn, calls: identCalls(fn), refs: identRefs(fn)}
	})

	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	verifies := func(s site) bool { return s.calls["AuthenticateWithOptions"] > 0 || s.calls["Authenticate"] > 0 }
	for _, key := range keys {
		s := seen[key]
		class, ok := fedCredentialCensus[key]
		if !assert.True(t, ok, "unclassified federation credential site %s: classify it in fedCredentialCensus; if it accepts federation credentials it must verify them through AuthenticateWithOptions", key) {
			continue
		}
		switch class {
		case fedCensusAcceptor:
			assert.True(t, verifiesWithOptions(s.fn), "%s: an acceptor must verify through AuthenticateWithOptions or its Authenticate wrapper, passing FederationAuthOptions", key)
			for _, field := range []string{"PlatformAuthSA", "DriverPrincipals"} {
				assert.True(t, setsOptionsField(s.fn, field), "%s: an acceptor must pass %s in FederationAuthOptions", key, field)
			}
		case fedCensusVerifier:
			assert.Positive(t, s.calls["federationPrincipalRefusal"], "%s: the verifier must run federationPrincipalRefusal", key)
		case fedCensusWrapper:
			assert.Positive(t, s.calls["AuthenticateWithOptions"], "%s: a wrapper must delegate to AuthenticateWithOptions", key)
		case fedCensusCheck:
			for _, rule := range []string{"IsReservedTestIdentityEmail", "IsFederatedSyntheticEmail", "isTransportIdentity"} {
				assert.Positive(t, s.calls[rule], "%s: the check must call %s", key, rule)
			}
		case fedCensusRefuses:
			assert.True(t, s.refs["FederationTokenHeader"], "%s: must read the federation header to refuse it", key)
			assert.False(t, verifies(s), "%s: must not verify a federation credential", key)
		case fedCensusNoCredential:
			assert.False(t, verifies(s), "%s: classified %q but verifies a credential", key, class)
		default:
			t.Errorf("%s: unknown class %q", key, class)
		}
	}
	for key := range fedCredentialCensus {
		_, ok := seen[key]
		assert.True(t, ok, "stale fedCredentialCensus entry %s: no such site", key)
	}
}
