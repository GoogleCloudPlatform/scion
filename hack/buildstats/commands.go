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

package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// commonFlags are shared by the measuring subcommands.
type commonFlags struct {
	label  string
	json   string
	cgroup string
}

func (c *commonFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&c.label, "label", "", "free-form label stored in the record (e.g. G1-hub-baseline)")
	fs.StringVar(&c.json, "json", "", "write the JSON record to this file (\"-\" = stdout)")
	fs.StringVar(&c.cgroup, "cgroup", "auto", "cgroup memory.peak file to read before/after (\"auto\", a path, or \"none\")")
}

func (c *commonFlags) cgroupPath() string {
	switch c.cgroup {
	case "auto":
		return defaultCgroupPeak()
	case "none", "":
		return ""
	}
	return c.cgroup
}

func newFlagSet(name, synopsis string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		_, _ = fmt.Fprintf(stderr, "usage: buildstats %s %s\n", name, synopsis)
		fs.PrintDefaults()
	}
	return fs
}

func parseFlags(fs *flag.FlagSet, args []string) error {
	// The flag package has already printed the problem (or the -h usage).
	if fs.Parse(args) != nil {
		return errUsage
	}
	return nil
}

// childArgs returns what follows "--" (flag.Parse stops at "--" and leaves
// the rest in fs.Args()).
func childArgs(fs *flag.FlagSet) ([]string, error) {
	a := fs.Args()
	if len(a) == 0 {
		return nil, errors.New("missing command; put it after --")
	}
	return a, nil
}

func artifactDir(dir string) (string, error) {
	if dir == "" {
		return os.MkdirTemp("", "buildstats-")
	}
	return dir, os.MkdirAll(dir, 0o755)
}

func cmdCompile(args []string, stdout, stderr io.Writer) (int, error) {
	fs := newFlagSet("compile", "[flags] -- go build|test -c [go flags] packages", stderr)
	var c commonFlags
	c.register(fs)
	dir := fs.String("dir", "", "directory for actiongraph.json and bench-<unixnano>.txt (default: a new temp dir, kept)")
	benchPkg := fs.String("bench-pkg", "", "package (import path or pattern as passed to -gcflags) to collect compiler -bench phase timings for")
	top := fs.Int("top", 12, "rows in the actiongraph top table")
	noAG := fs.Bool("no-actiongraph", false, "do not add -debug-actiongraph")
	if err := parseFlags(fs, args); err != nil {
		return 0, err
	}
	argv, err := childArgs(fs)
	if err != nil {
		return 0, err
	}
	d, err := artifactDir(*dir)
	if err != nil {
		return 0, err
	}
	agFile := filepath.Join(d, "actiongraph.json")
	// A unique -bench path per run changes the measured package's action ID,
	// so it is always recompiled; a fixed path would make a repeat run with
	// the same -dir a cache hit with nothing measured.
	benchFile := filepath.Join(d, fmt.Sprintf("bench-%d.txt", time.Now().UnixNano()))
	ij := injection{goflags: os.Getenv("GOFLAGS")}
	if !*noAG {
		ij.actiongraph = agFile
		_ = os.Remove(agFile)
	}
	if *benchPkg != "" {
		ij.benchPkg, ij.benchFile = *benchPkg, benchFile
		// -bench appends; start from an empty file so the record only
		// holds this run.
		_ = os.Remove(benchFile)
	}
	full, err := injectGoFlags(argv, ij)
	if err != nil {
		return 0, err
	}

	rec := newRecord("compile", c.label, full)
	rec.Host.setToolchain(goBinary(full))
	ru, err := measure(full, execOpts{cgroupPeak: c.cgroupPath()}, stderr)
	if err != nil {
		return 0, err
	}
	rec.Rusage = ru
	rec.Artifacts = map[string]string{}
	if !*noAG {
		if s, err := summarizeActiongraphFile(agFile, *top); err == nil {
			rec.Actiongraph = s
			rec.Artifacts["actiongraph"] = agFile
		} else {
			_, _ = fmt.Fprintf(stderr, "buildstats: actiongraph not summarised: %v\n", err)
		}
	}
	if *benchPkg != "" {
		if err := attachBench(rec, benchFile); err != nil {
			_, _ = fmt.Fprintf(stderr, "buildstats: no compiler -bench output for %s: %v\n", *benchPkg, err)
		}
	}
	return ru.ExitCode, finish(rec, c.json, stdout, stderr)
}

