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

package util

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestNoUnscopedHostGitCredentials is a static guard: host-side git must
// authenticate only through a host-scoped credential helper
// (ScopedCredentialHelperEnv), never an unscoped credential.helper or a token
// embedded in a URL. It parses the host git packages (comments are ignored)
// and fails if a string literal reintroduces either pattern, or if the retired
// TokenCredentialHelperEnv is declared again.
//
// Out of scope by design (not scanned): pkg/config/remote_templates.go (the
// template cache; its host fetch/checkout carry the pins and skip-smudge), the
// in-container agent credential helper written to the agent's own .gitconfig,
// and the in-container sciontool init path.
func TestNoUnscopedHostGitCredentials(t *testing.T) {
	root := repoRootForGuard(t)
	pkgs := []string{"pkg/util", "pkg/provision", "pkg/agent", "pkg/runtimebroker"}
	for _, pkg := range pkgs {
		dir := filepath.Join(root, pkg)
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			scanGoFileForTokenPatterns(t, filepath.Join(dir, name))
		}
	}
}

func scanGoFileForTokenPatterns(t *testing.T, path string) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0) // no comments
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncDecl:
			if x.Name.Name == "TokenCredentialHelperEnv" {
				t.Errorf("%s: TokenCredentialHelperEnv is retired; use ScopedCredentialHelperEnv", path)
			}
		case *ast.BasicLit:
			if x.Kind != token.STRING {
				return true
			}
			v := x.Value // includes quotes/backticks; fine for substring checks
			// An unscoped credential.helper key (the scoped form is
			// "credential.%s://%s.helper", which does not contain this).
			// git_host.go's ScopedCredentialHelperEnv legitimately emits an
			// empty `credential.helper` reset before the scoped helper; that is
			// the one sanctioned use.
			if strings.Contains(v, "credential.helper") && filepath.Base(path) != "git_host.go" {
				t.Errorf("%s:%d: unscoped 'credential.helper' literal; host git must use ScopedCredentialHelperEnv", path, fset.Position(x.Pos()).Line)
			}
			// A token embedded in a URL (userinfo), for host git.
			if strings.Contains(v, "oauth2:") || strings.Contains(v, "x-access-token:") {
				t.Errorf("%s:%d: token-in-URL construction; host git must not embed credentials in the URL", path, fset.Position(x.Pos()).Line)
			}
		case *ast.SelectorExpr:
			// url.UserPassword(...) embeds credentials in a URL.
			if x.Sel.Name == "UserPassword" {
				if id, ok := x.X.(*ast.Ident); ok && id.Name == "url" {
					t.Errorf("%s:%d: url.UserPassword builds a credential-bearing URL; host git must not embed credentials in the URL", path, fset.Position(x.Pos()).Line)
				}
			}
		}
		return true
	})
}

// partialCredentialExemptions names the non-test Go files allowed to contain a
// specific credential-embedding pattern (by finding kind), while every other
// pattern is still scanned there. Every exemption is scoped to (file, pattern),
// never a whole file, each with a one-line reason. A dead exemption (one that
// matches nothing) fails TestNoEmbeddedGitCredentialWholeTree. Finding kinds:
// findingURLUser, findingURLUserPassword, findingTokenURLLiteral,
// findingUserinfoFormat, findingPasswordHelper.
var partialCredentialExemptions = map[string]map[string]string{
	// The git credential-helper protocol prints "password=<token>" to git over
	// stdout; this is the helper itself answering git, not an argv or config value.
	"cmd/sciontool/commands/credential_helper.go": {findingPasswordHelper: "prints the credential-helper protocol (password=<token>) to git over stdout, not an argv or config value"},
	// Redaction rewrites the URL with a fixed mask, never a real credential.
	"pkg/config/dsn_redact.go": {findingURLUserPassword: "redaction rewrites the URL with a fixed mask, not a credential"},
	// The template cache's own clone still embeds a token in the URL; moving it to
	// a scoped credential helper is a separate, tracked follow-up, after which this
	// exemption is removed and the guard must pass without it.
	"pkg/config/remote_templates.go": {
		findingTokenURLLiteral: "template-cache clone; tracked follow-up to move to a scoped credential helper",
		findingUserinfoFormat:  "template-cache clone; tracked follow-up to move to a scoped credential helper",
	},
	// url.User here carries no credential: only the existing username.
	"pkg/hub/project_clone.go": {findingURLUser: "url.User carries only the existing username, no credential"},
	// urlredact is the URL-redaction package; its non-test source carries
	// token-bearing URLs as redaction inputs and expected outputs, not live
	// credentials.
	"pkg/util/urlredact/urlredact.go":                   {findingURLUser: "url.User sets the redaction mask, not a credential"},
	"pkg/util/urlredact/urlredacttest/rows.go":          {findingTokenURLLiteral: "token-bearing URLs are redaction test fixtures (inputs and expected outputs), not live credentials"},
	"pkg/util/urlredact/urlredacttest/urlredacttest.go": {findingTokenURLLiteral: "token-bearing URLs are redaction test fixtures (inputs and expected outputs), not live credentials"},
}

