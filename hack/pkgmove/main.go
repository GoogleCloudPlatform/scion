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

// Command pkgmove moves a set of files out of a Go package into a new
// package, exports the symbols that cross the new boundary, and leaves an
// alias file behind so the source package keeps compiling unchanged.
//
// See hack/pkgmove/README.md for usage, the safety report, and the
// regenerate-don't-rebase workflow.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// errPlan reports that the move cannot be generated (see Plan.Errors).
var errPlan = errors.New("the move cannot be generated; see the errors above")

func main() {
	os.Exit(runMain(os.Args[1:], os.Stdout, os.Stderr))
}

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			*s = append(*s, p)
		}
	}
	return nil
}

func runMain(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("pkgmove", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var cfg Config
	var tags stringList
	fs.StringVar(&cfg.SrcDir, "from", "", "source package directory (required)")
	fs.StringVar(&cfg.DstDir, "to", "", "target package directory (required; must not contain Go files)")
	fs.StringVar(&cfg.PkgName, "name", "", "target package name (default: base name of -to)")
	fs.StringVar(&cfg.Area, "area", "", "alias file stem: zz_alias_<area>.go (default: target package name)")
	fs.Var(&tags, "tags", "comma-separated build tags for the analysis")
	fs.BoolVar(&cfg.DryRun, "dry-run", false, "print the plan and safety report without touching the tree")
	fs.BoolVar(&cfg.Vet, "vet", false, "run go vet on both packages after the move (off by default)")
	fs.BoolVar(&cfg.NoGit, "no-git", false, "move files with os.Rename instead of git mv")
	fs.BoolVar(&cfg.Typecheck, "typecheck", true, "re-type-check both packages (with tests) after the move")
	fs.BoolVar(&cfg.AllowFieldExport, "allow-field-export", false, "allow exporting struct fields (reported as HIGH)")
	fs.BoolVar(&cfg.Strict, "strict", false, "treat HIGH safety findings (init(), var initialisers calling package code, linkname/embed) as errors")
	fs.StringVar(&cfg.TestMainSupport, "testmain-support", "", "import path of a test-support package with RunTestMain(m *testing.M) int; generates a TestMain in the target when moved tests leave a package that has one")
	fs.StringVar(&cfg.ReportPath, "report", "", "safety report path (default: <from>/zz_alias_<area>_safety.txt)")
	fs.Usage = func() {
		printf(stderr, "usage: pkgmove -from <dir> -to <dir> [flags] file.go [file_test.go ...]\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg.Files = fs.Args()
	cfg.Tags = tags
	cfg.Stdout = stdout
	if cfg.SrcDir == "" || cfg.DstDir == "" || len(cfg.Files) == 0 {
		fs.Usage()
		return 2
	}
	if err := run(&cfg); err != nil {
		printf(stderr, "pkgmove: %v\n", err)
		if errors.Is(err, errPlan) {
			return 1
		}
		return 3
	}
	return 0
}

// run performs (or, with DryRun, plans) one move.
func run(cfg *Config) error {
	var err error
	if cfg.SrcDir, err = filepath.Abs(cfg.SrcDir); err != nil {
		return err
	}
	if cfg.DstDir, err = filepath.Abs(cfg.DstDir); err != nil {
		return err
	}
	if cfg.PkgName == "" {
		cfg.PkgName = filepath.Base(cfg.DstDir)
	}
	if cfg.Area == "" {
		cfg.Area = cfg.PkgName
	}
	if cfg.ReportPath == "" {
		cfg.ReportPath = filepath.Join(cfg.SrcDir, "zz_alias_"+cfg.Area+"_safety.txt")
	}
	if cfg.Stdout == nil {
		cfg.Stdout = os.Stdout
	}
	a, err := analyze(cfg)
	if err != nil {
		return err
	}
	p := a.plan
	writePlan(cfg.Stdout, p)
	report := safetyReport(p)
	printf(cfg.Stdout, "\n%s", report)
	if len(p.Errors) > 0 {
		return errPlan
	}
	if cfg.DryRun {
		return nil
	}
	if err := a.execute(); err != nil {
		return err
	}
	if err := os.WriteFile(cfg.ReportPath, []byte(report), 0o644); err != nil {
		return err
	}
	for _, g := range p.StubFiles {
		if err := os.WriteFile(filepath.Join(a.mod.ModDir, g.Path), g.Content, 0o644); err != nil {
			return err
		}
		printf(cfg.Stdout, "reference stub written to %s\n", g.Path)
	}
	printf(cfg.Stdout, "\nsafety report written to %s\n", a.rel(cfg.ReportPath))
	return a.verify()
}

// testHookBeforeStage, when set (tests only), runs after the files are moved
// and written and before they are staged; an error triggers the rollback.
var testHookBeforeStage func() error

func (a *analysis) git(args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = a.cfg.SrcDir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return nil
}

// execute applies the plan to the tree. Every new file content is computed
// before anything is touched; if a later step fails, the moves, writes and
// staging done so far are rolled back.
func (a *analysis) execute() (err error) {
	type write struct {
		f       *srcFile
		path    string
		content []byte
	}
	var writes []write
	var files []*srcFile
	for f := range a.edits {
		files = append(files, f)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	for _, f := range files {
		content, err := applyEdits(f, a.edits[f])
		if err != nil {
			return err
		}
		path := f.Path
		if f.Moved {
			path = filepath.Join(a.cfg.DstDir, f.Name)
		}
		writes = append(writes, write{f, path, content})
	}
	var moves []string
	for _, f := range a.files {
		if f.Moved {
			moves = append(moves, f.Name)
		}
	}
	moves = append(moves, a.assets...)
	sort.Strings(moves)

	if !a.cfg.NoGit {
		// Fail before touching anything if a file to move is untracked, or if
		// a file this run edits has staged changes (the rollback resets the
		// index for those paths to HEAD).
		var paths []string
		for _, name := range moves {
			paths = append(paths, filepath.Join(a.cfg.SrcDir, name))
		}
		if err := a.git(append([]string{"ls-files", "--error-unmatch", "--"}, paths...)...); err != nil {
			return fmt.Errorf("files to move must be tracked by git (or pass -no-git): %v", err)
		}
		for _, w := range writes {
			if !w.f.Moved {
				paths = append(paths, w.f.Path)
			}
		}
		if err := a.git(append([]string{"diff", "--cached", "--quiet", "--"}, paths...)...); err != nil {
			return fmt.Errorf("files touched by the move have staged changes; commit or unstage them first")
		}
	}

	// Rollback state.
	_, statErr := os.Stat(a.cfg.DstDir)
	dstCreated := os.IsNotExist(statErr)
	var moved []string // names moved so far
	var rewritten []*srcFile
	var aliases []string
	staged := false
	defer func() {
		if err == nil {
			return
		}
		var problems []string
		note := func(e error) {
			if e != nil {
				problems = append(problems, e.Error())
			}
		}
		for _, path := range aliases {
			note(os.Remove(path))
			if staged {
				note(a.git("rm", "--cached", "-q", "--ignore-unmatch", "--", path))
			}
		}
		for _, f := range rewritten {
			if f.Moved {
				continue // restored below, after moving back
			}
			note(os.WriteFile(f.Path, f.Src, 0o644))
			if staged {
				note(a.git("reset", "-q", "--", f.Path))
			}
		}
		for i := len(moved) - 1; i >= 0; i-- {
			src, dst := filepath.Join(a.cfg.SrcDir, moved[i]), filepath.Join(a.cfg.DstDir, moved[i])
			if a.cfg.NoGit {
				note(os.Rename(dst, src))
			} else {
				note(a.git("mv", "-f", "--", dst, src))
				note(a.git("reset", "-q", "--", src))
			}
			if f := a.byPath[src]; f != nil {
				note(os.WriteFile(src, f.Src, 0o644))
			}
		}
		if dstCreated {
			_ = os.Remove(a.cfg.DstDir) // only succeeds if empty
		}
		if len(problems) > 0 {
			err = fmt.Errorf("%v\nrollback incomplete:\n  %s", err, strings.Join(problems, "\n  "))
		} else {
			err = fmt.Errorf("%v (all changes rolled back)", err)
		}
	}()

	if err := os.MkdirAll(a.cfg.DstDir, 0o755); err != nil {
		return err
	}
	for _, name := range moves {
		src, dst := filepath.Join(a.cfg.SrcDir, name), filepath.Join(a.cfg.DstDir, name)
		if a.cfg.NoGit {
			if err := os.Rename(src, dst); err != nil {
				return err
			}
		} else if err := a.git("mv", "--", src, dst); err != nil {
			return err
		}
		moved = append(moved, name)
	}
	var stage []string
	for _, w := range writes {
		rewritten = append(rewritten, w.f)
		if err := os.WriteFile(w.path, w.content, 0o644); err != nil {
			return err
		}
		stage = append(stage, w.path)
	}
	for _, g := range a.plan.AliasFiles {
		path := filepath.Join(a.mod.ModDir, g.Path)
		aliases = append(aliases, path)
		if err := os.WriteFile(path, g.Content, 0o644); err != nil {
			return err
		}
		stage = append(stage, path)
	}
	for _, g := range a.plan.ExtraFiles {
		path := filepath.Join(a.mod.ModDir, g.Path)
		aliases = append(aliases, path)
		if err := os.WriteFile(path, g.Content, 0o644); err != nil {
			return err
		}
		stage = append(stage, path)
	}
	if testHookBeforeStage != nil {
		if err := testHookBeforeStage(); err != nil {
			return err
		}
	}
	if !a.cfg.NoGit && len(stage) > 0 {
		// Stage the whole move (the safety report stays unstaged).
		staged = true
		if err := a.git(append([]string{"add", "--"}, stage...)...); err != nil {
			return err
		}
	}
	return nil
}

// verify runs the post-move sanity checks.
func (a *analysis) verify() error {
	out := a.cfg.Stdout
	printf(out, "\nsanity: go list %s %s\n", a.mod.ImportPath, a.dstImport)
	if _, err := goList(a.mod.ModDir, a.cfg.Tags, "-test", a.mod.ImportPath, a.dstImport); err != nil {
		return fmt.Errorf("post-move go list failed: %v", err)
	}
	printf(out, "sanity: go list OK\n")
	if a.cfg.Typecheck {
		printf(out, "sanity: type-checking both packages (with in-package tests)\n")
		if err := typecheckAfter(a.cfg, a.mod, a.dstImport); err != nil {
			return err
		}
		printf(out, "sanity: type-check OK\n")
	}
	if a.cfg.Vet {
		printf(out, "sanity: go vet\n")
		args := []string{"vet"}
		if len(a.cfg.Tags) > 0 {
			args = append(args, "-tags="+strings.Join(a.cfg.Tags, ","))
		}
		args = append(args, a.mod.ImportPath, a.dstImport)
		cmd := exec.Command("go", args...)
		cmd.Dir = a.mod.ModDir
		if b, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("go vet failed: %v\n%s", err, b)
		}
		printf(out, "sanity: go vet OK\n")
	}
	return nil
}