// attachBench adds the -bench file's records, the size-normalised figures
// and the measured toolchain (the compiler's own "commit:" line, which is
// authoritative) to rec. rec.Rusage must already be set.
func attachBench(rec *Record, benchFile string) error {
	b, err := parseBenchFile(benchFile)
	if err != nil {
		return err
	}
	if len(b) == 0 {
		return errors.New("no BenchmarkCompile lines (package cached or pattern did not match?)")
	}
	rec.Bench = b
	if rec.Artifacts == nil {
		rec.Artifacts = map[string]string{}
	}
	rec.Artifacts["bench"] = benchFile
	if rec.Rusage != nil {
		rec.Normalized = normalize(rec.Rusage.PeakRSSBytes, b)
	}
	if v := benchGoVersion(benchFile); v != "" && rec.Host != nil {
		rec.Host.GoToolchain, rec.Host.GoToolchainSource = v, "bench"
	}
	return nil
}

func cmdTest(args []string, stdout, stderr io.Writer) (int, error) {
	fs := newFlagSet("test", "[flags] -- <go test -json ... | go tool test2json -t -p NAME BINARY -test.v=test2json ...>", stderr)
	var c commonFlags
	c.register(fs)
	out := fs.String("out", "", "file to capture the command's test2json stdout (default: test2json.json in a new temp dir)")
	top := fs.Int("top", 10, "slowest tests to list per package")
	if err := parseFlags(fs, args); err != nil {
		return 0, err
	}
	argv, err := childArgs(fs)
	if err != nil {
		return 0, err
	}
	o := *out
	if o == "" {
		d, err := artifactDir("")
		if err != nil {
			return 0, err
		}
		o = filepath.Join(d, "test2json.json")
	}
	rec := newRecord("test", c.label, argv)
	rec.Host.setToolchain(goBinary(argv))
	ru, err := measure(argv, execOpts{stdout: o, cgroupPeak: c.cgroupPath()}, stderr)
	if err != nil {
		return 0, err
	}
	rec.Rusage = ru
	rec.Artifacts = map[string]string{"test2json": o}
	ts, err := parseTest2JSONFile(o, *top)
	if err != nil {
		return ru.ExitCode, err
	}
	rec.Tests = ts
	return ru.ExitCode, finish(rec, c.json, stdout, stderr)
}

func cmdRun(args []string, stdout, stderr io.Writer) (int, error) {
	fs := newFlagSet("run", "[flags] -- command [args]", stderr)
	var c commonFlags
	c.register(fs)
	so := fs.String("stdout", "", "redirect the command's stdout to this file")
	if err := parseFlags(fs, args); err != nil {
		return 0, err
	}
	argv, err := childArgs(fs)
	if err != nil {
		return 0, err
	}
	rec := newRecord("run", c.label, argv)
	rec.Host.setToolchain(goBinary(argv))
	ru, err := measure(argv, execOpts{stdout: *so, cgroupPeak: c.cgroupPath()}, stderr)
	if err != nil {
		return 0, err
	}
	rec.Rusage = ru
	return ru.ExitCode, finish(rec, c.json, stdout, stderr)
}

func cmdActiongraph(args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("actiongraph", "[-top N] [-json F] FILE", stderr)
	top := fs.Int("top", 12, "rows in the top table")
	js := fs.String("json", "", "write the JSON record to this file (\"-\" = stdout)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return errUsage
	}
	s, err := summarizeActiongraphFile(fs.Arg(0), *top)
	if err != nil {
		return err
	}
	rec := &Record{Schema: schemaVersion, Kind: "actiongraph", Actiongraph: s, Artifacts: map[string]string{"actiongraph": fs.Arg(0)}}
	return emit(rec, *js, stdout)
}

