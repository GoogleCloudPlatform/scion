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

//go:build unix

package agent

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/GoogleCloudPlatform/scion/pkg/agenthome"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
)

const testSkillsDir = ".claude/skills"

// writeTemplateSkill writes skills/<name>/<file> in the template dir tpl.
func writeTemplateSkill(t *testing.T, tpl, name, file, content string) {
	t.Helper()
	writeFileString(t, filepath.Join(tpl, "skills", name, file), content)
}

// newSkillsHome returns a new agent home and an outside directory with a
// file in it (outsideDir).
func newSkillsHome(t *testing.T) (home, outside, outsideFile string) {
	t.Helper()
	home = filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	outside, outsideFile = outsideDir(t)
	return home, outside, outsideFile
}

func mustLink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

// A template whose skills/ is a link to an outside directory supplies no
// skills, and nothing from that directory reaches the home.
func TestInstallTemplateSkills_LinkedSkillsDirIsSkipped(t *testing.T) {
	home, outside, outsideFile := newSkillsHome(t)
	writeFileString(t, filepath.Join(outside, "evil", "SKILL.md"), "outside skill")
	base := filepath.Join(t.TempDir(), "base")
	mustLink(t, outside, filepath.Join(base, "skills"))
	child := filepath.Join(t.TempDir(), "child")
	writeTemplateSkill(t, child, "good", "SKILL.md", "good")

	names, err := installTemplateSkills(home, []*config.Template{{Name: "base", Path: base}, {Name: "child", Path: child}}, testSkillsDir)
	if err != nil {
		t.Fatal(err)
	}
	if names["evil"] || !names["good"] {
		t.Fatalf("template skill names = %v, want only good", names)
	}
	if _, err := os.Lstat(filepath.Join(home, testSkillsDir, "evil")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a skill from a linked skills dir reached the home (err %v)", err)
	}
	if got := readFileString(t, filepath.Join(home, testSkillsDir, "good", "SKILL.md")); got != "good" {
		t.Fatalf("good skill = %q", got)
	}
	if got := readFileString(t, filepath.Join(outside, "evil", "SKILL.md")); got != "outside skill" {
		t.Fatalf("outside skill changed: %q", got)
	}
	if got := readFileString(t, outsideFile); got != "outside content" {
		t.Fatalf("outside file changed: %q", got)
	}
}

