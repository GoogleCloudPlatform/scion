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

package brokercontrol

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixture is a fake home with a Scion global directory and some control
// files, plus an unrelated directory outside the home.
type fixture struct {
	home, global, outside string
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := fixture{
		home:    filepath.Join(root, "home", "u"),
		outside: filepath.Join(root, "data"),
	}
	f.global = filepath.Join(f.home, ".scion")
	for _, d := range []string{
		f.outside,
		filepath.Join(f.global, "hub-credentials"),
		filepath.Join(f.global, "runtime-brokers", "k"),
		filepath.Join(f.global, "projects", "p", "workspace"),
		filepath.Join(f.global, "projects", "p", ".scion", "agents", "a", "home"),
		filepath.Join(f.global, "project-configs", "p__1", "shared-dirs", "s"),
		filepath.Join(f.global, "templates", "t"),
	} {
		mkdir(t, d)
	}
	for _, p := range []string{
		filepath.Join(f.global, "broker-credentials.json"),
		filepath.Join(f.global, "settings.yaml"),
		filepath.Join(f.global, "hub-credentials", "default.json"),
		filepath.Join(f.global, "runtime-brokers", "k", "identity.json"),
	} {
		write(t, p)
	}
	setGlobalDir(t, f.global)
	return f
}

func setGlobalDir(t *testing.T, g string) {
	t.Helper()
	prev := globalDirFunc
	globalDirFunc = func() (string, error) { return g, nil }
	t.Cleanup(func() { globalDirFunc = prev })
}

func setCaseInsensitive(t *testing.T, v bool) {
	t.Helper()
	prev := caseInsensitive
	caseInsensitive = v
	t.Cleanup(func() { caseInsensitive = prev })
}

func mkdir(t *testing.T, d string) {
	t.Helper()
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
}

func write(t *testing.T, p string) {
	t.Helper()
	mkdir(t, filepath.Dir(p))
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	mkdir(t, filepath.Dir(link))
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
}

// raw joins path elements with the separator without cleaning, so ".."
// reaches the resolver as written.
func raw(elems ...string) string {
	return strings.Join(elems, string(filepath.Separator))
}

func wantRefused(t *testing.T, err error, rule Rule) {
	t.Helper()
	if err == nil {
		t.Fatalf("want refusal (%s), got nil", rule)
	}
	if !errors.Is(err, ErrControlPathMount) {
		t.Fatalf("error %v does not wrap ErrControlPathMount", err)
	}
	var re *RefusalError
	if !errors.As(err, &re) {
		t.Fatalf("error %T is not a *RefusalError", err)
	}
	if rule != "" && re.Rule != rule {
		t.Fatalf("rule = %q, want %q (%s)", re.Rule, rule, re.Detail())
	}
}

func TestCheck_Cases(t *testing.T) {
	f := newFixture(t)
	g := f.global
	cases := []struct {
		name string
		src  string
		rule Rule // "" means allowed
	}{
		{"credentials file", filepath.Join(g, "broker-credentials.json"), RuleEqual},
		{"settings file", filepath.Join(g, "settings.yaml"), RuleEqual},
		{"settings spelling that does not exist", filepath.Join(g, "settings.json"), RuleEqual},
		{"server config", filepath.Join(g, "server.yaml"), RuleEqual},
		{"hub database", filepath.Join(g, "hub.db"), RuleEqual},
		{"dev token", filepath.Join(g, "dev-token"), RuleEqual},
		{"file inside credentials dir", filepath.Join(g, "hub-credentials", "default.json"), RuleInside},
		{"credentials dir", filepath.Join(g, "hub-credentials"), RuleEqual},
		{"identity file", filepath.Join(g, "runtime-brokers", "k", "identity.json"), RuleInside},
		{"global dir", g, RuleAncestor},
		{"home", f.home, RuleAncestor},
		{"filesystem root", filepath.VolumeName(f.home) + string(filepath.Separator), RuleAncestor},
		{"plugins dir", filepath.Join(g, "plugins", "x"), RuleInside},
		{"templates (floor)", filepath.Join(g, "templates", "t"), RuleNotAllowed},
		{"agent home under global (volume floor)", filepath.Join(g, "projects", "p", ".scion", "agents", "a", "home"), RuleNotAllowed},
		{"bare project dir", filepath.Join(g, "projects", "p"), RuleNotAllowed},
		{"project workspace", filepath.Join(g, "projects", "p", "workspace"), ""},
		{"shared dir", filepath.Join(g, "project-configs", "p__1", "shared-dirs", "s"), ""},
		{"not yet created under shared dir", filepath.Join(g, "project-configs", "p__1", "shared-dirs", "s", "new"), ""},
		{"outside", f.outside, ""},
		{"outside, not yet created", filepath.Join(f.outside, "new", "dir"), ""},
		{"relative", "data", RuleShape},
		{"empty", "", RuleShape},
		{"dotdot onto global", raw(f.outside, "..", "home", "u", ".scion"), RuleAncestor},
		{"dotdot after missing component", raw(f.outside, "missing", "..", "x"), RuleUnresolved},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := Check(tc.src, VolumeAllowed)
			if tc.rule == "" {
				if err != nil {
					t.Fatalf("want allowed, got %v", err)
				}
				if !filepath.IsAbs(r) {
					t.Fatalf("resolved %q is not absolute", r)
				}
				return
			}
			wantRefused(t, err, tc.rule)
		})
	}
}