func cmdBench(args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("bench", "[-json F] FILE", stderr)
	js := fs.String("json", "", "write the JSON record to this file (\"-\" = stdout)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return errUsage
	}
	b, err := parseBenchFile(fs.Arg(0))
	if err != nil {
		return err
	}
	if len(b) == 0 {
		return fmt.Errorf("%s: no BenchmarkCompile lines", fs.Arg(0))
	}
	rec := &Record{Schema: schemaVersion, Kind: "bench", Bench: b, Artifacts: map[string]string{"bench": fs.Arg(0)}}
	return emit(rec, *js, stdout)
}

func cmdTests(args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("tests", "[-top N] [-json F] FILE...", stderr)
	top := fs.Int("top", 10, "slowest tests to list per package")
	js := fs.String("json", "", "write the JSON record to this file (\"-\" = stdout)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		fs.Usage()
		return errUsage
	}
	rec := &Record{Schema: schemaVersion, Kind: "tests"}
	for _, f := range fs.Args() {
		ts, err := parseTest2JSONFile(f, *top)
		if err != nil {
			return err
		}
		rec.Tests = append(rec.Tests, ts...)
	}
	return emit(rec, *js, stdout)
}

func cmdDeps(args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("deps", "[-test] [-json F] [-label L] PKG...", stderr)
	test := fs.Bool("test", false, "count the test binary's closure (go list -deps -test)")
	js := fs.String("json", "", "write the JSON record to this file (\"-\" = stdout)")
	label := fs.String("label", "", "free-form label stored in the record")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		fs.Usage()
		return errUsage
	}
	argv := depsArgs(fs.Args(), *test)
	cmd := exec.Command(argv[0], argv[1:]...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", strings.Join(argv, " "), err)
	}
	d, err := parseGoList(&buf, *test)
	if err != nil {
		return err
	}
	rec := newRecord("deps", *label, argv)
	rec.Host.setToolchain(goBinary(argv))
	rec.Deps = d
	return emit(rec, *js, stdout)
}

// emit prints the table to stdout, unless the JSON goes to stdout, in which
// case the table goes nowhere (pipe-friendly).
func emit(rec *Record, js string, stdout io.Writer) error {
	var printErr error
	if js != "-" {
		printErr = printRecord(stdout, rec)
	}
	return errors.Join(writeJSON(rec, js, stdout), printErr)
}

// finish is emit for the measuring subcommands: the table goes to stderr
// (stdout may carry the child's output or the JSON), and the JSON record is
// written even if printing the table failed.
func finish(rec *Record, js string, stdout, stderr io.Writer) error {
	printErr := printRecord(stderr, rec)
	return errors.Join(writeJSON(rec, js, stdout), printErr)
}

// injection describes the flags compile adds to a go build / go test -c.
type injection struct {
	actiongraph string // -debug-actiongraph file, "" = none
	benchPkg    string // -gcflags pattern to add -bench to, "" = none
	benchFile   string
	goflags     string // value of $GOFLAGS, consulted for -gcflags
	// resolve maps a package argument or pattern to an import path (so
	// "./pkg/hub" and "example.com/m/pkg/hub" compare equal). nil means
	// resolveImportPath (go.mod lookup from the working directory).
	resolve func(string) string
}