// TestNoEmbeddedGitCredentialWholeTree is a whole-tree static guard (non-test Go):
// no file may format a credential into a git credential-helper value or into a
// URL. It catches a token injected into helper text (a `password=` value that is
// not a run-time ${ENV} expansion) and a token placed in URL userinfo
// (url.UserPassword, or an "oauth2:"/"x-access-token:" userinfo literal). Files
// that legitimately contain such a pattern are listed, one by one and scoped to
// the specific pattern, in partialCredentialExemptions.
func TestNoEmbeddedGitCredentialWholeTree(t *testing.T) {
	root := repoRootForGuard(t)
	// used records each (file, kind) exemption that actually suppressed a finding,
	// so a dead exemption (one that matches nothing) can be reported after the walk.
	used := exemptionUse{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "node_modules", "testdata", "extras":
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		slash := filepath.ToSlash(rel)
		scanGoFileForEmbeddedCredential(t, path, slash, partialCredentialExemptions[slash], used)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	// A dead exemption (the file has no finding of that kind, or the file no longer
	// exists) is a latent hole: it would silently allow that pattern if the file
	// reintroduced it. Fail so dead entries are dropped, not left behind.
	if dead := deadExemptions(partialCredentialExemptions, used); len(dead) > 0 {
		t.Errorf("dead credential-guard exemption(s) that match nothing — drop them:\n%s", strings.Join(dead, "\n"))
	}
}

// exemptionUse records which (file, kind) exemptions suppressed a finding.
type exemptionUse map[string]bool

func (u exemptionUse) mark(file, kind string) { u[file+"\x00"+kind] = true }
func (u exemptionUse) has(file, kind string) bool {
	return u[file+"\x00"+kind]
}

// deadExemptions returns "<file> [<kind>]" for every exemption that never
// suppressed a finding (sorted). Such an entry matches nothing — either the file
// has no finding of that kind, or the file is absent.
func deadExemptions(exemptions map[string]map[string]string, used exemptionUse) []string {
	var dead []string
	for file, kinds := range exemptions {
		for kind := range kinds {
			if !used.has(file, kind) {
				dead = append(dead, file+" ["+kind+"]")
			}
		}
	}
	sort.Strings(dead)
	return dead
}

func scanGoFileForEmbeddedCredential(t *testing.T, path, rel string, allowedKinds map[string]string, used exemptionUse) {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	findings := embeddedCredentialFindings(src)
	// Record which allowed kinds actually appear in this file, so an exemption that
	// matches nothing is reported as dead after the walk.
	for _, v := range findings {
		if _, ok := allowedKinds[v.kind]; ok {
			used.mark(rel, v.kind)
		}
	}
	// filterCredentialFindings applies the (file, pattern) exemption; the self-test
	// exercises the same helper so it covers the filter the guard actually runs.
	for _, v := range filterCredentialFindings(findings, allowedKinds) {
		t.Errorf("%s:%d: %s", rel, v.line, v.msg)
	}
}

// filterCredentialFindings drops findings whose kind is in allowedKinds (the
// (file, pattern) exemption for that file), keeping every other pattern. A nil
// allowedKinds returns the findings unchanged.
func filterCredentialFindings(findings []credentialFinding, allowedKinds map[string]string) []credentialFinding {
	var out []credentialFinding
	for _, v := range findings {
		if _, ok := allowedKinds[v.kind]; ok {
			continue
		}
		out = append(out, v)
	}
	return out
}

// credentialFinding is one place a credential is embedded in a URL or a git
// credential-helper value, with a stable kind so a file may be exempted from one
// pattern without silencing the others.
type credentialFinding struct {
	line int
	kind string
	msg  string
}

const (
	findingURLUser         = "url.User"
	findingURLUserPassword = "url.UserPassword"
	findingTokenURLLiteral = "token-url-literal"
	findingUserinfoFormat  = "userinfo-format"
	findingPasswordHelper  = "password-helper"
)

// embeddedCredentialFindings parses Go source (comments dropped) and returns each
// place a credential is embedded in a git credential-helper value or a URL:
// url.User/url.UserPassword (under whatever local name net/url is imported), an
// "oauth2:"/"x-access-token:" userinfo literal, a token formatted into URL
// userinfo ("…%s@…"), or a password= helper value that injects a token (a format
// verb or a concatenation) rather than the allowed run-time ${ENV} expansion.
// Unparseable input returns no findings.
func embeddedCredentialFindings(src []byte) []credentialFinding {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", src, 0)
	if err != nil {
		return nil
	}
	// The local name net/url is imported under (default "url"); "" if not imported.
	urlName := ""
	for _, imp := range f.Imports {
		p, uErr := strconv.Unquote(imp.Path.Value)
		if uErr == nil && p == "net/url" {
			if imp.Name != nil {
				urlName = imp.Name.Name
			} else {
				urlName = "url"
			}
		}
	}
	var out []credentialFinding
	add := func(pos token.Pos, kind, msg string) {
		out = append(out, credentialFinding{line: fset.Position(pos).Line, kind: kind, msg: msg})
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SelectorExpr:
			if urlName != "" && urlName != "_" && (x.Sel.Name == "UserPassword" || x.Sel.Name == "User") {
				if id, ok := x.X.(*ast.Ident); ok && id.Name == urlName {
					kind := findingURLUser
					if x.Sel.Name == "UserPassword" {
						kind = findingURLUserPassword
					}
					add(x.Pos(), kind, fmt.Sprintf("url.%s embeds a credential in a URL", x.Sel.Name))
				}
			}
		case *ast.BasicLit:
			if x.Kind != token.STRING {
				return true
			}
			v := x.Value
			if strings.Contains(v, "oauth2:") || strings.Contains(v, "x-access-token:") {
				add(x.Pos(), findingTokenURLLiteral, "token-in-URL userinfo literal; supply the token through a credential helper")
			}
			// A token formatted into URL userinfo, e.g. "https://%s@host" or
			// "https://user:%s@host". Only the authority (between "://" and the next
			// "/") counts, so a "%s@" elsewhere in the path (e.g. a gh://owner/repo@ref
			// ref separator) is not a userinfo credential.
			if i := strings.Index(v, "://"); i >= 0 {
				auth := v[i+3:]
				if j := strings.IndexByte(auth, '/'); j >= 0 {
					auth = auth[:j]
				}
				if strings.Contains(auth, "%s@") {
					add(x.Pos(), findingUserinfoFormat, "token formatted into a URL userinfo (%s@); supply it through a credential helper")
				}
			}
			// A credential-helper value that injects a token into password= (a
			// format verb, or a literal that ends at password= for concatenation),
			// as opposed to the allowed run-time ${ENV} expansion.
			if (strings.Contains(v, "password=%") || strings.HasSuffix(strings.TrimRight(v, "\"`"), "password=")) &&
				!strings.Contains(v, "password=${") {
				add(x.Pos(), findingPasswordHelper, "credential injected into a password= helper value; read it from the environment at run time instead")
			}
		}
		return true
	})
	return out
}