func TestCheck_ReadOnlyIsNotAnException(t *testing.T) {
	// The check has no mode parameter: a read-only mount of a control path
	// is refused exactly like a writable one.
	f := newFixture(t)
	_, err := Check(filepath.Join(f.global, "broker-credentials.json"), VolumeAllowed)
	wantRefused(t, err, RuleEqual)
}

func TestCheck_Symlinks(t *testing.T) {
	f := newFixture(t)
	g := f.global

	link := filepath.Join(f.outside, "creds")
	symlink(t, filepath.Join(g, "broker-credentials.json"), link)
	_, err := Check(link, VolumeAllowed)
	wantRefused(t, err, RuleEqual)

	homeLink := filepath.Join(f.outside, "h")
	symlink(t, f.home, homeLink)
	_, err = Check(homeLink, VolumeAllowed)
	wantRefused(t, err, RuleAncestor)

	// An intermediate symlink in the source.
	_, err = Check(filepath.Join(homeLink, ".scion", "settings.yaml"), VolumeAllowed)
	wantRefused(t, err, RuleEqual)

	// ".." after a symlink goes to the target's parent (kernel semantics):
	// <outside>/p -> <global>/projects/p, so <outside>/p/../../settings.yaml
	// is <global>/settings.yaml. Lexically it would be a harmless path next
	// to <outside>.
	projLink := filepath.Join(f.outside, "p")
	symlink(t, filepath.Join(g, "projects", "p"), projLink)
	_, err = Check(raw(projLink, "..", "..", "settings.yaml"), VolumeAllowed)
	wantRefused(t, err, RuleEqual)

	dangling := filepath.Join(f.outside, "dangling")
	symlink(t, filepath.Join(f.outside, "nowhere"), dangling)
	_, err = Check(dangling, VolumeAllowed)
	wantRefused(t, err, RuleUnresolved)

	// A symlink to an allowed directory is judged by its target.
	wsLink := filepath.Join(f.outside, "ws")
	symlink(t, filepath.Join(g, "projects", "p", "workspace"), wsLink)
	if _, err := Check(wsLink, VolumeAllowed); err != nil {
		t.Fatalf("symlink to an allowed workspace: %v", err)
	}
}

func TestCheck_SymlinkedGlobalDir(t *testing.T) {
	// ~/.scion itself a symlink to another disk: the set resolves through
	// it, so mounting the real location's parent is refused.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(root, "disk", "scion")
	write(t, filepath.Join(real, "broker-credentials.json"))
	home := filepath.Join(root, "home")
	symlink(t, real, filepath.Join(home, ".scion"))
	setGlobalDir(t, filepath.Join(home, ".scion"))

	_, err = Check(filepath.Join(root, "disk"), VolumeAllowed)
	wantRefused(t, err, RuleAncestor)
	_, err = Check(filepath.Join(real, "broker-credentials.json"), VolumeAllowed)
	wantRefused(t, err, RuleEqual)
}

func TestCheck_GlobalDirNotYetCreated(t *testing.T) {
	// A fresh host with no ~/.scion yet: the locations are still protected.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "home")
	mkdir(t, home)
	setGlobalDir(t, filepath.Join(home, ".scion"))
	_, err = Check(home, VolumeAllowed)
	wantRefused(t, err, RuleAncestor)
}

