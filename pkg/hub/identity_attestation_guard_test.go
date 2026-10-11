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
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Ancestry attestation is decided in package hub by AncestryIsHubAttested
// (identity_attestation.go), in two ways:
//
//   - the local user types declared with the principal types are listed by
//     exact pointer type in a type switch (ancestryAttestedPrincipalTypes);
//   - every other attested type opts in through the unexported
//     localAncestryProvenanceIdentity marker, which only a type declared in
//     package hub can implement (ancestryMarkerTypes).
//
// The switch replaces the compile-time guarantee the marker method gives, so
// TestAncestryAttestationGuard pins both lists against the source of
// pkg/hub/... (production and test files): a new marker implementation, a new
// UserIdentity implementation, a new type embedding an attested type, a new
// assertion of the marker, a change to the switch, or an attested type whose
// declaration leaves pkg/hub/... fails it until the lists below are updated
// as a deliberate edit.

// ancestryAttestedPrincipalTypes is the exact set of case types in
// AncestryIsHubAttested's type switch. These types carry no marker method.
var ancestryAttestedPrincipalTypes = []string{"*AuthenticatedUser", "*ScopedUserIdentity"}

// ancestryMarkerTypes is the exact set of types in pkg/hub/... that declare
// an ancestry-provenance marker method, keyed by type name, with whether the
// declaring file is a test file.
var ancestryMarkerTypes = map[string]bool{
	"DevUser":              false,
	"agentIdentityWrapper": false,
	"explainAgentIdentity": false,
	"peerAgentIdentity":    false,
	"storedAgentIdentity":  false,
	// Test fakes.
	"federatedWithAncestryMarkerTestIdentity": true, // federated: rejected before the marker check
	"grantGuardAgentIdentity":                 true,
	"testAgentIdentity":                       true,
	"testProgenyAgentIdentity":                true,
	"testRelAgentIdentity":                    true,
	"wakeDMTestIdentity":                      true,
}

// userIdentityImplementations is every type in pkg/hub/... that implements
// UserIdentity as far as an AST scan can tell (it declares Email,
// DisplayName and Role, or embeds UserIdentity), with how
// AncestryIsHubAttested treats it. A new UserIdentity implementation must be
// classified here, and given a switch case or a marker if it is a local user
// whose ancestry the hub vouches for.
var userIdentityImplementations = map[string]string{
	"AuthenticatedUser":     "switch",
	"ScopedUserIdentity":    "switch",
	"DevUser":               "marker",
	"FederatedUserIdentity": "federated",
	// Test fakes.
	"bindableFederatedAdmin":  "federated",
	"bypassAgentsDevIdentity": "not attested",
	"devTypedUserIdentity":    "not attested",
	"federatedTestIdentity":   "not attested",
}

// ancestryEmbeddingExceptions lists the types allowed to embed an attested
// type, keyed by "Outer embeds Inner". Embedding a marker holder promotes the
// marker and attests the outer type; embedding a switch type does not (the
// switch matches exact types), so either silently changes attestation.
var ancestryEmbeddingExceptions = map[string]bool{
	// e2b_initiator_attribution_test.go: models a *DevUser-like wrapper; it is
	// attested through the promoted marker (pinned in
	// TestAncestryIsHubAttested_EquivalenceTable).
	"devUserEmbeddingWrapper embeds DevUser": true,
	// message_wake_lifecycle_test.go: a wake DM sender with token scopes; it
	// is attested through wakeDMTestIdentity's promoted marker (pinned in
	// TestAncestryIsHubAttested_EquivalenceTable).
	"wakeLifecycleAgentIdentity embeds wakeDMTestIdentity": true,
}

// ancestryAttestationFile is the file that declares the marker interface and
// AncestryIsHubAttested, relative to pkg/hub.
const ancestryAttestationFile = "identity_attestation.go"

// attestationTypeInfo is the AST-level description of one declared type.
type attestationTypeInfo struct {
	file    string // relative to pkg/hub, slash-separated
	methods map[string]bool
	embeds  []string // unqualified names of embedded fields
}

type attestationScan struct {
	types map[string]*attestationTypeInfo
	files map[string]*ast.File
	fset  *token.FileSet
	// dupes records type names declared more than once across pkg/hub/...
	dupes map[string][]string
}

func (s *attestationScan) typeInfo(name string) *attestationTypeInfo {
	ti := s.types[name]
	if ti == nil {
		ti = &attestationTypeInfo{methods: map[string]bool{}}
		s.types[name] = ti
	}
	return ti
}