// A template directory that is itself a link supplies no skills.
func TestInstallTemplateSkills_LinkedTemplateDirIsSkipped(t *testing.T) {
	home, _, _ := newSkillsHome(t)
	realTpl := filepath.Join(t.TempDir(), "tpl")
	writeTemplateSkill(t, realTpl, "s", "SKILL.md", "s")
	linked := filepath.Join(t.TempDir(), "linked")
	mustLink(t, realTpl, linked)
	names, err := installTemplateSkills(home, []*config.Template{{Name: "linked", Path: linked}}, testSkillsDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 0 {
		t.Fatalf("names = %v, want none from a linked template dir", names)
	}
	if _, err := os.Lstat(filepath.Join(home, testSkillsDir, "s")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("skill installed from a linked template dir (err %v)", err)
	}
}

// A later template's skill replaces an earlier template's skill of the
// same name as a whole; loose files in skills/ are copied as files.
func TestInstallTemplateSkills_PerSkillReplace(t *testing.T) {
	home, _, _ := newSkillsHome(t)
	base := filepath.Join(t.TempDir(), "base")
	writeTemplateSkill(t, base, "shared", "SKILL.md", "base")
	writeTemplateSkill(t, base, "shared", "extra.md", "base only")
	writeFileString(t, filepath.Join(base, "skills", "README.md"), "readme")
	child := filepath.Join(t.TempDir(), "child")
	writeTemplateSkill(t, child, "shared", "SKILL.md", "child")

	if _, err := installTemplateSkills(home, []*config.Template{{Name: "base", Path: base}, {Name: "child", Path: child}}, testSkillsDir); err != nil {
		t.Fatal(err)
	}
	skill := filepath.Join(home, testSkillsDir, "shared")
	if got := readFileString(t, filepath.Join(skill, "SKILL.md")); got != "child" {
		t.Fatalf("SKILL.md = %q, want the child template's", got)
	}
	if _, err := os.Lstat(filepath.Join(skill, "extra.md")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("base template file kept in the replaced skill (err %v)", err)
	}
	if got := readFileString(t, filepath.Join(home, testSkillsDir, "README.md")); got != "readme" {
		t.Fatalf("loose skills file = %q", got)
	}
}

// Reprovisioning into a home the previous container could write: links
// placed at skill directories and inside skills are replaced by the
// installed skills, never followed.
func TestSkillInstalls_ReplacePlantedLinks(t *testing.T) {
	platformFS := fstest.MapFS{"plat/SKILL.md": {Data: []byte("---\nname: plat\n---\n")}}

	t.Run("platform skill directory", func(t *testing.T) {
		home, outside, outsideFile := newSkillsHome(t)
		mustLink(t, outside, filepath.Join(home, testSkillsDir, "plat"))
		ctx := workspaceSkillsInjectionContext{ForceOverwrite: true}
		if err := injectPlatformSkills(platformFS, home, testSkillsDir, ctx); err != nil {
			t.Fatal(err)
		}
		fi, err := os.Lstat(filepath.Join(home, testSkillsDir, "plat"))
		if err != nil || !fi.IsDir() {
			t.Fatalf("platform skill = %v, %v; want a real directory", fi, err)
		}
		assertOutsideUntouched(t, outside, outsideFile)
	})

	t.Run("platform skill link is present without force", func(t *testing.T) {
		home, outside, outsideFile := newSkillsHome(t)
		link := filepath.Join(home, testSkillsDir, "plat")
		mustLink(t, outside, link)
		if err := injectPlatformSkills(platformFS, home, testSkillsDir, workspaceSkillsInjectionContext{}); err != nil {
			t.Fatal(err)
		}
		if fi, err := os.Lstat(link); err != nil || fi.Mode()&fs.ModeSymlink == 0 {
			t.Fatalf("entry = %v, %v; want the link left alone", fi, err)
		}
		assertOutsideUntouched(t, outside, outsideFile)
	})

	t.Run("file inside a template skill", func(t *testing.T) {
		home, outside, outsideFile := newSkillsHome(t)
		mustLink(t, outsideFile, filepath.Join(home, testSkillsDir, "s", "SKILL.md"))
		tpl := filepath.Join(t.TempDir(), "tpl")
		writeTemplateSkill(t, tpl, "s", "SKILL.md", "template")
		if _, err := installTemplateSkills(home, []*config.Template{{Name: "tpl", Path: tpl}}, testSkillsDir); err != nil {
			t.Fatal(err)
		}
		if got := readFileString(t, filepath.Join(home, testSkillsDir, "s", "SKILL.md")); got != "template" {
			t.Fatalf("SKILL.md = %q", got)
		}
		assertOutsideUntouched(t, outside, outsideFile)
	})

	t.Run("resolved skill directory", func(t *testing.T) {
		home, outside, outsideFile := newSkillsHome(t)
		h := &harness.Generic{}
		mustLink(t, outside, filepath.Join(home, h.SkillsDir(), "review"))
		ctx := ContextWithSkillResolver(context.Background(), &countingSkillResolver{})
		if _, err := installSkillsIntoHome(ctx, "a", "", h, home, []api.SkillReference{{URI: "hub://skills/review"}}); err != nil {
			t.Fatal(err)
		}
		if got := readFileString(t, filepath.Join(home, h.SkillsDir(), "review", "SKILL.md")); got != "# review\n" {
			t.Fatalf("resolved skill = %q", got)
		}
		assertOutsideUntouched(t, outside, outsideFile)
	})
}

// A skills directory in the home that is a link, or sits below one, fails
// every skills site closed with a fixed message that names no path.
func TestSkillInstalls_LinkedSkillsDirFailsClosed(t *testing.T) {
	platformFS := fstest.MapFS{"plat/SKILL.md": {Data: []byte("---\nname: plat\n---\n")}}
	for _, linkRel := range []string{testSkillsDir, ".claude"} {
		t.Run(linkRel, func(t *testing.T) {
			home, outside, outsideFile := newSkillsHome(t)
			mustLink(t, outside, filepath.Join(home, linkRel))
			tpl := filepath.Join(t.TempDir(), "tpl")
			writeTemplateSkill(t, tpl, "s", "SKILL.md", "s")

			errs := map[string]error{}
			_, errs["template"] = installTemplateSkills(home, []*config.Template{{Name: "tpl", Path: tpl}}, testSkillsDir)
			errs["platform"] = injectPlatformSkills(platformFS, home, testSkillsDir, workspaceSkillsInjectionContext{ForceOverwrite: true})
			for site, err := range errs {
				if !errors.Is(err, errSkillPathRefused) {
					t.Errorf("%s: err = %v, want errSkillPathRefused", site, err)
					continue
				}
				if strings.Contains(err.Error(), home) || strings.Contains(err.Error(), outside) {
					t.Errorf("%s: error %q names a path", site, err)
				}
			}
			assertOutsideUntouched(t, outside, outsideFile)
		})
	}
}

// A skills directory in the home that is a regular file (not a directory)
// fails the template and platform sites closed with the same fixed text.
func TestSkillInstalls_FileSkillsDirFailsClosed(t *testing.T) {
	platformFS := fstest.MapFS{"plat/SKILL.md": {Data: []byte("---\nname: plat\n---\n")}}
	home, outside, outsideFile := newSkillsHome(t)
	writeFileString(t, filepath.Join(home, testSkillsDir), "not a directory")
	tpl := filepath.Join(t.TempDir(), "tpl")
	writeTemplateSkill(t, tpl, "s", "SKILL.md", "s")

	errs := map[string]error{}
	_, errs["template"] = installTemplateSkills(home, []*config.Template{{Name: "tpl", Path: tpl}}, testSkillsDir)
	errs["platform"] = injectPlatformSkills(platformFS, home, testSkillsDir, workspaceSkillsInjectionContext{})
	errs["platform-force"] = injectPlatformSkills(platformFS, home, testSkillsDir, workspaceSkillsInjectionContext{ForceOverwrite: true})
	for site, err := range errs {
		if !errors.Is(err, errSkillPathRefused) {
			t.Errorf("%s: err = %v, want errSkillPathRefused", site, err)
			continue
		}
		if strings.Contains(err.Error(), home) {
			t.Errorf("%s: error %q names a path", site, err)
		}
	}
	if got := readFileString(t, filepath.Join(home, testSkillsDir)); got != "not a directory" {
		t.Fatalf("the skills_dir file changed: %q", got)
	}
	assertOutsideUntouched(t, outside, outsideFile)
}

// homeSkillError maps each refusal class to its fixed, path-free text: a
// bare ErrLinkComponent (as the source side returns it), a wrapped
// ErrNotDirectory (as a destination walk returns it), and a *LimitError.
func TestHomeSkillError(t *testing.T) {
	const secret = "/broker/side/path"
	for name, tc := range map[string]struct {
		err      error
		sentinel bool
		text     string
	}{
		"bare link component":     {agenthome.ErrLinkComponent, true, errSkillPathRefused.Error()},
		"wrapped not a directory": {&fs.PathError{Op: "open", Path: secret, Err: agenthome.ErrNotDirectory}, true, errSkillPathRefused.Error()},
		"limit":                   {&agenthome.LimitError{Limit: "4096 files"}, false, "skill s exceeds the agent home limit of 4096 files; it was not installed"},
	} {
		got := homeSkillError("s", tc.err)
		if tc.sentinel != errors.Is(got, errSkillPathRefused) {
			t.Errorf("%s: errors.Is(errSkillPathRefused) = %v, want %v", name, !tc.sentinel, tc.sentinel)
		}
		if got.Error() != tc.text {
			t.Errorf("%s: text = %q, want %q", name, got.Error(), tc.text)
		}
		if strings.Contains(got.Error(), secret) {
			t.Errorf("%s: text %q names a path", name, got.Error())
		}
	}
}

// The resolver's scratch directory is broker-owned, never inside the home,
// and removed after both a successful and a failed install.
func TestInstallSkillsIntoHome_ScratchIsRemoved(t *testing.T) {
	var scratches []string
	orig := skillScratchDir
	skillScratchDir = func() (string, error) {
		dir, err := orig()
		scratches = append(scratches, dir)
		return dir, err
	}
	t.Cleanup(func() { skillScratchDir = orig })
	h := &harness.Generic{}
	ctx := ContextWithSkillResolver(context.Background(), &countingSkillResolver{})
	refs := []api.SkillReference{{URI: "hub://skills/review"}}

	home, outside, outsideFile := newSkillsHome(t)
	if _, err := installSkillsIntoHome(ctx, "a", "", h, home, refs); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(home, h.SkillsDir()))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), stagingDirPrefix) || strings.HasPrefix(e.Name(), ".") {
			t.Errorf("staging entry %q left in the home's skills dir", e.Name())
		}
	}

	failHome, _, _ := newSkillsHome(t)
	mustLink(t, outside, filepath.Join(failHome, h.SkillsDir()))
	_, err = installSkillsIntoHome(ctx, "a", "", h, failHome, refs)
	if !errors.Is(err, errSkillPathRefused) {
		t.Fatalf("install through a linked skills dir: err = %v, want errSkillPathRefused", err)
	}
	if strings.Contains(err.Error(), failHome) || strings.Contains(err.Error(), outside) {
		t.Fatalf("error %q names a path", err)
	}
	assertOutsideUntouched(t, outside, outsideFile)

	if len(scratches) != 2 {
		t.Fatalf("scratch dirs = %v, want one per install", scratches)
	}
	for _, dir := range scratches {
		if strings.HasPrefix(dir, home) || strings.HasPrefix(dir, failHome) {
			t.Errorf("scratch %s is inside an agent home", dir)
		}
		if _, err := os.Lstat(dir); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("scratch %s not removed (err %v)", dir, err)
		}
	}
}