func TestCheck_UnresolvableSetFailsClosed(t *testing.T) {
	newFixture(t)
	prev := globalDirFunc
	globalDirFunc = func() (string, error) { return "", errors.New("no home") }
	t.Cleanup(func() { globalDirFunc = prev })
	_, err := Check(t.TempDir(), VolumeAllowed)
	wantRefused(t, err, RuleUnresolved)
}

func TestCheck_ExtraPaths(t *testing.T) {
	f := newFixture(t)
	state := filepath.Join(f.outside, "state", "b1")
	mkdir(t, state)
	SetExtraPaths("test", []Entry{{Path: state, Kind: Dir, Class: ClassHostState}})
	t.Cleanup(func() { SetExtraPaths("test", nil) })

	_, err := Check(filepath.Join(f.outside, "state"), VolumeAllowed)
	wantRefused(t, err, RuleAncestor)
	_, err = Check(filepath.Join(state, "x"), VolumeAllowed)
	wantRefused(t, err, RuleInside)
	if _, err := Check(filepath.Join(f.outside, "other"), VolumeAllowed); err != nil {
		t.Fatalf("sibling of an extra path: %v", err)
	}

	SetExtraPaths("test", []Entry{{Path: "relative", Kind: File, Class: ClassCredentials}})
	_, err = Check(f.outside, VolumeAllowed)
	wantRefused(t, err, RuleUnresolved)
}

func TestCheck_CaseInsensitive(t *testing.T) {
	f := newFixture(t)
	setCaseInsensitive(t, true)
	upper := filepath.Join(f.home, ".SCION", "SETTINGS.YAML")
	_, err := Check(upper, VolumeAllowed)
	wantRefused(t, err, "")
	// A case variant of an allowed subtree is refused: allow-list decisions
	// need an exact spelling.
	_, err = Check(filepath.Join(f.home, ".SCION", "projects", "p", "workspace"), VolumeAllowed)
	wantRefused(t, err, "")
}

func TestCheckNamed_LeavesGlobalSubtreeToCaller(t *testing.T) {
	f := newFixture(t)
	// CheckNamed does not apply an allow list inside the global directory...
	if _, err := CheckNamed(filepath.Join(f.global, "projects", "p")); err != nil {
		t.Fatalf("CheckNamed refused a project dir: %v", err)
	}
	// ...but still refuses named entries and the floor.
	_, err := CheckNamed(filepath.Join(f.global, "settings.yaml"))
	wantRefused(t, err, RuleEqual)
	_, err = CheckNamed(f.home)
	wantRefused(t, err, RuleAncestor)
}

func TestRefusalError_DoesNotLeakResolvedPaths(t *testing.T) {
	f := newFixture(t)
	link := filepath.Join(f.outside, "creds")
	symlink(t, filepath.Join(f.global, "broker-credentials.json"), link)
	_, err := Check(link, VolumeAllowed)
	wantRefused(t, err, RuleEqual)
	msg := err.Error()
	if strings.Contains(msg, f.global) {
		t.Fatalf("requester-facing error names the resolved control path: %s", msg)
	}
	if !strings.Contains(msg, link) || !strings.Contains(msg, string(ClassCredentials)) {
		t.Fatalf("error should name the source and the class: %s", msg)
	}
	var re *RefusalError
	errors.As(err, &re)
	if !strings.Contains(re.Detail(), f.global) {
		t.Fatalf("operator detail should name the resolved path: %s", re.Detail())
	}
}

func TestVolumeAllowed(t *testing.T) {
	g := filepath.Join(string(filepath.Separator), "h", ".scion")
	j := func(p ...string) string { return filepath.Join(append([]string{g}, p...)...) }
	allowed := []string{
		j("workspace"), j("workspace", "x"),
		j("projects", "p", "workspace"), j("projects", "p", "workspace", "a"),
		j("projects", "p", "worktrees", "w"), j("projects", "p", "worktrees", "w", "x"),
		j("projects", "p", ".scion", "agents", "a", "workspace"),
		j("project-configs", "d", ".scion", "agents", "a", "workspace", "x"),
		j("project-configs", "d", "shared-dirs", "s"), j("project-configs", "d", "shared-dirs", "s", "x"),
	}
	refused := []string{
		g, j("projects"), j("projects", "p"), j("projects", "p", ".scion"),
		j("projects", "p", ".scion", "settings.yaml"),
		j("projects", "p", ".scion", "agents", "a", "home"),
		j("projects", "p", "worktrees"),
		j("project-configs"), j("project-configs", "d"), j("project-configs", "d", ".scion"),
		j("project-configs", "d", "shared-dirs"),
		j("agents", "a", "home"), j("templates"), j("harness-configs", "x"), j("cache"),
		j("settings.yaml"), j("broker-credentials.json"),
	}
	for _, p := range allowed {
		if !VolumeAllowed(p, g) {
			t.Errorf("VolumeAllowed(%q) = false, want true", p)
		}
	}
	for _, p := range refused {
		if VolumeAllowed(p, g) {
			t.Errorf("VolumeAllowed(%q) = true, want false", p)
		}
	}
}