// attestationEmbeddedName returns the unqualified type name of an embedded
// field type: T, *T, pkg.T and *pkg.T all give "T".
func attestationEmbeddedName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return attestationEmbeddedName(t.X)
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		return t.Sel.Name
	case *ast.IndexExpr:
		return attestationEmbeddedName(t.X)
	default:
		return ""
	}
}

// scanAttestationSources parses every .go file under pkg/hub, recursively,
// test files included, and records declared types, their methods and their
// embedded fields.
func scanAttestationSources(t *testing.T) *attestationScan {
	t.Helper()
	root := findHubDir(t)
	s := &attestationScan{
		types: map[string]*attestationTypeInfo{},
		files: map[string]*ast.File{},
		fset:  token.NewFileSet(),
		dupes: map[string][]string{},
	}
	// pkgmove:scan-covers pkg/hub/...
	// recursive walk of pkg/hub, test files included: covers every directory below it.
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && (d.Name() == "testdata" || strings.HasPrefix(d.Name(), ".") || strings.HasPrefix(d.Name(), "_")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		f, err := parser.ParseFile(s.fset, path, nil, 0)
		if err != nil {
			return err
		}
		s.files[rel] = f
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Recv == nil || len(d.Recv.List) != 1 {
					continue
				}
				if recv := attestationEmbeddedName(d.Recv.List[0].Type); recv != "" {
					s.typeInfo(recv).methods[d.Name.Name] = true
				}
			}
		}
		// Every type declaration, including types local to a function body
		// (which cannot declare methods but can embed an attested type).
		ast.Inspect(f, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok {
				return true
			}
			ti := s.typeInfo(ts.Name.Name)
			if ti.file != "" {
				s.dupes[ts.Name.Name] = append(s.dupes[ts.Name.Name], ti.file, rel)
			}
			ti.file = rel
			if st, ok := ts.Type.(*ast.StructType); ok && st.Fields != nil {
				for _, field := range st.Fields.List {
					if len(field.Names) == 0 {
						if name := attestationEmbeddedName(field.Type); name != "" {
							ti.embeds = append(ti.embeds, name)
						}
					}
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("scanning pkg/hub sources: %v", err)
	}
	return s
}

// isAncestryMarkerMethod reports whether name looks like an ancestry
// provenance marker method, in any capitalisation.
func isAncestryMarkerMethod(name string) bool {
	return strings.Contains(strings.ToLower(name), "ancestryprovenance")
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestAncestryAttestationGuard(t *testing.T) {
	s := scanAttestationSources(t)

	switchTypes := map[string]bool{}
	for _, name := range ancestryAttestedPrincipalTypes {
		switchTypes[strings.TrimPrefix(name, "*")] = true
	}
	attested := map[string]bool{}
	for name := range switchTypes {
		attested[name] = true
	}
	for name := range ancestryMarkerTypes {
		attested[name] = true
	}

	t.Run("NoDuplicateTypeNames", func(t *testing.T) {
		// The scan keys types by name across pkg/hub/...; a second
		// declaration of an attested name in another package would make it
		// ambiguous.
		for name, files := range s.dupes {
			if attested[name] || userIdentityImplementations[name] != "" {
				t.Errorf("type %s is declared more than once under pkg/hub (%v); the attestation guard keys types by name", name, files)
			}
		}
	})

	t.Run("MarkerImplementations", func(t *testing.T) {
		got := map[string]bool{}
		for name, ti := range s.types {
			for m := range ti.methods {
				if isAncestryMarkerMethod(m) {
					got[name] = true
				}
			}
		}
		if g, w := sortedKeys(got), sortedKeys(ancestryMarkerTypes); strings.Join(g, ",") != strings.Join(w, ",") {
			t.Errorf("types declaring an ancestry-provenance method:\n got  %v\n want %v\na new marker implementation attests a type; classify it in ancestryMarkerTypes (and TestAncestryIsHubAttested_EquivalenceTable) deliberately", g, w)
		}
		for name := range switchTypes {
			if got[name] {
				t.Errorf("%s is attested by AncestryIsHubAttested's type switch and must not also declare a marker method", name)
			}
		}
		for name, wantTest := range ancestryMarkerTypes {
			ti := s.types[name]
			if ti == nil || ti.file == "" {
				continue // reported by DeclaredUnderHub
			}
			if isTest := strings.HasSuffix(ti.file, "_test.go"); isTest != wantTest {
				t.Errorf("%s declared in %s: test file = %v, want %v", name, ti.file, isTest, wantTest)
			}
			if strings.Contains(ti.file, "/") {
				t.Errorf("marker type %s is declared in %s; the unexported marker can only be implemented in package hub", name, ti.file)
			}
		}
	})

	t.Run("UserIdentityImplementations", func(t *testing.T) {
		got := map[string]bool{}
		for name, ti := range s.types {
			if ti.methods["Email"] && ti.methods["DisplayName"] && ti.methods["Role"] {
				got[name] = true
			}
			for _, e := range ti.embeds {
				if e == "UserIdentity" {
					got[name] = true
				}
			}
		}
		if g, w := sortedKeys(got), sortedKeys(userIdentityImplementations); strings.Join(g, ",") != strings.Join(w, ",") {
			t.Errorf("UserIdentity implementations:\n got  %v\n want %v\na new UserIdentity implementation needs a deliberate attestation decision in userIdentityImplementations (switch case, marker, or not attested)", g, w)
		}
		for name, outcome := range userIdentityImplementations {
			ti := s.types[name]
			if ti == nil {
				continue
			}
			marker := false
			for m := range ti.methods {
				marker = marker || isAncestryMarkerMethod(m)
			}
			federated := ti.methods["IssuerURL"]
			var want string
			switch {
			case federated:
				want = "federated"
			case switchTypes[name]:
				want = "switch"
			case marker:
				want = "marker"
			default:
				want = "not attested"
			}
			if outcome != want {
				t.Errorf("%s: recorded as %q, source says %q", name, outcome, want)
			}
		}
	})

	t.Run("DeclaredUnderHub", func(t *testing.T) {
		for _, name := range sortedKeys(attested) {
			ti := s.types[name]
			if ti == nil || ti.file == "" {
				t.Errorf("attested type %s is no longer declared under pkg/hub/...; attestation must stay decided by hub-side code over hub-side types", name)
				continue
			}
			if switchTypes[name] && strings.HasSuffix(ti.file, "_test.go") {
				t.Errorf("switch-attested type %s is declared in test file %s", name, ti.file)
			}
		}
	})

	t.Run("NoEmbeddingOfAttestedTypes", func(t *testing.T) {
		for _, outer := range sortedKeys(s.types) {
			for _, inner := range s.types[outer].embeds {
				if !attested[inner] {
					continue
				}
				key := outer + " embeds " + inner
				if !ancestryEmbeddingExceptions[key] {
					t.Errorf("%s (%s): embedding an attested type changes attestation silently (a promoted marker attests the outer type; the type switch does not match it); record it in ancestryEmbeddingExceptions deliberately", key, s.types[outer].file)
				}
			}
		}
		for key := range ancestryEmbeddingExceptions {
			parts := strings.SplitN(key, " embeds ", 2)
			found := false
			if ti := s.types[parts[0]]; ti != nil {
				for _, e := range ti.embeds {
					found = found || e == parts[1]
				}
			}
			if !found {
				t.Errorf("stale ancestryEmbeddingExceptions entry %q", key)
			}
		}
	})

	t.Run("SingleMarkerAssertionSite", func(t *testing.T) {
		for _, rel := range sortedKeys(s.files) {
			f := s.files[rel]
			var stack []ast.Node
			ast.Inspect(f, func(n ast.Node) bool {
				if n == nil {
					stack = stack[:len(stack)-1]
					return true
				}
				stack = append(stack, n)
				switch x := n.(type) {
				case *ast.Ident:
					if x.Name != "localAncestryProvenanceIdentity" {
						return true
					}
					if rel != ancestryAttestationFile {
						t.Errorf("%s names localAncestryProvenanceIdentity; only %s may", s.fset.Position(x.Pos()), ancestryAttestationFile)
						return true
					}
					// Allowed: the declaration, and the uses inside
					// AncestryIsHubAttested.
					if len(stack) >= 2 {
						if _, ok := stack[len(stack)-2].(*ast.TypeSpec); ok {
							return true
						}
					}
					inFunc := ""
					for _, anc := range stack {
						if fd, ok := anc.(*ast.FuncDecl); ok {
							inFunc = fd.Name.Name
						}
					}
					if inFunc != "AncestryIsHubAttested" {
						t.Errorf("%s names localAncestryProvenanceIdentity outside AncestryIsHubAttested", s.fset.Position(x.Pos()))
					}
				case *ast.InterfaceType:
					// An anonymous interface with the marker method is
					// another way to assert it.
					if x.Methods == nil {
						return true
					}
					for _, m := range x.Methods.List {
						for _, mn := range m.Names {
							if !isAncestryMarkerMethod(mn.Name) {
								continue
							}
							parentIsDecl := false
							if len(stack) >= 2 {
								if ts, ok := stack[len(stack)-2].(*ast.TypeSpec); ok && ts.Name.Name == "localAncestryProvenanceIdentity" && rel == ancestryAttestationFile {
									parentIsDecl = true
								}
							}
							if !parentIsDecl {
								t.Errorf("%s declares an interface with an ancestry-provenance method; only localAncestryProvenanceIdentity in %s may", s.fset.Position(mn.Pos()), ancestryAttestationFile)
							}
						}
					}
				}
				return true
			})
		}
	})

	t.Run("SwitchCasesMatch", func(t *testing.T) {
		f := s.files[ancestryAttestationFile]
		if f == nil {
			t.Fatalf("%s not found", ancestryAttestationFile)
		}
		var fn *ast.FuncDecl
		for _, decl := range f.Decls {
			if fd, ok := decl.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Name.Name == "AncestryIsHubAttested" {
				fn = fd
			}
		}
		if fn == nil {
			t.Fatalf("AncestryIsHubAttested not found in %s", ancestryAttestationFile)
		}
		var got []string
		switches := 0
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSwitchStmt)
			if !ok {
				return true
			}
			switches++
			for _, stmt := range ts.Body.List {
				cc := stmt.(*ast.CaseClause)
				for _, e := range cc.List {
					star := ""
					if se, ok := e.(*ast.StarExpr); ok {
						star, e = "*", se.X
					}
					got = append(got, star+attestationEmbeddedName(e))
				}
			}
			return true
		})
		if switches != 1 {
			t.Fatalf("AncestryIsHubAttested has %d type switches, want exactly 1", switches)
		}
		sort.Strings(got)
		want := append([]string(nil), ancestryAttestedPrincipalTypes...)
		sort.Strings(want)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("AncestryIsHubAttested type switch cases = %v, want %v (ancestryAttestedPrincipalTypes)", got, want)
		}
	})
}