// The resolution record is written into the home without following a
// link: a link placed at the record's path is replaced, its target
// untouched.
func TestResolveAndInstallSkills_RecordNeverFollowsLinks(t *testing.T) {
	home, outside, outsideFile := newSkillsHome(t)
	h := &harness.Generic{}
	mustLink(t, outsideFile, filepath.Join(home, ".scion", resolvedSkillsInputName))
	ctx := ContextWithSkillResolver(context.Background(), &countingSkillResolver{})
	if _, err := resolveAndInstallSkills(ctx, "a", "", h, home, []api.SkillReference{{URI: "hub://skills/review"}}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(filepath.Join(home, ".scion", resolvedSkillsInputName))
	if err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("record = %v, %v; want the link replaced by a regular file", fi, err)
	}
	assertOutsideUntouched(t, outside, outsideFile)
}

// A skill over the skill limits fails closed with a fixed message, and the
// previous copy stays in place.
func TestSkillInstalls_LimitKeepsPreviousCopy(t *testing.T) {
	orig := skillTreeLimits
	skillTreeLimits = agenthome.Limits{MaxFiles: 1}
	t.Cleanup(func() { skillTreeLimits = orig })
	home, _, _ := newSkillsHome(t)
	writeFileString(t, filepath.Join(home, testSkillsDir, "s", "OLD.md"), "old")
	tpl := filepath.Join(t.TempDir(), "tpl")
	writeTemplateSkill(t, tpl, "s", "a.md", "a")
	writeTemplateSkill(t, tpl, "s", "b.md", "b")
	_, err := installTemplateSkills(home, []*config.Template{{Name: "tpl", Path: tpl}}, testSkillsDir)
	if err == nil || !strings.Contains(err.Error(), "exceeds the agent home limit") {
		t.Fatalf("err = %v, want the limit error", err)
	}
	if strings.Contains(err.Error(), home) || strings.Contains(err.Error(), tpl) {
		t.Fatalf("limit error %q names a path", err)
	}
	if got := readFileString(t, filepath.Join(home, testSkillsDir, "s", "OLD.md")); got != "old" {
		t.Fatalf("previous copy not kept: %q", got)
	}
}