// goValueFlags are go build/test flags that take a separate value when not
// written as -flag=value. Any other flag is treated as boolean when finding
// the positional package arguments.
var goValueFlags = map[string]bool{
	"C": true, "o": true, "p": true, "asmflags": true, "buildmode": true, "compiler": true,
	"gccgoflags": true, "gcflags": true, "installsuffix": true, "ldflags": true, "mod": true,
	"modfile": true, "overlay": true, "pgo": true, "pkgdir": true, "tags": true, "toolexec": true,
	"exec": true, "coverpkg": true, "covermode": true, "coverprofile": true, "outputdir": true,
	"run": true, "skip": true, "bench": true, "benchtime": true, "count": true, "cpu": true,
	"timeout": true, "parallel": true, "list": true, "shuffle": true, "fuzz": true, "fuzztime": true,
	"fuzzminimizetime": true, "cpuprofile": true, "memprofile": true, "memprofilerate": true,
	"blockprofile": true, "blockprofilerate": true, "mutexprofile": true, "mutexprofilefraction": true,
	"trace": true, "vet": true, "debug-actiongraph": true, "debug-runtime-trace": true, "debug-trace": true,
}

// goPackageArgs returns the positional package arguments of a go build/test
// command line (args after the subcommand). It stops at "-args" or "--".
func goPackageArgs(args []string) []string {
	var pkgs []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-args" || a == "--args" || a == "--" {
			break
		}
		if !strings.HasPrefix(a, "-") {
			pkgs = append(pkgs, a)
			continue
		}
		name := strings.TrimLeft(a, "-")
		if strings.Contains(name, "=") {
			continue
		}
		if goValueFlags[name] {
			i++ // skip the value
		}
	}
	return pkgs
}

// injectGoFlags adds -debug-actiongraph and a per-package -gcflags carrying
// -bench to argv, which must be "go build ..." or "go test ...".
//
// cmd/go applies, per package, only the LAST -gcflags whose pattern matches
// (GOFLAGS first, then the command line); values do not accumulate. An
// unpatterned -gcflags applies only to the packages named on the command
// line. So the added -gcflags must (a) repeat the flags that would otherwise
// have applied to the bench package (e.g. GOFLAGS=-gcflags=-c=1, but only if
// the bench package is named on the command line), and (b) come after every
// -gcflags already on the command line.
func injectGoFlags(argv []string, ij injection) ([]string, error) {
	if len(argv) < 2 || filepath.Base(argv[0]) != "go" || (argv[1] != "build" && argv[1] != "test") {
		if ij.actiongraph == "" && ij.benchPkg == "" {
			return argv, nil
		}
		return nil, fmt.Errorf("compile expects \"go build ...\" or \"go test -c ...\", got %q", strings.Join(argv, " "))
	}
	if ij.benchPkg != "" && strings.ContainsAny(ij.benchPkg+ij.benchFile, " \t\n") {
		return nil, fmt.Errorf("bench package %q or bench file %q contains whitespace, which would split the -gcflags value; use a -dir without spaces", ij.benchPkg, ij.benchFile)
	}
	resolve := ij.resolve
	if resolve == nil {
		resolve = resolveImportPath
	}
	named := false
	if ij.benchPkg != "" {
		want := resolve(ij.benchPkg)
		for _, p := range goPackageArgs(argv[2:]) {
			if matchPattern(resolve(p), want) {
				named = true
				break
			}
		}
	}
	applies := func(value string) (string, bool) {
		return gcflagsFor(value, ij.benchPkg, named, resolve)
	}

	base := ""
	for _, f := range strings.Fields(ij.goflags) {
		if v, ok := gcflagsValue(f); ok {
			if a, ok := applies(v); ok {
				base = a
			}
		}
	}
	lastGC := -1 // index of the last -gcflags argument (its value, if separate)
	for i := 2; i < len(argv); i++ {
		a := argv[i]
		if a == "--" || a == "-args" || a == "--args" {
			break
		}
		if strings.HasPrefix(a, "-debug-actiongraph") || strings.HasPrefix(a, "--debug-actiongraph") {
			if ij.actiongraph != "" {
				return nil, errors.New("command already has -debug-actiongraph; drop it or pass -no-actiongraph")
			}
		}
		if a == "-gcflags" || a == "--gcflags" {
			if i+1 >= len(argv) {
				return nil, errors.New("-gcflags without a value")
			}
			if g, ok := applies(argv[i+1]); ok {
				base = g
			}
			i++
			lastGC = i
			continue
		}
		if v, ok := gcflagsValue(a); ok {
			if g, ok := applies(v); ok {
				base = g
			}
			lastGC = i
		}
	}

	out := append([]string{}, argv[:2]...)
	if ij.actiongraph != "" {
		out = append(out, "-debug-actiongraph="+ij.actiongraph)
	}
	benchFlag := ""
	if ij.benchPkg != "" {
		v := strings.TrimSpace(base + " -bench=" + ij.benchFile)
		benchFlag = "-gcflags=" + ij.benchPkg + "=" + v
	}
	rest := argv[2:]
	if benchFlag == "" {
		return append(out, rest...), nil
	}
	if lastGC < 0 {
		out = append(out, benchFlag)
		return append(out, rest...), nil
	}
	cut := lastGC - 2 + 1
	out = append(out, rest[:cut]...)
	out = append(out, benchFlag)
	return append(out, rest[cut:]...), nil
}

