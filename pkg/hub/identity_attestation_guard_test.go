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
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Ancestry attestation is decided in package hub by AncestryIsHubAttested
// (identity_attestation.go), in two ways:
//
//   - the local user types declared with the principal types are listed by
//     exact pointer type in a type switch (switchTypes below);
//   - every other attested type opts in through the unexported
//     localAncestryProvenanceIdentity marker, which only a type declared in
//     package hub can implement (markerTypes), or acquires it by embedding a
//     marker holder (promotedMarkerTypes).
//
// The switch replaces the compile-time guarantee the marker method gives, so
// TestAncestryAttestationGuard pins both lists against the source of
// pkg/hub/... (production and test files): a new marker implementation, a new
// UserIdentity implementation, a new type embedding an attested type
// (directly or transitively), a new assertion of the marker, a change to
// AncestryIsHubAttested's checks or their order, or an attested type whose
// declaration leaves pkg/hub/... fails it until the expectations below are
// updated as a deliberate edit. TestAncestryAttestationGuard_SelfTest runs
// each check against fixtures that must trip it, so a check cannot silently
// become a no-op.

// attestationExpectations is what the attestation guard checks a source tree
// against.
type attestationExpectations struct {
	// attestationFile is the file, relative to the scanned root, that
	// declares the marker interface and AncestryIsHubAttested.
	attestationFile string
	// switchTypes is the exact set of case types in AncestryIsHubAttested's
	// type switch. These types carry no marker method.
	switchTypes []string
	// markerTypes is the exact set of types that declare an
	// ancestry-provenance marker method, with whether the declaring file is a
	// test file.
	markerTypes map[string]bool
	// promotedMarkerTypes is the exact set of types attested through a
	// promoted marker (they embed a marker holder, directly or through
	// another such type), mapped to the attested type they embed.
	promotedMarkerTypes map[string]string
	// userIdentity is every type that implements UserIdentity as far as an
	// AST scan can tell (it declares Email, DisplayName and Role, or embeds
	// UserIdentity), with how AncestryIsHubAttested treats it: "switch",
	// "marker", "federated" or "not attested". A new UserIdentity
	// implementation must be classified here, and given a switch case or a
	// marker if it is a local user whose ancestry the hub vouches for.
	userIdentity map[string]string
}

var hubAttestationExpectations = attestationExpectations{
	attestationFile: "identity_attestation.go",
	switchTypes:     []string{"*AuthenticatedUser", "*ScopedUserIdentity"},
	markerTypes: map[string]bool{
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
	},
	promotedMarkerTypes: map[string]string{
		// Test types, both pinned in TestAncestryIsHubAttested_EquivalenceTable.
		// A *DevUser-like wrapper (e2b_initiator_attribution_test.go).
		"devUserEmbeddingWrapper": "DevUser",
		// A wake DM sender with token scopes (wake lifecycle tests).
		"wakeLifecycleAgentIdentity": "wakeDMTestIdentity",
	},
	userIdentity: map[string]string{
		"AuthenticatedUser":     "switch",
		"ScopedUserIdentity":    "switch",
		"DevUser":               "marker",
		"FederatedUserIdentity": "federated",
		// Test fakes.
		"bindableFederatedAdmin":  "federated",
		"bypassAgentsDevIdentity": "not attested",
		"devTypedUserIdentity":    "not attested",
		"federatedTestIdentity":   "not attested",
	},
}

// ancestryAttestedPrincipalTypes is the exact set of case types in
// AncestryIsHubAttested's type switch (see hubAttestationExpectations).
var ancestryAttestedPrincipalTypes = hubAttestationExpectations.switchTypes

// attestationTypeInfo is the AST-level description of one type name.
type attestationTypeInfo struct {
	// decls are the files (relative to the scanned root, slash-separated)
	// holding a defining declaration of the name: `type T ...`, not an alias
	// `type T = ...`. An alias, such as the ones a pkgmove alias file
	// declares, is recorded in aliases instead.
	decls   []string
	aliases []string
	methods map[string]bool
	embeds  []string // unqualified names of embedded fields
}