func TestNamedEntriesDisjointFromAllowLists(t *testing.T) {
	g := filepath.Join(string(filepath.Separator), "h", ".scion")
	for _, n := range namedEntries {
		if strings.ContainsRune(n.rel, filepath.Separator) || strings.Contains(n.rel, "/") {
			t.Errorf("named entry %q is not a direct child of the global directory", n.rel)
		}
		p := filepath.Join(g, n.rel)
		if VolumeAllowed(p, g) || VolumeAllowed(filepath.Join(p, "x"), g) {
			t.Errorf("named entry %q falls inside the volume allow list", n.rel)
		}
	}
}

func TestFingerprint_ChangesWhenGlobalDirMoves(t *testing.T) {
	f := newFixture(t)
	s1, err := Resolve()
	if err != nil {
		t.Fatal(err)
	}
	s1again, err := Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if s1.Fingerprint() != s1again.Fingerprint() {
		t.Fatal("fingerprint is not stable")
	}
	other := filepath.Join(f.outside, "g2")
	mkdir(t, other)
	setGlobalDir(t, other)
	s2, err := Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if s1.Fingerprint() == s2.Fingerprint() {
		t.Fatal("fingerprint did not change when the global directory moved")
	}
}

// TestTokenFilesAreCredentials pins scion-token to exactly the same
// treatment as transport-token: a named File entry of the credentials
// class, refused by equality and by hard-link identity, not merely by the
// global-directory floor.
func TestTokenFilesAreCredentials(t *testing.T) {
	for _, name := range []string{"transport-token", "scion-token"} {
		t.Run(name, func(t *testing.T) {
			var entry *namedEntry
			for i := range namedEntries {
				if namedEntries[i].rel == name {
					entry = &namedEntries[i]
				}
			}
			if entry == nil || entry.kind != File || entry.class != ClassCredentials {
				t.Fatalf("named entry for %q = %+v, want a File entry of class %q", name, entry, ClassCredentials)
			}
			if _, ok := floorOnlyNames[name]; ok {
				t.Fatalf("%q is also listed as floor-only", name)
			}
			f := newFixture(t)
			p := filepath.Join(f.global, name)
			write(t, p)
			_, err := Check(p, VolumeAllowed)
			wantRefused(t, err, RuleEqual)
			var re *RefusalError
			errors.As(err, &re)
			if re.Class != ClassCredentials {
				t.Fatalf("class = %q, want %q", re.Class, ClassCredentials)
			}
			// The parent directory is refused as containing it.
			_, err = Check(f.global, VolumeAllowed)
			wantRefused(t, err, "")
		})
	}
}

// TestVolumeAllowed_UnresolvableIsRefused: containment is decided by
// pathcanon.Within, and a path it cannot resolve (here a dangling link) is
// not allowed, even in an allow-listed shape.
func TestVolumeAllowed_UnresolvableIsRefused(t *testing.T) {
	f := newFixture(t)
	g := f.global
	ws := filepath.Join(g, "workspace")
	mkdir(t, ws)
	if !VolumeAllowed(filepath.Join(ws, "not-yet-created"), g) {
		t.Fatal("a not-yet-created path in an allowed subtree must stay allowed")
	}
	symlink(t, filepath.Join(f.outside, "nowhere"), filepath.Join(ws, "dangling"))
	if VolumeAllowed(filepath.Join(ws, "dangling", "x"), g) {
		t.Fatal("a path through a dangling link was allowed")
	}
	// A link in an allowed subtree that leads outside the global
	// directory is outside it.
	symlink(t, f.outside, filepath.Join(ws, "out"))
	if VolumeAllowed(filepath.Join(ws, "out"), g) {
		t.Fatal("a link leading outside the global directory was allowed")
	}
}