// gcflagsValue returns V for "-gcflags=V" / "--gcflags=V".
func gcflagsValue(arg string) (string, bool) {
	for _, p := range []string{"-gcflags=", "--gcflags="} {
		if strings.HasPrefix(arg, p) {
			return strings.TrimPrefix(arg, p), true
		}
	}
	return "", false
}

// gcflagsFor returns the compiler args of a -gcflags value if cmd/go would
// apply it to pkg: an unpatterned value only when pkg is named on the command
// line (named), a patterned one when its pattern is "all" or matches pkg
// (exactly, or as a "/..." prefix) after resolving relative paths.
func gcflagsFor(value, pkg string, named bool, resolve func(string) string) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" || strings.HasPrefix(value, "-") {
		return value, named
	}
	pat, args, ok := strings.Cut(value, "=")
	if !ok {
		return "", false
	}
	if pat == "all" || matchPattern(resolve(pat), resolve(pkg)) {
		return args, true
	}
	return "", false
}

// matchPattern reports whether package pattern pat matches import path pkg:
// equal, or pat is "X/..." and pkg is X or below it. Other wildcard forms
// are treated as not matching.
func matchPattern(pat, pkg string) bool {
	if prefix, ok := strings.CutSuffix(pat, "/..."); ok {
		return pkg == prefix || strings.HasPrefix(pkg, prefix+"/")
	}
	return pat == pkg
}

// resolveImportPath maps a relative package path ("./pkg/hub", "../x/...",
// ".") to an import path using the nearest go.mod above the working
// directory, without running the go command. Other arguments are returned
// unchanged, as is anything that cannot be resolved.
func resolveImportPath(arg string) string {
	if arg != "." && arg != ".." && !strings.HasPrefix(arg, "./") && !strings.HasPrefix(arg, "../") && !filepath.IsAbs(arg) {
		return arg
	}
	suffix := ""
	if p, ok := strings.CutSuffix(arg, "/..."); ok {
		arg, suffix = p, "/..."
	}
	abs, err := filepath.Abs(arg)
	if err != nil {
		return arg + suffix
	}
	root, mod := findModule(abs)
	if root == "" {
		return arg + suffix
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil || strings.HasPrefix(rel, "..") {
		return arg + suffix
	}
	if rel == "." {
		return mod + suffix
	}
	return mod + "/" + filepath.ToSlash(rel) + suffix
}

// findModule walks up from dir to the nearest go.mod and returns its
// directory and module path ("" if none).
func findModule(dir string) (root, module string) {
	for d := dir; ; d = filepath.Dir(d) {
		if b, err := os.ReadFile(filepath.Join(d, "go.mod")); err == nil {
			for _, line := range strings.Split(string(b), "\n") {
				if m, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
					return d, strings.Trim(strings.TrimSpace(m), `"`)
				}
			}
			return "", ""
		}
		if filepath.Dir(d) == d {
			return "", ""
		}
	}
}