type fixedSkillsDir string

func (d fixedSkillsDir) SkillsDir() string { return string(d) }

// harnessSkillsDir refuses a skills directory that is not a path inside
// the agent home, for a harness and for a harness-config entry alike, and
// names the field, not the value.
func TestHarnessSkillsDir(t *testing.T) {
	for _, v := range []string{"/abs/skills", "../x", "a/../b", ".", "a/.."} {
		for name, src := range map[string]skillsDirSource{
			"harness": fixedSkillsDir(v),
			"entry":   entrySkills{config.HarnessConfigEntry{SkillsDir: v}},
		} {
			_, err := harnessSkillsDir(src)
			if !errors.Is(err, errSkillsDirInvalid) {
				t.Errorf("%s %q: err = %v, want errSkillsDirInvalid", name, v, err)
			}
			if err != nil && strings.Contains(err.Error(), v) && v != "." {
				t.Errorf("%s %q: error %q carries the value", name, v, err)
			}
		}
	}
	for v, want := range map[string]string{"": "", "skills": "skills", ".claude/skills/": ".claude/skills", "./.decl/skills": ".decl/skills"} {
		if got, err := harnessSkillsDir(fixedSkillsDir(v)); err != nil || got != want {
			t.Errorf("harnessSkillsDir(%q) = %q, %v; want %q", v, got, err, want)
		}
	}
	if got, err := harnessSkillsDir(entrySkills{config.HarnessConfigEntry{}}); err != nil || got != harness.GenericSkillsDir {
		t.Errorf("entry with no skills_dir = %q, %v; want %q", got, err, harness.GenericSkillsDir)
	}
}