type attestationScan struct {
	types map[string]*attestationTypeInfo
	files map[string]*ast.File
	fset  *token.FileSet
}

func (s *attestationScan) typeInfo(name string) *attestationTypeInfo {
	ti := s.types[name]
	if ti == nil {
		ti = &attestationTypeInfo{methods: map[string]bool{}}
		s.types[name] = ti
	}
	return ti
}

// attestationEmbeddedName returns the unqualified type name of a type
// expression: T, *T, pkg.T, *pkg.T, T[K] and T[K, V] all give "T".
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
	case *ast.IndexListExpr:
		return attestationEmbeddedName(t.X)
	default:
		return ""
	}
}

// scanAttestationSources scans pkg/hub, recursively.
func scanAttestationSources(t *testing.T) *attestationScan {
	t.Helper()
	s, err := scanAttestationSourcesIn(findHubDir(t))
	if err != nil {
		t.Fatalf("scanning pkg/hub sources: %v", err)
	}
	return s
}

// scanAttestationSourcesIn parses every .go file under root, recursively,
// test files included, and records declared types, their methods and their
// embedded fields.
func scanAttestationSourcesIn(root string) (*attestationScan, error) {
	s := &attestationScan{
		types: map[string]*attestationTypeInfo{},
		files: map[string]*ast.File{},
		fset:  token.NewFileSet(),
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
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Recv == nil || len(fd.Recv.List) != 1 {
				continue
			}
			if recv := attestationEmbeddedName(fd.Recv.List[0].Type); recv != "" {
				s.typeInfo(recv).methods[fd.Name.Name] = true
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
			if ts.Assign.IsValid() {
				ti.aliases = append(ti.aliases, rel)
				return true
			}
			ti.decls = append(ti.decls, rel)
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
	return s, err
}

// isAncestryMarkerMethod reports whether name looks like an ancestry
// provenance marker method, in any capitalisation.
func isAncestryMarkerMethod(name string) bool {
	return strings.Contains(strings.ToLower(name), "ancestryprovenance")
}

func attestationSortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (e attestationExpectations) switchTypeNames() map[string]bool {
	out := map[string]bool{}
	for _, name := range e.switchTypes {
		out[strings.TrimPrefix(name, "*")] = true
	}
	return out
}

func (s *attestationScan) markerHolders() map[string]bool {
	got := map[string]bool{}
	for name, ti := range s.types {
		for m := range ti.methods {
			if isAncestryMarkerMethod(m) {
				got[name] = true
			}
		}
	}
	return got
}

// promotedMarkerHolders returns the types that acquire a marker method by
// embedding a marker holder, directly or through another such type (a
// fixpoint over embedded fields), mapped to one attested type they embed.
func (s *attestationScan) promotedMarkerHolders() map[string]string {
	attested := s.markerHolders()
	promoted := map[string]string{}
	for changed := true; changed; {
		changed = false
		for name, ti := range s.types {
			if attested[name] {
				continue
			}
			for _, e := range ti.embeds {
				if attested[e] {
					attested[name] = true
					promoted[name] = e
					changed = true
					break
				}
			}
		}
	}
	return promoted
}

// checkAttestationNoDuplicateTypeNames: the scan keys types by name across
// the tree; a second defining declaration of an attested or UserIdentity
// name in another package would make it ambiguous. Aliases do not count.
func checkAttestationNoDuplicateTypeNames(s *attestationScan, e attestationExpectations) []string {
	relevant := e.switchTypeNames()
	for name := range e.markerTypes {
		relevant[name] = true
	}
	for name := range e.promotedMarkerTypes {
		relevant[name] = true
	}
	for name := range e.userIdentity {
		relevant[name] = true
	}
	var problems []string
	for _, name := range attestationSortedKeys(relevant) {
		if ti := s.types[name]; ti != nil && len(ti.decls) > 1 {
			problems = append(problems, fmt.Sprintf("type %s is declared more than once (%v); the attestation guard keys types by name", name, ti.decls))
		}
	}
	return problems
}

func checkAttestationMarkerImplementations(s *attestationScan, e attestationExpectations) []string {
	var problems []string
	got := s.markerHolders()
	if g, w := attestationSortedKeys(got), attestationSortedKeys(e.markerTypes); strings.Join(g, ",") != strings.Join(w, ",") {
		problems = append(problems, fmt.Sprintf("types declaring an ancestry-provenance method:\n got  %v\n want %v\na new marker implementation attests a type; classify it deliberately (and pin it in TestAncestryIsHubAttested_EquivalenceTable)", g, w))
	}
	for name := range e.switchTypeNames() {
		if got[name] {
			problems = append(problems, fmt.Sprintf("%s is attested by AncestryIsHubAttested's type switch and must not also declare a marker method", name))
		}
	}
	for _, name := range attestationSortedKeys(e.markerTypes) {
		ti := s.types[name]
		if ti == nil || len(ti.decls) != 1 {
			continue // reported by DeclaredUnderHub or NoDuplicateTypeNames
		}
		file := ti.decls[0]
		if isTest := strings.HasSuffix(file, "_test.go"); isTest != e.markerTypes[name] {
			problems = append(problems, fmt.Sprintf("%s declared in %s: test file = %v, want %v", name, file, isTest, e.markerTypes[name]))
		}
		if strings.Contains(file, "/") {
			problems = append(problems, fmt.Sprintf("marker type %s is declared in %s; the unexported marker can only be implemented in package hub", name, file))
		}
	}
	return problems
}

func checkAttestationUserIdentityImplementations(s *attestationScan, e attestationExpectations) []string {
	var problems []string
	got := map[string]bool{}
	for name, ti := range s.types {
		if ti.methods["Email"] && ti.methods["DisplayName"] && ti.methods["Role"] {
			got[name] = true
		}
		for _, emb := range ti.embeds {
			if emb == "UserIdentity" {
				got[name] = true
			}
		}
	}
	if g, w := attestationSortedKeys(got), attestationSortedKeys(e.userIdentity); strings.Join(g, ",") != strings.Join(w, ",") {
		problems = append(problems, fmt.Sprintf("UserIdentity implementations:\n got  %v\n want %v\na new UserIdentity implementation needs a deliberate attestation decision (switch case, marker, or not attested)", g, w))
	}
	switchTypes := e.switchTypeNames()
	markers := s.markerHolders()
	for _, name := range attestationSortedKeys(e.userIdentity) {
		ti := s.types[name]
		if ti == nil {
			continue
		}
		var want string
		switch {
		case ti.methods["IssuerURL"]:
			want = "federated"
		case switchTypes[name]:
			want = "switch"
		case markers[name]:
			want = "marker"
		default:
			want = "not attested"
		}
		if e.userIdentity[name] != want {
			problems = append(problems, fmt.Sprintf("%s: recorded as %q, source says %q", name, e.userIdentity[name], want))
		}
	}
	return problems
}

// checkAttestationDeclaredUnderHub: every attested type must have a defining
// declaration in the scanned tree. An alias does not count, so a type whose
// declaration moves out of pkg/hub/... leaving only an alias behind fails.
func checkAttestationDeclaredUnderHub(s *attestationScan, e attestationExpectations) []string {
	var problems []string
	attested := e.switchTypeNames()
	for name := range e.markerTypes {
		attested[name] = true
	}
	for name := range e.promotedMarkerTypes {
		attested[name] = true
	}
	switchTypes := e.switchTypeNames()
	for _, name := range attestationSortedKeys(attested) {
		ti := s.types[name]
		if ti == nil || len(ti.decls) == 0 {
			problems = append(problems, fmt.Sprintf("attested type %s has no defining declaration under pkg/hub/... (aliases do not count); attestation must stay decided by hub-side code over hub-side types", name))
			continue
		}
		if switchTypes[name] {
			for _, file := range ti.decls {
				if strings.HasSuffix(file, "_test.go") {
					problems = append(problems, fmt.Sprintf("switch-attested type %s is declared in test file %s", name, file))
				}
			}
		}
	}
	return problems
}

// checkAttestationEmbedding: embedding a marker holder promotes the marker
// and attests the outer type, transitively; embedding a switch type does not
// attest it (the switch matches exact types). Either changes attestation
// silently, so both are pinned.
func checkAttestationEmbedding(s *attestationScan, e attestationExpectations) []string {
	var problems []string
	got := s.promotedMarkerHolders()
	if g, w := attestationSortedKeys(got), attestationSortedKeys(e.promotedMarkerTypes); strings.Join(g, ",") != strings.Join(w, ",") {
		problems = append(problems, fmt.Sprintf("types attested through a promoted marker (embedding a marker holder, directly or transitively):\n got  %v\n want %v\nrecord a new one deliberately (and pin it in TestAncestryIsHubAttested_EquivalenceTable)", g, w))
	}
	for _, outer := range attestationSortedKeys(e.promotedMarkerTypes) {
		ti := s.types[outer]
		if ti == nil {
			continue
		}
		found := false
		for _, emb := range ti.embeds {
			found = found || emb == e.promotedMarkerTypes[outer]
		}
		if !found {
			problems = append(problems, fmt.Sprintf("%s no longer embeds %s", outer, e.promotedMarkerTypes[outer]))
		}
	}
	switchTypes := e.switchTypeNames()
	for _, outer := range attestationSortedKeys(s.types) {
		for _, emb := range s.types[outer].embeds {
			if switchTypes[emb] {
				problems = append(problems, fmt.Sprintf("%s (%v) embeds switch-attested type %s: the type switch does not attest the outer type, so this silently changes attestation", outer, s.types[outer].decls, emb))
			}
		}
	}
	return problems
}

// checkAttestationSingleMarkerAssertionSite: only AncestryIsHubAttested may
// name or assert the marker interface, and no other interface may declare a
// marker method (an anonymous interface assertion would be a second site).
func checkAttestationSingleMarkerAssertionSite(s *attestationScan, e attestationExpectations) []string {
	var problems []string
	for _, rel := range attestationSortedKeys(s.files) {
		var stack []ast.Node
		ast.Inspect(s.files[rel], func(n ast.Node) bool {
			if n == nil {
				stack = stack[:len(stack)-1]
				return true
			}
			stack = append(stack, n)
			parentIsMarkerDecl := func() bool {
				if len(stack) < 2 || rel != e.attestationFile {
					return false
				}
				ts, ok := stack[len(stack)-2].(*ast.TypeSpec)
				return ok && ts.Name.Name == "localAncestryProvenanceIdentity"
			}
			switch x := n.(type) {
			case *ast.Ident:
				if x.Name != "localAncestryProvenanceIdentity" {
					return true
				}
				if rel != e.attestationFile {
					problems = append(problems, fmt.Sprintf("%s names localAncestryProvenanceIdentity; only %s may", s.fset.Position(x.Pos()), e.attestationFile))
					return true
				}
				if parentIsMarkerDecl() {
					return true
				}
				inFunc := ""
				for _, anc := range stack {
					if fd, ok := anc.(*ast.FuncDecl); ok {
						inFunc = fd.Name.Name
					}
				}
				if inFunc != "AncestryIsHubAttested" {
					problems = append(problems, fmt.Sprintf("%s names localAncestryProvenanceIdentity outside AncestryIsHubAttested", s.fset.Position(x.Pos())))
				}
			case *ast.InterfaceType:
				if x.Methods == nil {
					return true
				}
				for _, m := range x.Methods.List {
					for _, mn := range m.Names {
						if isAncestryMarkerMethod(mn.Name) && !parentIsMarkerDecl() {
							problems = append(problems, fmt.Sprintf("%s declares an interface with an ancestry-provenance method; only localAncestryProvenanceIdentity in %s may", s.fset.Position(mn.Pos()), e.attestationFile))
						}
					}
				}
			}
			return true
		})
	}
	return problems
}

// expectedAttestationBody is AncestryIsHubAttested's normalized body: its
// calls, type assertions and type switch, in source order. The order is the
// security property: nil first, then the federated rejection, then the
// scoped-wraps-federated unwrap, all before any allow path.
func (e attestationExpectations) expectedAttestationBody() []string {
	return []string{
		"call isNilIdentity",
		"assert FederatedIdentity",
		"assert *ScopedUserIdentity",
		"assert FederatedIdentity",
		"switch " + strings.Join(e.switchTypes, ","),
		"assert localAncestryProvenanceIdentity",
	}
}

func attestationTypeString(expr ast.Expr) string {
	if se, ok := expr.(*ast.StarExpr); ok {
		return "*" + attestationEmbeddedName(se.X)
	}
	return attestationEmbeddedName(expr)
}

// checkAttestationBody pins AncestryIsHubAttested's normalized body (see
// expectedAttestationBody), including the exact type switch cases.
func checkAttestationBody(s *attestationScan, e attestationExpectations) []string {
	f := s.files[e.attestationFile]
	if f == nil {
		return []string{e.attestationFile + " not found"}
	}
	var fn *ast.FuncDecl
	for _, decl := range f.Decls {
		if fd, ok := decl.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Name.Name == "AncestryIsHubAttested" {
			fn = fd
		}
	}
	if fn == nil || fn.Body == nil {
		return []string{"AncestryIsHubAttested not found in " + e.attestationFile}
	}
	var got []string
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			got = append(got, "call "+attestationEmbeddedName(x.Fun))
		case *ast.TypeAssertExpr:
			if x.Type != nil { // x.(type) in a type switch has no Type
				got = append(got, "assert "+attestationTypeString(x.Type))
			}
		case *ast.TypeSwitchStmt:
			var cases []string
			for _, stmt := range x.Body.List {
				for _, c := range stmt.(*ast.CaseClause).List {
					cases = append(cases, attestationTypeString(c))
				}
			}
			got = append(got, "switch "+strings.Join(cases, ","))
		}
		return true
	})
	want := e.expectedAttestationBody()
	if strings.Join(got, "; ") != strings.Join(want, "; ") {
		return []string{fmt.Sprintf("AncestryIsHubAttested's checks changed:\n got  %v\n want %v", got, want)}
	}
	return nil
}