func repoRootForGuard(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// Tests run in pkg/util; the module root is two levels up.
	root := filepath.Join(wd, "..", "..")
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("could not locate repo root from %s: %v", wd, err)
	}
	return root
}

// Self-test for embeddedCredentialFindings: each credential-embedding pattern is
// flagged with the expected kind, and the allowed run-time ${ENV} expansion is not.
func TestEmbeddedCredentialViolations_SelfTest(t *testing.T) {
	flagged := map[string]struct {
		src  string
		kind string
	}{
		"url.UserPassword": {"package p\nimport \"net/url\"\nfunc f(tok string) { _ = url.UserPassword(\"oauth2\", tok) }\n", findingURLUserPassword},
		"url.User":         {"package p\nimport \"net/url\"\nfunc f(tok string) { _ = url.User(tok) }\n", findingURLUser},
		"aliased url.User": {"package p\nimport neturl \"net/url\"\nfunc f(tok string) { _ = neturl.User(tok) }\n", findingURLUser},
		"oauth2 userinfo":  {"package p\nvar s = \"https://oauth2:abc@github.com/o/r.git\"\n", findingTokenURLLiteral},
		"x-access-token":   {"package p\nvar s = \"https://x-access-token:abc@github.com/o/r.git\"\n", findingTokenURLLiteral},
		"%s@ URL":          {"package p\nimport \"fmt\"\nfunc f(tok string) string { return fmt.Sprintf(\"https://%s@github.com/o/r.git\", tok) }\n", findingUserinfoFormat},
		"password format":  {"package p\nimport \"fmt\"\nfunc f(tok string) string { return fmt.Sprintf(\"!f(){ echo password=%s; }; f\", tok) }\n", findingPasswordHelper},
		"password concat":  {"package p\nfunc f(tok string) string { return \"!f(){ echo password=\" + tok + \"; }; f\" }\n", findingPasswordHelper},
	}
	for name, tc := range flagged {
		got := embeddedCredentialFindings([]byte(tc.src))
		if len(got) == 0 {
			t.Errorf("%s: expected a finding, got none", name)
			continue
		}
		found := false
		for _, v := range got {
			if v.kind == tc.kind {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: expected a %q finding, got %+v", name, tc.kind, got)
		}
	}
	clean := map[string]string{
		"env expansion helper": "package p\nvar s = `!f(){ echo \"password=${GITHUB_TOKEN}\"; }; f`\n",
		"plain https url":      "package p\nvar s = \"https://github.com/o/r.git\"\n",
		"username only":        "package p\nvar s = \"username=oauth2\"\n",
	}
	for name, src := range clean {
		if v := embeddedCredentialFindings([]byte(src)); len(v) != 0 {
			t.Errorf("%s: expected no finding, got %+v", name, v)
		}
	}
}

// A (file, pattern) exemption must still catch every other pattern in the exempted
// file: a token embedded in a URL (e.g. an "oauth2:" userinfo literal added to the
// url.User-exempted pkg/hub/project_clone.go) is still a finding, so adding one
// turns TestNoEmbeddedGitCredentialWholeTree red.
func TestURLUserOnlyExemption_StillScansOtherPatterns(t *testing.T) {
	urlUserOnly := map[string]string{findingURLUser: "test"}
	// url.User alone is exempted (no finding survives the filter the guard runs).
	onlyUser := "package p\nimport \"net/url\"\nfunc f(u string) { _ = url.User(u) }\n"
	if got := filterCredentialFindings(embeddedCredentialFindings([]byte(onlyUser)), urlUserOnly); len(got) != 0 {
		t.Errorf("url.User alone must be exempted, got %+v", got)
	}
	// An oauth2 userinfo literal in the same file is NOT exempted. This uses the same
	// filterCredentialFindings helper scanGoFileForEmbeddedCredential calls, so
	// loosening that filter to drop every pattern turns this self-test red too.
	withToken := "package p\nimport \"net/url\"\nfunc f(u string) { _ = url.User(u) }\nvar s = \"https://oauth2:tok@github.com/o/r.git\"\n"
	got := filterCredentialFindings(embeddedCredentialFindings([]byte(withToken)), urlUserOnly)
	if len(got) == 0 {
		t.Fatalf("an oauth2 token in a url.User-exempted file must still be flagged, got none")
	}
	for _, v := range got {
		if v.kind == findingURLUser {
			t.Errorf("url.User should have been filtered out, got %+v", v)
		}
	}
}

// deadExemptions is the self-check behind TestNoEmbeddedGitCredentialWholeTree's
// no-match rule. A used exemption is not reported; an unused one (a re-added dead
// entry — the mutant) is reported, so a dead exemption cannot be left behind.
func TestDeadExemptionsDetected(t *testing.T) {
	exemptions := map[string]map[string]string{
		"live.go": {findingURLUser: "reason"},
		// The mutant: a re-added dead entry that matches nothing.
		"dead.go": {findingTokenURLLiteral: "reason"},
	}
	used := exemptionUse{}
	used.mark("live.go", findingURLUser) // live.go actually had a url.User finding

	dead := deadExemptions(exemptions, used)
	if len(dead) != 1 || dead[0] != "dead.go ["+findingTokenURLLiteral+"]" {
		t.Fatalf("expected exactly the dead entry reported, got %v", dead)
	}

	// With the dead entry removed, nothing is reported.
	delete(exemptions, "dead.go")
	if got := deadExemptions(exemptions, used); len(got) != 0 {
		t.Errorf("a fully-used exemption set must report no dead entries, got %v", got)
	}

	// A second kind on the same file that is never used is also dead (per-pattern,
	// not per-file).
	exemptions["live.go"][findingPasswordHelper] = "reason"
	if got := deadExemptions(exemptions, used); len(got) != 1 || got[0] != "live.go ["+findingPasswordHelper+"]" {
		t.Errorf("an unused kind on a partly-used file must be reported, got %v", got)
	}
}