// principalContextSetterFiles is the allow-list of non-test files under
// pkg/hub that may name each request-context setter of the principal types
// (called, passed as a value, or declared). The setters are what make an
// identity, a request credential or a broker on-behalf-of marker visible to
// authorization, so a new production caller must be a deliberate edit here.
// Test files are exempt: they build request contexts directly. Keys are
// lower-camel setter names; references are matched with either first-letter
// case, so the exported names of the principal package count too.
var principalContextSetterFiles = map[string]map[string]bool{
	"contextWithIdentity": {
		// Declaration: identity.go before the principal types move,
		// internal/principal/identity.go and the hub alias file after.
		"identity.go": true, "internal/principal/identity.go": true, "zz_alias_principal.go": true,
		// Authentication middleware.
		"auth.go": true, "auth_external_bearer.go": true, "brokerauth.go": true,
		// In-process identity substitution for deferred or internal work.
		"authz.go": true, "capabilities.go": true, "chat_scheduled_send.go": true,
		"pty_handlers.go": true, "server.go": true,
	},
	"contextWithCredentialContext": {
		"identity.go": true, "internal/principal/identity.go": true, "zz_alias_principal.go": true,
		"auth.go": true, "auth_external_bearer.go": true, "brokerauth.go": true,
	},
	"contextWithBrokerOnBehalfOf": {
		"identity.go": true, "internal/principal/identity.go": true, "zz_alias_principal.go": true,
		"brokerauth.go": true,
	},
}

// principalSetterKey maps an identifier to its principalContextSetterFiles
// key, accepting either first-letter case.
func principalSetterKey(name string) string {
	if name == "" {
		return ""
	}
	key := strings.ToLower(name[:1]) + name[1:]
	if _, ok := principalContextSetterFiles[key]; ok {
		return key
	}
	return ""
}

func TestPrincipalContextSetterFileAllowList(t *testing.T) {
	s := scanAttestationSources(t)
	found := map[string]int{}
	for _, rel := range sortedKeys(s.files) {
		if strings.HasSuffix(rel, "_test.go") {
			continue
		}
		ast.Inspect(s.files[rel], func(n ast.Node) bool {
			id, ok := n.(*ast.Ident)
			if !ok {
				return true
			}
			key := principalSetterKey(id.Name)
			if key == "" {
				return true
			}
			found[key]++
			if !principalContextSetterFiles[key][rel] {
				t.Errorf("%s names %s; only the files in principalContextSetterFiles[%q] may", s.fset.Position(id.Pos()), id.Name, key)
			}
			return true
		})
	}
	for key := range principalContextSetterFiles {
		if found[key] == 0 {
			t.Errorf("found no references to %s: scanner broken", key)
		}
	}
}
