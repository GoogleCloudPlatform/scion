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

package secret

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

// moduleRoot is the repository root, two levels above pkg/secret.
func moduleRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root not found at %s: %v", root, err)
	}
	return root
}

// selectorCallSites returns "<file>:<enclosing func>" for every reference
// to a method or function named name in the non-test Go files of the
// module: any selector x.name (a call, a method value or a method
// expression, inside a function or a package-level func literal) and any
// direct call name(...). extras (separate modules) and web are excluded.
func selectorCallSites(t *testing.T, name string) []string {
	t.Helper()
	return selectorCallSitesByName(t, name)[name]
}

// selectorCallSitesByName is selectorCallSites for several names in one
// walk of the module, keyed by name.
func selectorCallSitesByName(t *testing.T, names ...string) map[string][]string {
	t.Helper()
	root := moduleRoot(t)
	sites := map[string][]string{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if d.IsDir() {
			switch rel {
			case ".git", "extras", "web", "node_modules", "docs-site", ".scion":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		for _, name := range names {
			sites[name] = append(sites[name], referenceSites(f, filepath.ToSlash(rel), name)...)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		sort.Strings(sites[name])
	}
	return sites
}

// decryptForMigrationCallers is the allow-list of non-test call sites of
// ValueCipher.DecryptForMigration ("<file>:<func>"). Plaintext admission is
// migration-only (ptone/scion#1530 §7): only the administrative migration
// reader (phase 6) may call it, never a runtime path. Adding an entry is a
// security-relevant change.
var decryptForMigrationCallers = map[string]bool{}

func TestDecryptForMigrationCallers(t *testing.T) {
	for _, site := range selectorCallSites(t, "DecryptForMigration") {
		if !decryptForMigrationCallers[site] {
			t.Errorf("DecryptForMigration called outside the migration allow-list: %s", site)
		}
	}
}

// The scanner itself must see references: a known caller of a known name.
func TestSelectorCallSitesFindsKnownCaller(t *testing.T) {
	sites := selectorCallSites(t, "decryptPayload")
	found := false
	for _, s := range sites {
		if s == "pkg/secret/localbackend.go:(*LocalBackend).Resolve" {
			found = true
		}
	}
	if !found {
		t.Fatalf("scanner did not find LocalBackend.Resolve calling decryptPayload: %v", sites)
	}
}

// referenceSites returns "<rel>:<enclosing func>" for every reference to
// name in f. Every reference counts, not only calls: a method value, a
// method expression, a bare identifier used as a value (f := name, a
// callback field, an alias) or a use inside a package-level func literal
// reaches the function as well. Declared names are not references: a
// FuncDecl name, a struct field, parameter or result name, a declared
// constant, variable or type name, and a composite-literal key (a field
// written, not read). A method's enclosing name carries its receiver type,
// "(*T).Name" or "T.Name".
func referenceSites(f *ast.File, rel, name string) []string {
	var sites []string
	enclosing := "<package scope>"
	var walk func(n ast.Node) bool
	inspect := func(n ast.Node) {
		if n != nil {
			ast.Inspect(n, walk)
		}
	}
	walk = func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncDecl:
			prev := enclosing
			enclosing = funcDeclName(x)
			if x.Body != nil {
				inspect(x.Body)
			}
			enclosing = prev
			return false
		case *ast.SelectorExpr:
			if x.Sel.Name == name {
				sites = append(sites, rel+":"+enclosing)
			}
			inspect(x.X)
			return false
		case *ast.KeyValueExpr:
			if _, ok := x.Key.(*ast.Ident); !ok {
				inspect(x.Key)
			}
			inspect(x.Value)
			return false
		case *ast.Field:
			inspect(x.Type)
			return false
		case *ast.ValueSpec:
			inspect(x.Type)
			for _, v := range x.Values {
				inspect(v)
			}
			return false
		case *ast.TypeSpec:
			inspect(x.Type)
			return false
		case *ast.Ident:
			if x.Name == name {
				sites = append(sites, rel+":"+enclosing)
			}
		}
		return true
	}
	for _, decl := range f.Decls {
		inspect(decl)
	}
	return sites
}

// funcDeclName is a function's name, or "(*T).Name" / "T.Name" for a
// method (type parameters dropped).
func funcDeclName(d *ast.FuncDecl) string {
	if d.Recv == nil || len(d.Recv.List) == 0 {
		return d.Name.Name
	}
	typ := d.Recv.List[0].Type
	star := ""
	if s, ok := typ.(*ast.StarExpr); ok {
		star, typ = "*", s.X
	}
	switch g := typ.(type) {
	case *ast.IndexExpr:
		typ = g.X
	case *ast.IndexListExpr:
		typ = g.X
	}
	recv := "?"
	if id, ok := typ.(*ast.Ident); ok {
		recv = id.Name
	}
	if star != "" {
		return "(" + star + recv + ")." + d.Name.Name
	}
	return recv + "." + d.Name.Name
}

// The scanner sees every reference form, not only calls.
func TestReferenceSitesFindsEveryForm(t *testing.T) {
	const src = `package p
var pkgLit = func(c *T) { _, _ = c.DecryptForMigration("", nil) }
var methodExpr = (*T).DecryptForMigration
func call(c *T)        { c.DecryptForMigration("", nil) }
func methodValue(c *T) { f := c.DecryptForMigration; _ = f }
func (T) DecryptForMigration(string, any) (string, error) { return "", nil }
func unrelated(c *T)   { c.Decrypt("") }
`
	f, err := parser.ParseFile(token.NewFileSet(), "p.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	got := referenceSites(f, "p.go", "DecryptForMigration")
	want := []string{"p.go:<package scope>", "p.go:<package scope>", "p.go:call", "p.go:methodValue"}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("referenceSites = %v, want %v", got, want)
	}
}

// Bare identifiers used as values count; declared names do not; a
// method's site carries its receiver type.
func TestReferenceSitesBareValuesAndDeclarations(t *testing.T) {
	const src = `package p
func (s *S) bareValue() { g := plainFunc; _ = g }
func (S) valueRecv()    { plainFunc() }
func (g *G[K]) generic() { plainFunc() }
var alias = plainFunc
type cfg struct{ cb func() }
func callback() cfg     { return cfg{cb: plainFunc} }
func plainFunc()        {}
type D struct{ plainFunc int }
func keyed() D          { return D{plainFunc: 1} }
func param(plainFunc int) {}
const plainFuncConst = 1
`
	f, err := parser.ParseFile(token.NewFileSet(), "p.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	got := referenceSites(f, "p.go", "plainFunc")
	want := []string{"p.go:(*G).generic", "p.go:(*S).bareValue", "p.go:<package scope>", "p.go:S.valueRecv", "p.go:callback"}
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("referenceSites = %v, want %v", got, want)
	}
}

// TestNoDirectScalarCipherCallers is the call-site audit of
// ptone/scion#1530 Phase 2: every locally stored Hub ciphertext goes
// through the shared ValueCipher, so no non-test code calls the legacy
// scalar helpers. They remain only as compatibility fixtures (and are
// pinned to the cipher's v1 envelope by the golden tests).
func TestNoDirectScalarCipherCallers(t *testing.T) {
	for _, name := range []string{"EncryptValue", "DecryptValue", "DeriveLocalEncryptionKey"} {
		for _, site := range selectorCallSites(t, name) {
			t.Errorf("direct scalar-secret cipher call %s at %s: use the shared secret.ValueCipher", name, site)
		}
	}
}

// Ring-mode legacy-entry census (ptone/scion#1530 Phase 6, task T1). It
// pins every reference to the shared signing secret, its resolvers, the
// keyring "legacy" signing entry and the legacy key derivations, so a new
// reader fails CI until it is classified here and in the consumer map
// (.design/secret-rotation-consumer-map.md).
//
// Classes:
//   - consumerOnRing: derives a purpose key from every generation of the
//     signing ring (the "legacy" generation derives the old bytes).
//   - consumerLegacyOnly: reached only in legacy mode (no keyring file),
//     or its value is unused in ring mode, or it is a test fixture or tool.
//   - consumerRingLegacyEntry: reads the keyring "legacy" entry (or the
//     shared signing secret, which is that entry) in managed ring mode.
//     These are task T7's exact scope; the list must be empty before the
//     old signing generation can be retired.
//   - consumerReleaseGate: the release refusal that requires the "legacy"
//     entry until task T10 lifts it.
const (
	consumerOnRing          = "on-ring"
	consumerLegacyOnly      = "legacy-only"
	consumerRingLegacyEntry = "ring-mode legacy-entry reader"
	consumerReleaseGate     = "release gate"
)

// legacyEntryNames are the names the census tracks: the shared signing
// secret field, the cmd resolvers of the session secret and the keyring
// "legacy" entry, the legacy signing-key resolution and derivation, and the
// legacy encryption constructions.
var legacyEntryNames = []string{
	"SharedSigningSecret",
	"resolveSessionSecret",
	"sessionSecretValue",
	"keyringLegacySecret",
	"LegacySecret",
	"ensureSigningKey",
	"deriveSharedSigningKey",
	"NewLegacyValueCipher",
	"DeriveLegacyEncryptionKey",
}

// legacyEntryConsumer classifies the references to one tracked name in one
// function.
type legacyEntryConsumer struct {
	refs  int // references to the name in the function; a new one fails
	class string
	why   string
}

// legacyEntryConsumers is keyed "<name>@<file>:<enclosing func>" (non-test
// files, see selectorCallSites). Line references in the reasons are at
// 4c60666.
var legacyEntryConsumers = map[string]legacyEntryConsumer{
	// The keyring "legacy" entry and the cmd resolvers that return it in
	// ring mode: the root of every cmd ring-mode dependence below.
	"LegacySecret@cmd/server_keyring.go:keyringLegacySecret": {1, consumerRingLegacyEntry,
		"returns the keyring file's legacy signing entry to resolveSessionSecret and sessionSecretValue"},
	"keyringLegacySecret@cmd/server_foreground.go:resolveSessionSecret": {1, consumerRingLegacyEntry,
		"with a keyring file the session secret is the legacy entry; every caller below reads it"},
	"keyringLegacySecret@cmd/server_foreground.go:sessionSecretValue": {1, consumerRingLegacyEntry,
		"with a keyring file the session secret value is the legacy entry"},

	// Startup configuration and validation.
	"resolveSessionSecret@cmd/server_foreground.go:buildHubServerConfig": {2, consumerRingLegacyEntry,
		"sets hub.ServerConfig.SharedSigningSecret to the legacy entry (:2899); the second reference feeds webSessionKeySource, which ignores it with a keyring file (:2902)"},
	"SharedSigningSecret@pkg/hub/keyring.go:validateKeyringConfig": {1, consumerRingLegacyEntry,
		"requires SharedSigningSecret == the legacy entry in ring mode (:126)"},
	"LegacySecret@pkg/hub/keyring.go:validateKeyringConfig": {1, consumerRingLegacyEntry,
		"requires a legacy signing entry in ring mode (:116)"},
	"LegacySecret@pkg/keyring/load.go:(*Keyring).CheckSessionPilot": {1, consumerReleaseGate,
		"release refusal: requires the legacy entry (T10 lifts it)"},
	"DeriveLegacyEncryptionKey@pkg/keyring/load.go:(*Keyring).CheckSessionPilot": {1, consumerReleaseGate,
		"release refusal: active encryption key must equal the key derived from the legacy entry (T10 lifts it)"},
	"resolveSessionSecret@cmd/server_foreground.go:validateHostedBasic": {1, consumerRingLegacyEntry,
		"hosted warning keyed on a non-empty session secret; without a legacy entry it would warn in ring mode"},
	"resolveSessionSecret@cmd/server_foreground.go:validateHostedHAPreflight": {1, consumerRingLegacyEntry,
		"hosted HA preflight refuses an empty session secret; without a legacy entry it would refuse ring mode"},
	"SharedSigningSecret@cmd/server_foreground.go:initHubServer": {1, consumerRingLegacyEntry,
		"hosted warning keyed on an empty SharedSigningSecret"},

	// Relay-peer auth: the signer comes from the ring (SigningRing), but the
	// PeerSecret inequality check compares against the legacy entry only
	// (Obl 6 wants every generation).
	"resolveSessionSecret@cmd/server_conduit.go:conduitPeerAuthOptions": {1, consumerRingLegacyEntry,
		"ConduitPeerAuthOptions.SharedSecret = legacy entry; relay.NewConduitPeerAuth compares PeerSecret against it only (conduit_peerauth.go:149)"},
	"sessionSecretValue@cmd/server_conduit.go:externalPeerAuthOptions": {1, consumerRingLegacyEntry,
		"external relay mode: SharedSecret = legacy entry for the PeerSecret inequality check"},
	"sessionSecretValue@cmd/server_foreground.go:validateServerPreflight": {1, consumerRingLegacyEntry,
		"validateConduitPeerSecret compares the peer secret against the legacy entry only (server_conduit.go:483)"},

	// Test admin tier G7: refuses a non-empty SharedSigningSecret, which in
	// ring mode is the legacy entry (signing_keys_hub_local also refuses the
	// keyring source; T7 keeps the refusal without the legacy entry).
	"SharedSigningSecret@cmd/server_test_admin_gate.go:testModeInputsFor": {1, consumerRingLegacyEntry,
		"G7 input: SharedSigningSecret presence"},
	"SharedSigningSecret@cmd/server_test_admin_gate.go:resolveTestMode": {1, consumerRingLegacyEntry,
		"G7 refusal on SharedSigningSecret presence (testModeInputs bool)"},
	"SharedSigningSecret@pkg/hub/test_admin_gate.go:validateTestAdminGateConfig": {1, consumerRingLegacyEntry,
		"G7 re-check at construction on SharedSigningSecret presence"},

	// Session cookies: on the ring in ring mode (WebServerConfig.SigningRing);
	// SessionSecret is used only when the ring is nil (web.go:733-745).
	"resolveSessionSecret@cmd/server_foreground.go:initWebServer": {1, consumerLegacyOnly,
		"WebServerConfig.SessionSecret is unused when SigningRing (the keyring file's ring) is set"},

	// Encryption: the shared ValueCipher comes from the keyring file in ring
	// mode; the legacy cipher is built only without one.
	"resolveSessionSecret@cmd/server_keyring.go:buildServerValueCipher": {1, consumerLegacyOnly,
		"legacy cipher only without a keyring file (the file branch returns first)"},
	"NewLegacyValueCipher@cmd/server_keyring.go:buildServerValueCipher": {1, consumerLegacyOnly,
		"legacy cipher only without a keyring file"},
	"SharedSigningSecret@pkg/hub/keyring.go:serverValueCipher": {1, consumerLegacyOnly,
		"legacy cipher only without a keyring file (cfg.ValueCipher or the file branch returns first)"},
	"NewLegacyValueCipher@pkg/hub/keyring.go:serverValueCipher": {1, consumerLegacyOnly,
		"legacy cipher only without a keyring file"},
	"NewLegacyValueCipher@pkg/secret/localbackend.go:NewLocalBackend": {1, consumerLegacyOnly,
		"scalar-secret constructor; production uses NewBackend with the shared ValueCipher"},
	"NewLegacyValueCipher@pkg/secret/ciphertest/ciphertest.go:Legacy": {1, consumerLegacyOnly,
		"test fixture package"},
	"DeriveLegacyEncryptionKey@pkg/secret/ciphertest/ciphertest.go:Keyring": {1, consumerLegacyOnly,
		"test fixture package: imports the legacy key into a test ring"},
	"DeriveLegacyEncryptionKey@pkg/keyring/ring.go:Legacy": {1, consumerLegacyOnly,
		"the legacy adapter keyring (no keyring file)"},

	// Signing keys: ensureSigningKey and its shared-secret derivation run
	// only in legacy mode; ring mode takes managedCapabilityKeys or
	// initRingTokenServices (keyringManaged / managedSigning).
	"ensureSigningKey@pkg/hub/server.go:(*Server).initLegacyTokenServices": {2, consumerLegacyOnly,
		"agent/user JWT keys; ring mode calls initRingTokenServices (server.go:2832)"},
	"ensureSigningKey@pkg/hub/download_signing.go:(*Server).initDownloadSigningKey": {1, consumerLegacyOnly,
		"behind keyringManaged()"},
	"ensureSigningKey@pkg/hub/artifacts_store.go:(*Server).initArtifactViewKey": {1, consumerLegacyOnly,
		"behind keyringManaged()"},
	"ensureSigningKey@pkg/hub/port_view.go:(*Server).initPortViewKey": {1, consumerLegacyOnly,
		"behind keyringManaged()"},
	"ensureSigningKey@pkg/hub/authorized_list.go:(*Server).initListCursorSealer": {1, consumerLegacyOnly,
		"behind keyringManaged()"},
	"ensureSigningKey@pkg/hub/attachment_delivery.go:(*Server).initAttachmentDeliveryKey": {1, consumerLegacyOnly,
		"behind keyringManaged(); attachment delivery is integrated (ring mode: managedCapabilityKeys)"},
	"SharedSigningSecret@pkg/hub/server.go:(*Server).ensureSigningKeyWithSource": {2, consumerLegacyOnly,
		"reached only through ensureSigningKey (legacy mode)"},
	"deriveSharedSigningKey@pkg/hub/server.go:(*Server).ensureSigningKeyWithSource": {1, consumerLegacyOnly,
		"reached only through ensureSigningKey (legacy mode)"},
	"SharedSigningSecret@pkg/hub/conduit_admission.go:(*Server).conduitAdmissionRing": {1, consumerLegacyOnly,
		"ring mode returns the keyring's signing ring first (:166)"},
	"deriveSharedSigningKey@pkg/hub/capability_keys.go:sharedSecretPurpose": {1, consumerOnRing,
		"capability purposes (download, artifact view, port view, list cursor, attachment delivery) over every ring generation"},
	"deriveSharedSigningKey@pkg/hub/jwt_keys.go:<package scope>": {2, consumerOnRing,
		"agent and user JWT purposes over every ring generation"},

	// Tools outside the server.
	"SharedSigningSecret@perf/bench/seed/main.go:run": {1, consumerLegacyOnly,
		"benchmark seeding tool; legacy-mode hub.New"},
	"deriveSharedSigningKey@perf/bench/seed/main.go:run": {1, consumerLegacyOnly,
		"benchmark seeding tool; its own copy of the derivation"},
}

// legacyEntrySites counts the references to every tracked name, keyed like
// legacyEntryConsumers.
func legacyEntrySites(t *testing.T) map[string]int {
	t.Helper()
	sites := map[string]int{}
	for name, refs := range selectorCallSitesByName(t, legacyEntryNames...) {
		for _, site := range refs {
			sites[name+"@"+site]++
		}
	}
	return sites
}

// legacyEntryCensusViolations compares the scanned references with the
// census: an unclassified reference, a changed reference count or a stale
// classification is a violation.
func legacyEntryCensusViolations(sites map[string]int, census map[string]legacyEntryConsumer) []string {
	var out []string
	for key, n := range sites {
		c, ok := census[key]
		switch {
		case !ok:
			out = append(out, fmt.Sprintf("unclassified legacy-entry reference %s (%d): classify it in legacyEntryConsumers and the consumer map", key, n))
		case c.refs != n:
			out = append(out, fmt.Sprintf("%s: %d references, census says %d: reclassify", key, n, c.refs))
		}
	}
	for key := range census {
		if _, ok := sites[key]; !ok {
			out = append(out, "stale legacy-entry classification "+key)
		}
	}
	sort.Strings(out)
	return out
}

func TestRingModeLegacyEntryCensus(t *testing.T) {
	for _, v := range legacyEntryCensusViolations(legacyEntrySites(t), legacyEntryConsumers) {
		t.Error(v)
	}
	valid := map[string]bool{consumerOnRing: true, consumerLegacyOnly: true, consumerRingLegacyEntry: true, consumerReleaseGate: true}
	for key, c := range legacyEntryConsumers {
		if !valid[c.class] || c.why == "" || c.refs < 1 {
			t.Errorf("%s: class %q, refs %d, reason %q: want a known class, refs >= 1 and a reason", key, c.class, c.refs, c.why)
		}
	}
}

// A planted new reader fails the census: a new function reading
// SharedSigningSecret, a bare-value alias of resolveSessionSecret, a second
// read inside a classified function, and a removed reader (a stale
// classification).
func TestRingModeLegacyEntryCensusRefusesPlantedReader(t *testing.T) {
	const src = `package cmd
func plantedReader(c *Config) string { return c.SharedSigningSecret }
func plantedAlias() func() string { return resolveSessionSecret }
`
	f, err := parser.ParseFile(token.NewFileSet(), "planted.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	base := legacyEntrySites(t)
	if v := legacyEntryCensusViolations(base, legacyEntryConsumers); len(v) != 0 {
		t.Fatalf("census not clean before planting: %v", v)
	}

	planted := maps.Clone(base)
	for _, name := range legacyEntryNames {
		for _, site := range referenceSites(f, "cmd/planted.go", name) {
			planted[name+"@"+site]++
		}
	}
	got := legacyEntryCensusViolations(planted, legacyEntryConsumers)
	if len(got) != 2 ||
		!strings.HasPrefix(got[0], "unclassified legacy-entry reference SharedSigningSecret@cmd/planted.go:plantedReader ") ||
		!strings.HasPrefix(got[1], "unclassified legacy-entry reference resolveSessionSecret@cmd/planted.go:plantedAlias ") {
		t.Fatalf("planted new readers: violations %v, want the unclassified plantedReader and the bare-value plantedAlias", got)
	}

	inFunc := maps.Clone(base)
	inFunc["SharedSigningSecret@pkg/hub/conduit_admission.go:(*Server).conduitAdmissionRing"]++
	if got := legacyEntryCensusViolations(inFunc, legacyEntryConsumers); len(got) != 1 || !strings.Contains(got[0], "2 references, census says 1") {
		t.Fatalf("second read in a classified function: violations %v, want one count mismatch", got)
	}

	removed := maps.Clone(base)
	delete(removed, "SharedSigningSecret@pkg/hub/conduit_admission.go:(*Server).conduitAdmissionRing")
	if got := legacyEntryCensusViolations(removed, legacyEntryConsumers); len(got) != 1 || !strings.HasPrefix(got[0], "stale legacy-entry classification ") {
		t.Fatalf("removed reader: violations %v, want one stale classification", got)
	}
}

// consumerMapKey matches a backticked census key in the consumer map.
var consumerMapKey = regexp.MustCompile("`([A-Za-z][A-Za-z0-9_]*@[^`@ ]+:[^`@ ]+)`")

// consumerMapNone is the consumer map line recording an empty T7 list.
const consumerMapNone = "ring-mode legacy-entry readers: none"

// consumerMapSection returns the census keys in the consumer map section
// whose heading starts with heading, up to the next heading of any level,
// and whether the section holds the consumerMapNone line.
func consumerMapSection(t *testing.T, doc, heading string) (keys []string, none bool) {
	t.Helper()
	in, found := false, false
	for _, line := range strings.Split(doc, "\n") {
		if strings.HasPrefix(line, "#") {
			in = strings.HasPrefix(strings.TrimLeft(line, "# "), heading)
			found = found || in
			continue
		}
		if !in {
			continue
		}
		if strings.TrimSpace(line) == consumerMapNone {
			none = true
		}
		for _, m := range consumerMapKey.FindAllStringSubmatch(line, -1) {
			keys = append(keys, m[1])
		}
	}
	if !found {
		t.Fatalf("consumer map has no section %q", heading)
	}
	sort.Strings(keys)
	return keys, none
}

// censusKeys returns the census keys of class, sorted.
func censusKeys(class string) []string {
	var out []string
	for key, c := range legacyEntryConsumers {
		if c.class == class {
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out
}

// The consumer map's T7 and release-gate sections list exactly the census
// keys of those classes, so the document cannot drift from the census in
// either direction. The T7 section records consumerMapNone if and only if
// the list is empty.
func TestRingModeLegacyEntryReadersInConsumerMap(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(moduleRoot(t), ".design", "secret-rotation-consumer-map.md"))
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	for _, sec := range []struct{ heading, class string }{
		{"Ring-mode legacy-entry readers", consumerRingLegacyEntry},
		{"Release gate", consumerReleaseGate},
	} {
		got, none := consumerMapSection(t, doc, sec.heading)
		want := censusKeys(sec.class)
		if !slices.Equal(got, want) {
			t.Errorf("consumer map section %q lists %v, census %s keys are %v", sec.heading, got, sec.class, want)
		}
		if sec.class == consumerRingLegacyEntry && none != (len(want) == 0) {
			t.Errorf("consumer map section %q: %q line present = %v, census list empty = %v", sec.heading, consumerMapNone, none, len(want) == 0)
		}
	}
}