var attestationChecks = []struct {
	name  string
	check func(*attestationScan, attestationExpectations) []string
}{
	{"NoDuplicateTypeNames", checkAttestationNoDuplicateTypeNames},
	{"MarkerImplementations", checkAttestationMarkerImplementations},
	{"UserIdentityImplementations", checkAttestationUserIdentityImplementations},
	{"DeclaredUnderHub", checkAttestationDeclaredUnderHub},
	{"Embedding", checkAttestationEmbedding},
	{"SingleMarkerAssertionSite", checkAttestationSingleMarkerAssertionSite},
	{"AttestationBody", checkAttestationBody},
}

func TestAncestryAttestationGuard(t *testing.T) {
	s := scanAttestationSources(t)
	for _, c := range attestationChecks {
		t.Run(c.name, func(t *testing.T) {
			for _, p := range c.check(s, hubAttestationExpectations) {
				t.Error(p)
			}
		})
	}
}

// TestAncestryAttestationGuard_SelfTest runs every attestation check against
// a small fixture tree that passes all of them, then against mutations of it
// that must each trip a named check.
func TestAncestryAttestationGuard_SelfTest(t *testing.T) {
	expect := attestationExpectations{
		attestationFile:     "attest.go",
		switchTypes:         []string{"*User"},
		markerTypes:         map[string]bool{"Agent": false},
		promotedMarkerTypes: map[string]string{"AgentWrap": "Agent"},
		userIdentity:        map[string]string{"User": "switch"},
	}
	base := map[string]string{
		"types.go": `package p
type User struct{}
func (u *User) Email() string       { return "" }
func (u *User) DisplayName() string { return "" }
func (u *User) Role() string        { return "" }
type Agent struct{}
func (a *Agent) localAncestryProvenance() ancestryProvenance { return "" }
type AgentWrap struct{ *Agent }
`,
		"attest.go": `package p
type ancestryProvenance string
type localAncestryProvenanceIdentity interface {
	localAncestryProvenance() ancestryProvenance
}
func AncestryIsHubAttested(identity Identity) bool {
	if isNilIdentity(identity) {
		return false
	}
	if _, isFederated := identity.(FederatedIdentity); isFederated {
		return false
	}
	if scoped, ok := identity.(*ScopedUserIdentity); ok {
		if _, f := scoped.UserIdentity.(FederatedIdentity); f {
			return false
		}
	}
	switch identity.(type) {
	case *User:
		return true
	}
	_, ok := identity.(localAncestryProvenanceIdentity)
	return ok
}
`,
	}
	run := func(t *testing.T, files map[string]string) map[string][]string {
		t.Helper()
		root := t.TempDir()
		for name, src := range files {
			path := filepath.Join(root, filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		s, err := scanAttestationSourcesIn(root)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string][]string{}
		for _, c := range attestationChecks {
			if p := c.check(s, expect); len(p) > 0 {
				out[c.name] = p
			}
		}
		return out
	}
	with := func(edits map[string]string) map[string]string {
		out := map[string]string{}
		for k, v := range base {
			out[k] = v
		}
		for k, v := range edits {
			if v == "" {
				delete(out, k)
			} else {
				out[k] = v
			}
		}
		return out
	}

	t.Run("BaselinePasses", func(t *testing.T) {
		if got := run(t, base); len(got) > 0 {
			t.Fatalf("baseline fixture fails checks: %v", got)
		}
	})
	t.Run("AliasIsNotADeclaration", func(t *testing.T) {
		// An alias of a switch type in another directory (what a pkgmove
		// alias file looks like from the target's side) is neither a
		// duplicate nor a declaration.
		got := run(t, with(map[string]string{"sub/alias.go": "package sub\ntype User = p.User\ntype Agent = p.Agent\n"}))
		if len(got) > 0 {
			t.Fatalf("an alias tripped checks: %v", got)
		}
	})

	mutations := []struct {
		name      string
		edits     map[string]string
		wantCheck string
	}{
		{
			name:      "declaration leaves the tree, alias stays",
			edits:     map[string]string{"types.go": strings.Replace(base["types.go"], "type User struct{}", "type User = other.User", 1)},
			wantCheck: "DeclaredUnderHub",
		},
		{
			name:      "second defining declaration",
			edits:     map[string]string{"sub/dup.go": "package sub\ntype User struct{}\n"},
			wantCheck: "NoDuplicateTypeNames",
		},
		{
			name:      "new marker implementation",
			edits:     map[string]string{"new.go": "package p\ntype Other struct{}\nfunc (o *Other) localAncestryProvenance() ancestryProvenance { return \"\" }\n"},
			wantCheck: "MarkerImplementations",
		},
		{
			name:      "new marker implementation on a generic type with two parameters",
			edits:     map[string]string{"new.go": "package p\ntype G[K comparable, V any] struct{}\nfunc (g *G[K, V]) localAncestryProvenance() ancestryProvenance { return \"\" }\n"},
			wantCheck: "MarkerImplementations",
		},
		{
			name:      "marker on a switch type",
			edits:     map[string]string{"new.go": "package p\nfunc (u *User) localAncestryProvenance() ancestryProvenance { return \"\" }\n"},
			wantCheck: "MarkerImplementations",
		},
		{
			name:      "new UserIdentity implementation",
			edits:     map[string]string{"new.go": "package p\ntype Look struct{ UserIdentity }\n"},
			wantCheck: "UserIdentityImplementations",
		},
		{
			name:      "new type embedding a marker holder",
			edits:     map[string]string{"new.go": "package p\ntype Wrap struct{ Agent }\n"},
			wantCheck: "Embedding",
		},
		{
			name:      "new type embedding a promoted-marker type (transitive)",
			edits:     map[string]string{"new.go": "package p\ntype Wrap2 struct{ *AgentWrap }\n"},
			wantCheck: "Embedding",
		},
		{
			name:      "type local to a function embedding a marker holder",
			edits:     map[string]string{"new.go": "package p\nfunc f() { type local struct{ *Agent }; _ = local{} }\n"},
			wantCheck: "Embedding",
		},
		{
			name:      "new type embedding a switch type",
			edits:     map[string]string{"new.go": "package p\ntype UWrap struct{ *User }\n"},
			wantCheck: "Embedding",
		},
		{
			name:      "marker interface named in another file",
			edits:     map[string]string{"new.go": "package p\nvar _ localAncestryProvenanceIdentity\n"},
			wantCheck: "SingleMarkerAssertionSite",
		},
		{
			name:      "anonymous interface with a marker method",
			edits:     map[string]string{"new.go": "package p\nfunc g(i Identity) bool { _, ok := i.(interface{ localAncestryProvenance() ancestryProvenance }); return ok }\n"},
			wantCheck: "SingleMarkerAssertionSite",
		},
		{
			name: "allow path moved before the federated rejection",
			edits: map[string]string{"attest.go": strings.Replace(base["attest.go"],
				"\tif _, isFederated := identity.(FederatedIdentity); isFederated {\n\t\treturn false\n\t}\n",
				"", 1)},
			wantCheck: "AttestationBody",
		},
		{
			name:      "extra switch case",
			edits:     map[string]string{"attest.go": strings.Replace(base["attest.go"], "case *User:", "case *User, *Agent:", 1)},
			wantCheck: "AttestationBody",
		},
		{
			name:      "value case instead of pointer case",
			edits:     map[string]string{"attest.go": strings.Replace(base["attest.go"], "case *User:", "case User:", 1)},
			wantCheck: "AttestationBody",
		},
	}
	for _, m := range mutations {
		t.Run(m.name, func(t *testing.T) {
			got := run(t, with(m.edits))
			if len(got[m.wantCheck]) == 0 {
				t.Fatalf("check %s did not fire (fired: %v)", m.wantCheck, got)
			}
		})
	}
}

// principalContextSetterFiles is the allow-list of non-test files under
// pkg/hub that may name each request-context setter of the principal types
// (called, passed as a value, or declared). The setters are what make an
// identity, a request credential or a broker on-behalf-of marker visible to
// authorization, so a new production caller must be a deliberate edit here.
// Test files are exempt: they build request contexts directly. Keys are
// lower-camel setter names; references are matched with either first-letter
// case, so the exported names of the principal package count too.
//
// The check matches identifiers only. A func value of a setter taken in an
// allowed file (f := contextWithIdentity) and passed elsewhere escapes it:
// the call through f, in another file, names no setter. Review a setter used
// as a value in an allowed file accordingly.
var principalContextSetterFiles = map[string]map[string]bool{
	"contextWithIdentity": {
		// Declaration and the hub alias.
		"internal/principal/identity.go": true, "zz_alias_principal.go": true,
		// Authentication middleware.
		"auth.go": true, "auth_external_bearer.go": true, "brokerauth.go": true,
		// In-process identity substitution for deferred or internal work.
		"authz.go": true, "capabilities.go": true, "chat_scheduled_send.go": true,
		"pty_handlers.go": true, "server.go": true,
	},
	"contextWithCredentialContext": {
		"internal/principal/identity.go": true, "zz_alias_principal.go": true,
		"auth.go": true, "auth_external_bearer.go": true, "brokerauth.go": true,
	},
	"contextWithBrokerOnBehalfOf": {
		"internal/principal/identity.go": true, "zz_alias_principal.go": true,
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
	for _, rel := range attestationSortedKeys(s.files) {
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
