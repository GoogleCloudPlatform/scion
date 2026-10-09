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

package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
)

const (
	policyTestScripted = "harness: generic\nimage: scion-base:test\nuser: scion\nprovisioner:\n  type: container-script\n  interface_version: 1\n  command: [\"python3\", \"/home/scion/.scion/harness/provision.py\"]\n"
	policyTestDecl     = "harness: generic\nimage: scion-base:test\nuser: scion\n"
)

// policyTestEnv is an initialized machine and project for harness-config
// policy tests. root is the project root; scion is <root>/.scion.
type policyTestEnv struct {
	root, scion string
}

func newPolicyTestEnv(t *testing.T) *policyTestEnv {
	t.Helper()
	mockRuntimeForTest(t)
	tmp := t.TempDir()
	oldWd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldWd) })
	t.Setenv("HOME", tmp)
	if err := config.InitMachine(getTestHarnesses()); err != nil {
		t.Fatalf("InitMachine: %v", err)
	}
	root := filepath.Join(tmp, "project")
	scion := filepath.Join(root, ".scion")
	if err := config.InitProject(scion, getTestHarnesses()); err != nil {
		t.Fatalf("InitProject: %v", err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	return &policyTestEnv{root: root, scion: scion}
}

func writePolicyHC(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, "provisioner:") {
		if err := os.WriteFile(filepath.Join(dir, "provision.py"), []byte("#!/usr/bin/env python3\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// projectHC writes harness-configs/<name> in the project's resolved dir.
func (e *policyTestEnv) projectHC(t *testing.T, name, body string) {
	writePolicyHC(t, filepath.Join(e.scion, "harness-configs", name), body)
}

// template writes templates/<name>/scion-agent.yaml in the project.
func (e *policyTestEnv) template(t *testing.T, name, agentYAML string) string {
	t.Helper()
	dir := filepath.Join(e.scion, "templates", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "scion-agent.yaml"), []byte(agentYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// recordingPolicy records each evaluation and refuses container-script
// entries when refuse is set.
type recordingPolicy struct {
	mu     sync.Mutex
	refuse bool
	calls  []string // "<name>:<scripted|declarative>"
}

var errTestPolicyRefusal = errors.New("test policy refusal")

func (p *recordingPolicy) fn(name string, entry config.HarnessConfigEntry) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	kind := "declarative"
	if entry.Provisioner != nil {
		kind = "scripted"
	}
	p.calls = append(p.calls, name+":"+kind)
	if p.refuse && entry.Provisioner != nil {
		return errTestPolicyRefusal
	}
	return nil
}

func (p *recordingPolicy) ctx() context.Context {
	return config.ContextWithHarnessConfigPolicy(context.Background(), p.fn)
}

func (p *recordingPolicy) recorded() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.calls...)
}

func assertPolicyRefused(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, ErrHarnessConfigPolicy) || !errors.Is(err, errTestPolicyRefusal) {
		t.Fatalf("expected a refusal wrapping ErrHarnessConfigPolicy and the policy's error, got %v", err)
	}
}

func policyTestManager(runs *int) Manager {
	return NewManager(&runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, cfg runtime.RunConfig) (string, error) {
			if runs != nil {
				*runs++
			}
			return "mock-id", nil
		},
	})
}

// With no policy attached (the solo CLI path), a container-script
// harness-config launches as usual and its bundle is staged.
func TestHarnessConfigPolicy_NoPolicyLaunchesContainerScript(t *testing.T) {
	e := newPolicyTestEnv(t)
	e.projectHC(t, "hc-scripted", policyTestScripted)
	runs := 0
	if _, err := policyTestManager(&runs).Start(context.Background(), api.StartOptions{
		Name: "solo", ProjectPath: e.scion, HarnessConfig: "hc-scripted", NoAuth: true,
	}); err != nil {
		t.Fatalf("Start without a policy: %v", err)
	}
	if runs != 1 {
		t.Errorf("expected the container to run once, got %d", runs)
	}
	staged := filepath.Join(config.GetAgentHomePath(e.scion, "solo"), ".scion", "harness", "provision.py")
	if _, err := os.Stat(staged); err != nil {
		t.Errorf("container-script bundle not staged: %v", err)
	}
}

// The policy is evaluated at every point launch resolves the harness-config:
// template and harness-config resolution, ProvisionAgent's harness
// construction, and Start's harness construction.
func TestHarnessConfigPolicy_EvaluatedAtEveryResolutionPoint(t *testing.T) {
	e := newPolicyTestEnv(t)
	e.projectHC(t, "hc-decl", policyTestDecl)

	p := &recordingPolicy{}
	if _, err := policyTestManager(nil).Start(p.ctx(), api.StartOptions{
		Name: "all-points", ProjectPath: e.scion, HarnessConfig: "hc-decl", NoAuth: true,
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	want := []string{"hc-decl:declarative", "hc-decl:declarative", "hc-decl:declarative"}
	if got := p.recorded(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("policy evaluations = %v, want %v (resolution, provisioning harness, Start harness)", got, want)
	}
}

// C1a: the staged bundle comes from the resolved project dir.
func TestHarnessConfigPolicy_StagedBundleFromResolvedProjectDir(t *testing.T) {
	e := newPolicyTestEnv(t)
	writePolicyHC(t, filepath.Join(e.scion, "harness-configs", "hc"), policyTestScripted+"# copy: resolved\n")
	writePolicyHC(t, filepath.Join(e.root, "harness-configs", "hc"), policyTestScripted+"# copy: raw\n")

	if _, _, _, err := ProvisionAgent(context.Background(), "staged", "", "", "hc", e.root, "", "", "", ""); err != nil {
		t.Fatalf("ProvisionAgent: %v", err)
	}
	staged, err := os.ReadFile(filepath.Join(config.GetAgentHomePath(e.scion, "staged"), ".scion", "harness", "config.yaml"))
	if err != nil {
		t.Fatalf("read staged config.yaml: %v", err)
	}
	if !strings.Contains(string(staged), "# copy: resolved") {
		t.Errorf("staged bundle is not the <root>/.scion copy:\n%s", staged)
	}
}

// C1b: with no template on the request, launch resolves the default
// template's chain; a harness-config bundled there is what runs, and the
// policy evaluates it.
func TestHarnessConfigPolicy_DefaultTemplateBundledConfig(t *testing.T) {
	e := newPolicyTestEnv(t)
	e.projectHC(t, "hc", policyTestDecl)
	chain, err := config.GetTemplateChainInProject("default", e.scion)
	if err != nil || len(chain) == 0 {
		t.Fatalf("default template chain: %v", err)
	}
	writePolicyHC(t, filepath.Join(chain[len(chain)-1].Path, "harness-configs", "hc"), policyTestScripted)

	p := &recordingPolicy{refuse: true}
	assertPolicyRefused(t, PreflightResolve(p.ctx(), api.StartOptions{Name: "c1b", ProjectPath: e.scion, HarnessConfig: "hc"}))
}

// C2: the template names the harness-config; the request does not.
func TestHarnessConfigPolicy_TemplateNamedConfig(t *testing.T) {
	e := newPolicyTestEnv(t)
	e.projectHC(t, "hc-scripted", policyTestScripted)
	e.template(t, "tplx", "harness_config: hc-scripted\n")

	p := &recordingPolicy{refuse: true}
	assertPolicyRefused(t, PreflightResolve(p.ctx(), api.StartOptions{Name: "c2", ProjectPath: e.scion, Template: "tplx"}))
	if got := p.recorded(); len(got) == 0 || got[0] != "hc-scripted:scripted" {
		t.Errorf("policy evaluated %v, want hc-scripted first", got)
	}
}

// C3: the dispatch's inline config names the harness-config.
func TestHarnessConfigPolicy_InlineConfigNamedConfig(t *testing.T) {
	e := newPolicyTestEnv(t)
	e.projectHC(t, "hc-scripted", policyTestScripted)

	p := &recordingPolicy{refuse: true}
	err := PreflightResolve(p.ctx(), api.StartOptions{
		Name: "c3", ProjectPath: e.scion,
		InlineConfig: &api.ScionConfig{HarnessConfig: "hc-scripted"},
	})
	assertPolicyRefused(t, err)
}

// provisionExisting provisions agent name with harness-config hcName (no
// policy), leaving an existing agent for start/restart tests.
func (e *policyTestEnv) provisionExisting(t *testing.T, name, hcName string) {
	t.Helper()
	if _, _, _, err := ProvisionAgent(context.Background(), name, "", "", hcName, e.scion, "", "", "", ""); err != nil {
		t.Fatalf("ProvisionAgent: %v", err)
	}
}

// setStoredHarnessConfig rewrites scion-agent.json's harness_config, as a
// start's inline config update (applyInlineConfigUpdate) does.
func (e *policyTestEnv) setStoredHarnessConfig(t *testing.T, name, hcName string) {
	t.Helper()
	path := filepath.Join(e.scion, "agents", name, "scion-agent.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	cfg["harness_config"] = hcName
	out, _ := json.Marshal(cfg)
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}
}

// S2: a start whose request names no harness-config uses the stored config,
// as updated by the start's inline config; the policy evaluates that name.
func TestHarnessConfigPolicy_StartUsesStoredConfig(t *testing.T) {
	e := newPolicyTestEnv(t)
	e.projectHC(t, "hc-decl", policyTestDecl)
	e.projectHC(t, "hc-scripted", policyTestScripted)
	e.provisionExisting(t, "s2", "hc-decl")
	e.setStoredHarnessConfig(t, "s2", "hc-scripted")

	runs := 0
	p := &recordingPolicy{refuse: true}
	_, err := policyTestManager(&runs).Start(p.ctx(), api.StartOptions{Name: "s2", ProjectPath: e.scion, NoAuth: true})
	assertPolicyRefused(t, err)
	if runs != 0 {
		t.Error("the container must not run when the policy refuses")
	}
}

// R1: a restart-style start (no harness-config on the request) of an agent
// whose agent-info.json carries no harness-config uses scion-agent.json's.
func TestHarnessConfigPolicy_RestartUsesStoredConfigWithoutAgentInfo(t *testing.T) {
	e := newPolicyTestEnv(t)
	e.projectHC(t, "hc-scripted", policyTestScripted)
	e.provisionExisting(t, "r1", "hc-scripted")
	if err := os.Remove(filepath.Join(config.GetAgentHomePath(e.scion, "r1"), "agent-info.json")); err != nil {
		t.Fatal(err)
	}

	runs := 0
	p := &recordingPolicy{refuse: true}
	_, err := policyTestManager(&runs).Start(p.ctx(), api.StartOptions{Name: "r1", ProjectPath: e.scion, NoAuth: true})
	assertPolicyRefused(t, err)
	if runs != 0 {
		t.Error("the container must not run when the policy refuses")
	}
}

// S3/R2: starting an agent whose directory does not exist provisions it
// through the create resolution, which evaluates the policy.
func TestHarnessConfigPolicy_StartOfMissingAgentProvisions(t *testing.T) {
	e := newPolicyTestEnv(t)
	e.projectHC(t, "hc-scripted", policyTestScripted)
	e.template(t, "tplx", "harness_config: hc-scripted\n")

	runs := 0
	p := &recordingPolicy{refuse: true}
	_, err := policyTestManager(&runs).Start(p.ctx(), api.StartOptions{Name: "s3", ProjectPath: e.scion, Template: "tplx", NoAuth: true})
	assertPolicyRefused(t, err)
	if runs != 0 {
		t.Error("the container must not run when the policy refuses")
	}
}

const harnessImportPath = "github.com/GoogleCloudPlatform/scion/pkg/harness"

// TestHarnessConfigPolicy_ResolveCallSitesAreHooked guards completeness. It
// scans every non-test Go file under pkg/ and cmd/ (except pkg/harness
// itself) for harness constructions from a harness-config: calls to
// pkg/harness's Resolve or NewContainerScriptHarness, found by import path,
// so an aliased or dot import is covered. Each one must be a listed call site
// (NewContainerScriptHarness has none outside pkg/harness), and the policy
// check must be tied to it: a CheckHarnessConfigPolicy call later in the same
// function whose entry argument is the constructed harness's .Config (the
// variable the Resolve result is assigned to). resolveTemplateAndHarnessConfig
// must likewise evaluate EffectiveConfig of the dir it resolves.
//
// The tie is syntactic (position and argument), not control-flow: it does not
// prove the check is on every path, which the behavioural tests in this file
// cover. A new call site fails this test until it is hooked and listed.
func TestHarnessConfigPolicy_ResolveCallSitesAreHooked(t *testing.T) {
	allowed := map[string]bool{
		"pkg/agent/provision.go:ProvisionAgent": true,
		"pkg/agent/run.go:Start":                true,
	}
	root := mustAbs(t, filepath.Join("..", ".."))
	found := map[string]bool{}
	sawTemplateResolution := false

	for _, top := range []string{"pkg", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if path == filepath.Join(root, "pkg", "harness") || d.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, _ := filepath.Rel(root, path)
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			local, dot := harnessImportName(file)
			if local == "" && !dot && !strings.HasPrefix(rel, filepath.Join("pkg", "agent")+string(filepath.Separator)) {
				return nil
			}
			checkNoIndirectConstruction(t, file, filepath.ToSlash(rel), local, dot)
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				site := filepath.ToSlash(rel) + ":" + fn.Name.Name
				checkFunctionHooks(t, fn, site, local, dot, allowed, found)
				if fn.Name.Name == "resolveTemplateAndHarnessConfig" {
					sawTemplateResolution = true
					checkTemplateResolutionHooked(t, fn)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for site := range allowed {
		if !found[site] {
			t.Errorf("listed call site %s does not construct a harness; update this test", site)
		}
	}
	if !sawTemplateResolution {
		t.Error("resolveTemplateAndHarnessConfig not found; update this test")
	}
}

// checkNoIndirectConstruction flags, outside pkg/harness, every way to reach
// a container-script harness other than a direct call inside a function
// body (which checkFunctionHooks then ties to the policy check): a reference
// to harness.Resolve or harness.NewContainerScriptHarness that is not the
// callee of a call (a function value), any reference in a package-level
// declaration (var initializers, including function literals), and a
// ContainerScriptHarness composite literal or new(ContainerScriptHarness).
func checkNoIndirectConstruction(t *testing.T, file *ast.File, rel, local string, dot bool) {
	t.Helper()
	if local == "" && !dot {
		return
	}
	isRef := func(n ast.Node, names ...string) bool {
		switch x := n.(type) {
		case *ast.SelectorExpr:
			if id, ok := x.X.(*ast.Ident); ok && local != "" && id.Name == local {
				for _, name := range names {
					if x.Sel.Name == name {
						return true
					}
				}
			}
		case *ast.Ident:
			if dot {
				for _, name := range names {
					if x.Name == name {
						return true
					}
				}
			}
		}
		return false
	}
	for _, decl := range file.Decls {
		inFunc := false
		if fn, ok := decl.(*ast.FuncDecl); ok {
			if fn.Body == nil {
				continue
			}
			inFunc = true
		}
		callees := map[ast.Node]bool{}
		ast.Inspect(decl, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				callees[call.Fun] = true
			}
			return true
		})
		ast.Inspect(decl, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.SelectorExpr, *ast.Ident:
				if isRef(x, "Resolve", "NewContainerScriptHarness") {
					switch {
					case !inFunc:
						t.Errorf("%s: package-level reference to pkg/harness's harness construction; construct harnesses inside a hooked function", rel)
					case !callees[x]:
						t.Errorf("%s: pkg/harness's harness construction used as a function value; call it directly at a hooked call site", rel)
					}
				}
			case *ast.CompositeLit:
				if x.Type != nil && isRef(x.Type, "ContainerScriptHarness") {
					t.Errorf("%s: ContainerScriptHarness literal outside pkg/harness; build it through harness.Resolve", rel)
				}
			case *ast.CallExpr:
				if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "new" && len(x.Args) == 1 && isRef(x.Args[0], "ContainerScriptHarness") {
					t.Errorf("%s: new(ContainerScriptHarness) outside pkg/harness; build it through harness.Resolve", rel)
				}
			}
			return true
		})
	}
}

// harnessImportName returns the identifier file uses for pkg/harness ("" if
// it does not import it) and whether it is dot-imported.
func harnessImportName(file *ast.File) (string, bool) {
	for _, imp := range file.Imports {
		if strings.Trim(imp.Path.Value, `"`) != harnessImportPath {
			continue
		}
		if imp.Name == nil {
			return "harness", false
		}
		switch imp.Name.Name {
		case ".":
			return "", true
		case "_":
			return "", false
		default:
			return imp.Name.Name, false
		}
	}
	return "", false
}

// isHarnessCall reports whether call invokes pkg/harness's function name,
// given how the file imports pkg/harness.
func isHarnessCall(call *ast.CallExpr, local string, dot bool, names ...string) (string, bool) {
	switch f := call.Fun.(type) {
	case *ast.SelectorExpr:
		if x, ok := f.X.(*ast.Ident); ok && local != "" && x.Name == local {
			for _, n := range names {
				if f.Sel.Name == n {
					return n, true
				}
			}
		}
	case *ast.Ident:
		if dot {
			for _, n := range names {
				if f.Name == n {
					return n, true
				}
			}
		}
	}
	return "", false
}

func isPolicyCheck(call *ast.CallExpr) bool {
	switch f := call.Fun.(type) {
	case *ast.Ident:
		return f.Name == "CheckHarnessConfigPolicy"
	case *ast.SelectorExpr:
		return f.Sel.Name == "CheckHarnessConfigPolicy"
	}
	return false
}

// assignedVar returns the first left-hand identifier of the assignment whose
// right-hand side is call, or "".
func assignedVar(body *ast.BlockStmt, call *ast.CallExpr) string {
	name := ""
	ast.Inspect(body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Rhs) != 1 || as.Rhs[0] != call || len(as.Lhs) == 0 {
			return true
		}
		if id, ok := as.Lhs[0].(*ast.Ident); ok {
			name = id.Name
		}
		return false
	})
	return name
}

func checkFunctionHooks(t *testing.T, fn *ast.FuncDecl, site, local string, dot bool, allowed, found map[string]bool) {
	t.Helper()
	var checks []*ast.CallExpr
	type construction struct {
		call *ast.CallExpr
		name string
	}
	var constructions []construction
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if isPolicyCheck(call) {
			checks = append(checks, call)
		}
		if name, ok := isHarnessCall(call, local, dot, "Resolve", "NewContainerScriptHarness"); ok {
			constructions = append(constructions, construction{call, name})
		}
		return true
	})
	for _, c := range constructions {
		found[site] = true
		if c.name == "NewContainerScriptHarness" {
			t.Errorf("%s constructs a container-script harness directly; build it through harness.Resolve at a hooked call site", site)
			continue
		}
		if !allowed[site] {
			t.Errorf("unlisted harness construction at %s: evaluate the harness-config policy there (CheckHarnessConfigPolicy) and list it in this test", site)
			continue
		}
		v := assignedVar(fn.Body, c.call)
		var tied *ast.CallExpr
		for _, chk := range checks {
			if chk.Pos() <= c.call.End() || len(chk.Args) != 3 {
				continue
			}
			if sel, ok := chk.Args[2].(*ast.SelectorExpr); ok && sel.Sel.Name == "Config" {
				if x, ok := sel.X.(*ast.Ident); ok && v != "" && x.Name == v && tied == nil {
					tied = chk
				}
			}
		}
		if tied == nil {
			t.Errorf("%s: harness.Resolve result %q is not followed by CheckHarnessConfigPolicy(ctx, name, %s.Config)", site, v, v)
			continue
		}
		checkWrapperStagedOnlyWhenAllowed(t, fn, site, tied)
	}
}

// checkWrapperStagedOnlyWhenAllowed requires, at a hooked call site, that
// the harness is provisioned (which is where a container-script harness
// stages its provisioner wrapper) only after the policy check, and that a
// stale wrapper is cleared for a non-container-script harness
// (resetStagedProvisioning) after the check. Start must also
// route a Resolve error through harnessAfterResolveError.
func checkWrapperStagedOnlyWhenAllowed(t *testing.T, fn *ast.FuncDecl, site string, check *ast.CallExpr) {
	t.Helper()
	var cleared, routedErr bool
	var resetPos, firstProvisionPos token.Pos
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
			if x, ok := sel.X.(*ast.Ident); ok && x.Name == "h" && sel.Sel.Name == "Provision" && (firstProvisionPos == token.NoPos || call.Pos() < firstProvisionPos) {
				firstProvisionPos = call.Pos()
			}
		}
		switch f := call.Fun.(type) {
		case *ast.Ident:
			switch f.Name {
			case "resetStagedProvisioning":
				if call.Pos() > check.End() {
					cleared = true
					resetPos = call.Pos()
				}
			case "harnessAfterResolveError":
				routedErr = true
			}
		case *ast.SelectorExpr:
			if x, ok := f.X.(*ast.Ident); ok && x.Name == "h" && f.Sel.Name == "Provision" && call.Pos() < check.End() {
				t.Errorf("%s provisions the harness (staging any provisioner wrapper) before the policy check", site)
			}
		}
		return true
	})
	if !cleared {
		t.Errorf("%s does not reset staged provisioning (resetStagedProvisioning) after the policy check", site)
	} else if firstProvisionPos != token.NoPos && resetPos > firstProvisionPos {
		t.Errorf("%s provisions the harness before resetting staged provisioning", site)
	}
	if fn.Name.Name == "Start" && !routedErr {
		t.Errorf("%s does not route a harness.Resolve error through harnessAfterResolveError", site)
	}
}

// TestHarnessConfigPolicy_WrapperWrittenOnlyByContainerScriptProvision
// guards that pkg/harness stages the provisioner wrapper only from
// (*ContainerScriptHarness).Provision, the harness a hooked call site builds
// after the policy allowed it.
func TestHarnessConfigPolicy_WrapperWrittenOnlyByContainerScriptProvision(t *testing.T) {
	dir := filepath.Join("..", "harness")
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	var callers []string
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "writeHookWrapper" {
						recv := ""
						if fn.Recv != nil && len(fn.Recv.List) == 1 {
							if star, ok := fn.Recv.List[0].Type.(*ast.StarExpr); ok {
								if id, ok := star.X.(*ast.Ident); ok {
									recv = id.Name
								}
							}
						}
						callers = append(callers, recv+"."+fn.Name.Name)
					}
				}
				return true
			})
		}
	}
	if len(callers) != 1 || callers[0] != "ContainerScriptHarness.Provision" {
		t.Errorf("writeHookWrapper callers = %v, want only ContainerScriptHarness.Provision", callers)
	}
}

// checkTemplateResolutionHooked requires resolveTemplateAndHarnessConfig to
// evaluate the policy, after resolving the harness-config dir, on
// EffectiveConfig of that dir.
func checkTemplateResolutionHooked(t *testing.T, fn *ast.FuncDecl) {
	t.Helper()
	var resolveCall *ast.CallExpr
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "resolveHarnessConfigDir" {
				resolveCall = call
			}
		}
		return true
	})
	if resolveCall == nil {
		t.Fatal("resolveTemplateAndHarnessConfig no longer calls resolveHarnessConfigDir; update this test")
	}
	dirVar := assignedVar(fn.Body, resolveCall)
	tied := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || !isPolicyCheck(call) || call.Pos() <= resolveCall.End() || len(call.Args) != 3 {
			return true
		}
		inner, ok := call.Args[2].(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := inner.Fun.(*ast.SelectorExpr); !ok || sel.Sel.Name != "EffectiveConfig" {
			return true
		}
		for _, a := range inner.Args {
			if id, ok := a.(*ast.Ident); ok && id.Name == dirVar {
				tied = true
			}
		}
		return true
	})
	if !tied {
		t.Errorf("resolveTemplateAndHarnessConfig does not evaluate CheckHarnessConfigPolicy on EffectiveConfig(..., %s, ...) after resolving it", dirVar)
	}
}

func mustAbs(t *testing.T, p string) string {
	t.Helper()
	a, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func wrapperPath(e *policyTestEnv, agentName string) string {
	return filepath.Join(config.GetAgentHomePath(e.scion, agentName), ".scion", "hooks", "pre-start.d", "20-harness-provision")
}

func wrapperStaged(e *policyTestEnv, agentName string) bool {
	_, err := os.Stat(wrapperPath(e, agentName))
	return err == nil
}

// (iii) With no policy (the solo CLI), a container-script launch stages its
// provisioner wrapper as before.
func TestHarnessConfigPolicy_NoPolicyStagesWrapper(t *testing.T) {
	e := newPolicyTestEnv(t)
	e.projectHC(t, "hc-scripted", policyTestScripted)
	if _, err := policyTestManager(nil).Start(context.Background(), api.StartOptions{
		Name: "solo-wrap", ProjectPath: e.scion, HarnessConfig: "hc-scripted", NoAuth: true,
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !wrapperStaged(e, "solo-wrap") {
		t.Error("container-script launch without a policy must stage the provisioner wrapper")
	}
}

// (ii) A relaunch whose resolved harness is not container-script clears a
// wrapper staged by an earlier container-script run, on start and on a
// restart-style relaunch, with or without a policy.
func TestHarnessConfigPolicy_NonContainerScriptRelaunchClearsWrapper(t *testing.T) {
	for _, tc := range []struct {
		name      string
		withPol   bool
		relaunch2 bool // relaunch a second time (restart after a start)
	}{
		{"start, policy attached", true, false},
		{"restart, policy attached", true, true},
		{"start, no policy", false, false},
		{"restart, no policy", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newPolicyTestEnv(t)
			e.projectHC(t, "hc-scripted", policyTestScripted)
			e.projectHC(t, "hc-decl", policyTestDecl)
			mgr := policyTestManager(nil)
			if _, err := mgr.Start(context.Background(), api.StartOptions{Name: "relaunch", ProjectPath: e.scion, HarnessConfig: "hc-scripted", NoAuth: true}); err != nil {
				t.Fatalf("first Start: %v", err)
			}
			if !wrapperStaged(e, "relaunch") {
				t.Fatal("fixture: wrapper should be staged by the container-script run")
			}
			// What a provisioner run would have left: its env overlay output.
			home := config.GetAgentHomePath(e.scion, "relaunch")
			writeStagedOutputs(t, home)
			if req, err := hooks.LoadHarnessManifestRequirement(home); err != nil || !req.Required {
				t.Fatalf("fixture: container-script run should stage a required manifest (req=%+v err=%v)", req, err)
			}
			ctx := context.Background()
			if tc.withPol {
				ctx = (&recordingPolicy{refuse: true}).ctx()
			}
			opts := api.StartOptions{Name: "relaunch", ProjectPath: e.scion, HarnessConfig: "hc-decl", NoAuth: true}
			if _, err := mgr.Start(ctx, opts); err != nil {
				t.Fatalf("relaunch: %v", err)
			}
			if tc.relaunch2 {
				if _, err := mgr.Start(ctx, opts); err != nil {
					t.Fatalf("second relaunch: %v", err)
				}
			}
			if wrapperStaged(e, "relaunch") {
				t.Error("a non-container-script relaunch must clear the stale provisioner wrapper")
			}
			assertNoStagedProvisioning(t, config.GetAgentHomePath(e.scion, "relaunch"))
		})
	}
}

// (i) After a harness.Resolve error, with a policy attached and a wrapper
// staged, Start refuses; with no policy, or nothing staged, it clears the
// staged provisioning and falls back to harness.New.
//
// An unusable provisioner (legacy "builtin", empty command) fails Start
// before this decision (TestStart_UnusableProvisionerFailsLaunch). Otherwise
// harness.Resolve fails only for a container-script entry without an
// on-disk directory. Start cannot reach that today: entry.Provisioner comes
// only from a loaded directory's config.yaml (the settings overlay,
// mergeHarnessConfigEntries, never carries a provisioner, so a settings-only
// entry with a provisioner block resolves to a generic harness), and an
// unloadable hydrated path also resolves to a generic harness. So the
// decision is exercised directly, and the call-site guard requires Start's
// error branch to use it.
func TestHarnessConfigPolicy_ResolveErrorWithStagedWrapper(t *testing.T) {
	home := t.TempDir()
	stage := func() {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(home, ".scion", "hooks", "pre-start.d"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, ".scion", "hooks", "pre-start.d", "20-harness-provision"), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	resolveErr := errors.New("resolve failed")
	policyCtx := (&recordingPolicy{refuse: true}).ctx()

	stage()
	_, err := harnessAfterResolveError(policyCtx, home, "hc", "generic", resolveErr)
	var ne *HarnessConfigNotEvaluatedError
	if !errors.Is(err, ErrHarnessConfigPolicy) || !errors.Is(err, ErrHarnessConfigNotEvaluated) || !errors.As(err, &ne) || ne.Name != "hc" {
		t.Errorf("policy attached + wrapper staged: expected a refusal naming hc, got %v", err)
	}

	// No policy: the fallback runs, and the staged wrapper and bundle are
	// cleared, so the earlier launch's provisioner does not run.
	if err := os.MkdirAll(filepath.Join(home, ".scion", "harness"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".scion", "harness", "manifest.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	h, err := harnessAfterResolveError(context.Background(), home, "hc", "generic", resolveErr)
	if err != nil || h == nil {
		t.Errorf("no policy: expected the harness.New fallback, got h=%v err=%v", h, err)
	}
	for _, p := range []string{
		filepath.Join(home, ".scion", "hooks", "pre-start.d", "20-harness-provision"),
		filepath.Join(home, ".scion", "harness"),
	} {
		if _, statErr := os.Stat(p); !os.IsNotExist(statErr) {
			t.Errorf("no policy: staged provisioning not cleared on the fallback: %s (stat err=%v)", p, statErr)
		}
	}

	// Policy attached, nothing staged: the fallback runs.
	if h, err := harnessAfterResolveError(policyCtx, home, "hc", "generic", resolveErr); err != nil || h == nil {
		t.Errorf("policy attached, nothing staged: expected the fallback, got h=%v err=%v", h, err)
	}
}

// writeStagedOutputs writes the env overlay a provisioner run leaves in the
// bundle's outputs directory.
func writeStagedOutputs(t *testing.T, home string) {
	t.Helper()
	out := filepath.Join(home, ".scion", "harness", "outputs")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "env.json"), []byte(`{"STALE":"1"}`), 0o644); err != nil {
		t.Fatal(err)
	}
}

// assertNoStagedProvisioning checks that home holds no container-script
// provisioning state: no wrapper, and none of the staged bundle (manifest,
// so sciontool init does not require provisioning or load an env overlay;
// outputs; config.yaml; provision.py).
func assertNoStagedProvisioning(t *testing.T, home string) {
	t.Helper()
	for _, p := range []string{
		filepath.Join(home, ".scion", "hooks", "pre-start.d", "20-harness-provision"),
		filepath.Join(home, ".scion", "harness", "manifest.json"),
		filepath.Join(home, ".scion", "harness", "outputs"),
		filepath.Join(home, ".scion", "harness", "config.yaml"),
		filepath.Join(home, ".scion", "harness", "provision.py"),
	} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("stale provisioning state remains: %s (stat err=%v)", p, err)
		}
	}
	req, err := hooks.LoadHarnessManifestRequirement(home)
	if err != nil || req.Required || req.EnvOverlayPath != "" {
		t.Errorf("sciontool init would still treat this as a container-script provision: req=%+v err=%v", req, err)
	}
}

// ProvisionAgent alone clears container-script provisioning state staged in
// an agent home when it renders a harness that is not container-script.
func TestHarnessConfigPolicy_ProvisionClearsStagedProvisioning(t *testing.T) {
	e := newPolicyTestEnv(t)
	e.projectHC(t, "hc-decl", policyTestDecl)
	home := config.GetAgentHomePath(e.scion, "prov-clear")
	hookDir := filepath.Join(home, ".scion", "hooks", "pre-start.d")
	bundle := filepath.Join(home, ".scion", "harness")
	for _, d := range []string{hookDir, bundle} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(hookDir, "20-harness-provision"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `{"harness_config":{"provisioner":{"type":"container-script"}},"outputs":{"env":"$HOME/.scion/harness/outputs/env.json"}}`
	if err := os.WriteFile(filepath.Join(bundle, "manifest.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	writeStagedOutputs(t, home)

	if _, _, _, err := ProvisionAgent(context.Background(), "prov-clear", "", "", "hc-decl", e.scion, "", "", "", ""); err != nil {
		t.Fatalf("ProvisionAgent: %v", err)
	}
	assertNoStagedProvisioning(t, home)
}

// TestHarnessConfigPolicy_ContainerScriptConstructionPinnedToResolve guards
// pkg/harness itself: NewContainerScriptHarness is called only from Resolve,
// Resolve is not called from elsewhere in the package, and a
// ContainerScriptHarness value is built only in NewContainerScriptHarness. So
// every container-script harness outside pkg/harness comes from a
// harness.Resolve call, which the call-site guard ties to the policy check.
func TestHarnessConfigPolicy_ContainerScriptConstructionPinnedToResolve(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "harness", "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	var newCallers, resolveCallers, literalSites, indirect []string
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			// callees holds call targets; selector field names (x.Resolve)
			// refer to other types' members, not these functions.
			callees := map[ast.Node]bool{}
			ast.Inspect(decl, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CallExpr:
					callees[x.Fun] = true
				case *ast.SelectorExpr:
					callees[x.Sel] = true
				}
				return true
			})
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				// Package-level declarations must not reference the
				// constructors at all.
				ast.Inspect(decl, func(n ast.Node) bool {
					if id, ok := n.(*ast.Ident); ok && !callees[id] && (id.Name == "NewContainerScriptHarness" || id.Name == "Resolve") || isCallOf(n, "NewContainerScriptHarness", "Resolve") {
						if _, isFunc := decl.(*ast.FuncDecl); !isFunc {
							indirect = append(indirect, "package-level reference")
						}
					}
					return true
				})
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.Ident:
					if (x.Name == "NewContainerScriptHarness" || x.Name == "Resolve") && !callees[x] {
						indirect = append(indirect, fn.Name.Name+": "+x.Name+" as a value")
					}
				case *ast.CallExpr:
					if id, ok := x.Fun.(*ast.Ident); ok {
						switch id.Name {
						case "NewContainerScriptHarness":
							newCallers = append(newCallers, fn.Name.Name)
						case "Resolve":
							resolveCallers = append(resolveCallers, fn.Name.Name)
						case "new":
							if len(x.Args) == 1 {
								if a, ok := x.Args[0].(*ast.Ident); ok && a.Name == "ContainerScriptHarness" {
									literalSites = append(literalSites, fn.Name.Name+" (new)")
								}
							}
						}
					}
				case *ast.CompositeLit:
					if id, ok := x.Type.(*ast.Ident); ok && id.Name == "ContainerScriptHarness" {
						literalSites = append(literalSites, fn.Name.Name)
					}
				}
				return true
			})
		}
	}
	if len(newCallers) != 1 || newCallers[0] != "Resolve" {
		t.Errorf("NewContainerScriptHarness callers in pkg/harness = %v, want only Resolve", newCallers)
	}
	if len(resolveCallers) != 0 {
		t.Errorf("Resolve is called from %v within pkg/harness; container-script harnesses must come only from harness.Resolve call sites the policy guard checks", resolveCallers)
	}
	if len(literalSites) != 1 || literalSites[0] != "NewContainerScriptHarness" {
		t.Errorf("ContainerScriptHarness literals in %v, want only NewContainerScriptHarness", literalSites)
	}
	if len(indirect) != 0 {
		t.Errorf("indirect references to the harness constructors in pkg/harness: %v", indirect)
	}
}

// A non-container-script relaunch restages the capture-auth assets the
// harness-config provides after clearing the container-script bundle.
func TestHarnessConfigPolicy_RelaunchRestagesCaptureAuth(t *testing.T) {
	e := newPolicyTestEnv(t)
	e.projectHC(t, "hc-scripted", policyTestScripted)
	e.projectHC(t, "hc-decl", policyTestDecl)
	if err := os.WriteFile(filepath.Join(e.scion, "harness-configs", "hc-decl", "capture_auth.py"), []byte("#!/usr/bin/env python3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	mgr := policyTestManager(nil)
	if _, err := mgr.Start(context.Background(), api.StartOptions{Name: "ca", ProjectPath: e.scion, HarnessConfig: "hc-scripted", NoAuth: true}); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	if _, err := mgr.Start(context.Background(), api.StartOptions{Name: "ca", ProjectPath: e.scion, HarnessConfig: "hc-decl", NoAuth: true}); err != nil {
		t.Fatalf("relaunch: %v", err)
	}
	home := config.GetAgentHomePath(e.scion, "ca")
	assertNoStagedProvisioning(t, home)
	if _, err := os.Stat(filepath.Join(home, ".scion", "harness", "capture_auth.py")); err != nil {
		t.Errorf("capture-auth assets not restaged after the bundle clear: %v", err)
	}
}

// Switching an agent from container-script harness-config A to B clears A's
// staged bundle (secrets, outputs, dialect.yaml, manifest, inputs) before B
// is provisioned; the control-plane inputs are restaged and B stages its own
// bundle.
func TestHarnessConfigPolicy_ContainerScriptSwitchClearsPreviousBundle(t *testing.T) {
	e := newPolicyTestEnv(t)
	e.projectHC(t, "hc-a", policyTestScripted+"# config: a\n")
	if err := os.WriteFile(filepath.Join(e.scion, "harness-configs", "hc-a", "dialect.yaml"), []byte("name: a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.projectHC(t, "hc-b", policyTestScripted+"# config: b\n")
	mgr := policyTestManager(nil)
	if _, err := mgr.Start(context.Background(), api.StartOptions{Name: "switch", ProjectPath: e.scion, HarnessConfig: "hc-a", NoAuth: true}); err != nil {
		t.Fatalf("Start with A: %v", err)
	}
	home := config.GetAgentHomePath(e.scion, "switch")
	bundle := filepath.Join(home, ".scion", "harness")
	// What A's provisioning left behind.
	writeStagedOutputs(t, home)
	if err := os.MkdirAll(filepath.Join(bundle, "secrets"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "secrets", "A_TOKEN"), []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(bundle, "inputs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "inputs", "unrelated.md"), []byte("unrelated"), 0o644); err != nil {
		t.Fatal(err)
	}
	aStale := []string{
		filepath.Join(bundle, "secrets", "A_TOKEN"),
		filepath.Join(bundle, "outputs", "env.json"),
		filepath.Join(bundle, "dialect.yaml"),
	}
	for _, p := range aStale {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("fixture: %s should exist after A: %v", p, err)
		}
	}

	// Before B's Provision: resolve B and reset, as ProvisionAgent and Start
	// do, then check the bundle before provisioning.
	resolvedB, err := harness.Resolve(context.Background(), harness.ResolveOptions{Name: "hc-b", ProjectPath: e.scion})
	if err != nil || resolvedB.Implementation != "container-script" {
		t.Fatalf("resolve B: impl=%v err=%v", resolvedB, err)
	}
	if err := resetStagedProvisioning(home); err != nil {
		t.Fatalf("reset: %v", err)
	}
	for _, p := range append(aStale, filepath.Join(bundle, "manifest.json"), filepath.Join(bundle, "config.yaml")) {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("before B's Provision, A's staged file remains: %s (stat err=%v)", p, err)
		}
	}
	if _, err := os.Stat(filepath.Join(bundle, "inputs")); !os.IsNotExist(err) {
		t.Errorf("before B's Provision, inputs/ must be cleared (stat err=%v)", err)
	}

	// The full launch path: Start with B.
	if _, err := mgr.Start(context.Background(), api.StartOptions{Name: "switch", ProjectPath: e.scion, HarnessConfig: "hc-b", NoAuth: true}); err != nil {
		t.Fatalf("Start with B: %v", err)
	}
	for _, p := range aStale {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("after B's launch, A's staged file remains: %s (stat err=%v)", p, err)
		}
	}
	if _, err := os.Stat(filepath.Join(bundle, "inputs", "unrelated.md")); !os.IsNotExist(err) {
		t.Errorf("a file not written by the control plane survived in inputs/ (stat err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(bundle, "inputs", "instructions.md")); err != nil {
		t.Errorf("control-plane instructions.md not restaged: %v", err)
	}
	staged, err := os.ReadFile(filepath.Join(bundle, "config.yaml"))
	if err != nil || !strings.Contains(string(staged), "# config: b") {
		t.Errorf("B's config.yaml not staged: %v\n%s", err, staged)
	}
	if !wrapperStaged(e, "switch") {
		t.Error("B's provisioner wrapper should be staged")
	}
}

// isCallOf reports whether n is a call of one of the named package-local
// functions.
func isCallOf(n ast.Node, names ...string) bool {
	call, ok := n.(*ast.CallExpr)
	if !ok {
		return false
	}
	id, ok := call.Fun.(*ast.Ident)
	if !ok {
		return false
	}
	for _, name := range names {
		if id.Name == name {
			return true
		}
	}
	return false
}

// inputs/ holds only control-plane content: files a harness-config or
// template home/ tree would copy there are cleared before the control plane
// stages its inputs at provisioning, and other files there are
// replaced on every launch by the control plane's recorded copy.
func TestHarnessConfigPolicy_InputsOnlyFromControlPlane(t *testing.T) {
	e := newPolicyTestEnv(t)
	e.projectHC(t, "hc-scripted", policyTestScripted)
	// A harness-config home/ tree and a template home/ tree that would
	// place files under .scion/harness/inputs.
	hcHomeInputs := filepath.Join(e.scion, "harness-configs", "hc-scripted", "home", ".scion", "harness", "inputs")
	tplDir := e.template(t, "tplx", "harness_config: hc-scripted\n")
	tplHomeInputs := filepath.Join(tplDir, "home", ".scion", "harness", "inputs")
	for dir, name := range map[string]string{hcHomeInputs: "from-hc-home.md", tplHomeInputs: "instructions.md"} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte("copied home content"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	mgr := policyTestManager(nil)
	if _, err := mgr.Start(context.Background(), api.StartOptions{Name: "inputs", ProjectPath: e.scion, Template: "tplx", NoAuth: true}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	home := config.GetAgentHomePath(e.scion, "inputs")
	inputs := filepath.Join(home, ".scion", "harness", "inputs")
	if _, err := os.Stat(filepath.Join(inputs, "from-hc-home.md")); !os.IsNotExist(err) {
		t.Errorf("harness-config home/ content landed in inputs/ (stat err=%v)", err)
	}
	instr, err := os.ReadFile(filepath.Join(inputs, "instructions.md"))
	if err != nil || strings.Contains(string(instr), "copied home content") {
		t.Fatalf("instructions.md must be the control plane's, got %q (err=%v)", instr, err)
	}
	controlPlane := string(instr)

	// inputs/ changes between launches: a control-plane input is rewritten
	// and another file is added.
	if err := os.WriteFile(filepath.Join(inputs, "instructions.md"), []byte("unrecorded content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inputs, "unrecorded.md"), []byte("unrecorded content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Start(context.Background(), api.StartOptions{Name: "inputs", ProjectPath: e.scion, Template: "tplx", NoAuth: true}); err != nil {
		t.Fatalf("relaunch: %v", err)
	}
	if _, err := os.Stat(filepath.Join(inputs, "unrecorded.md")); !os.IsNotExist(err) {
		t.Errorf("a file the control plane did not stage remained in inputs/ (stat err=%v)", err)
	}
	if got, err := os.ReadFile(filepath.Join(inputs, "instructions.md")); err != nil || string(got) != controlPlane {
		t.Errorf("instructions.md not restaged from the control plane's copy: %q (err=%v)", got, err)
	}
}

// An agent provisioned before the control plane recorded its inputs (no
// agentDir/harness-inputs) has its record seeded once, on its next start,
// from the three known files in its existing inputs/ (regular files only),
// with a warning; any other file there is dropped, and later starts restore
// the seeded record.
func TestHarnessConfigPolicy_LegacyAgentInputsSeededOnce(t *testing.T) {
	e := newPolicyTestEnv(t)
	e.projectHC(t, "hc-scripted", policyTestScripted)
	mgr := policyTestManager(nil)
	opts := api.StartOptions{Name: "legacy", ProjectPath: e.scion, HarnessConfig: "hc-scripted", NoAuth: true}
	if _, err := mgr.Start(context.Background(), opts); err != nil {
		t.Fatalf("Start: %v", err)
	}
	agentDir := config.ResolveAgentDir(e.scion, "legacy")
	record := filepath.Join(agentDir, controlPlaneInputsDirName)
	if _, err := os.Stat(record); err != nil {
		t.Fatalf("fixture: provisioning should record inputs: %v", err)
	}
	// Simulate an agent provisioned before the record existed.
	if err := os.RemoveAll(record); err != nil {
		t.Fatal(err)
	}
	inputs := filepath.Join(config.GetAgentHomePath(e.scion, "legacy"), ".scion", "harness", "inputs")
	for name, body := range map[string]string{
		"instructions.md":      "legacy instructions",
		"resolved-skills.json": `{"skills":[]}`,
		"unrelated.md":         "not a control-plane input",
	} {
		if err := os.WriteFile(filepath.Join(inputs, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.Remove(filepath.Join(inputs, "system-prompt.md"))
	if err := os.Symlink(filepath.Join(inputs, "unrelated.md"), filepath.Join(inputs, "system-prompt.md")); err != nil {
		t.Fatal(err)
	}

	var logBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	if _, err := mgr.Start(context.Background(), opts); err != nil {
		t.Fatalf("legacy start: %v", err)
	}
	logged := logBuf.String()
	if !strings.Contains(logged, "harness inputs seeded once from pre-existing inputs; re-provision to refresh") || !strings.Contains(logged, "agent_id=legacy") {
		t.Errorf("expected the seeding warning with the agent id, got log: %s", logged)
	}
	if !strings.Contains(logged, "harness input not seeded: not a regular file") || !strings.Contains(logged, "file=system-prompt.md") {
		t.Errorf("expected a warning for the skipped symlink, got log: %s", logged)
	}
	if got, err := os.ReadFile(filepath.Join(inputs, "instructions.md")); err != nil || string(got) != "legacy instructions" {
		t.Errorf("instructions.md not restored from the prior file: %q (err=%v)", got, err)
	}
	if _, err := os.Stat(filepath.Join(inputs, "resolved-skills.json")); err != nil {
		t.Errorf("resolved-skills.json not restored: %v", err)
	}
	for _, name := range []string{"unrelated.md", "system-prompt.md"} {
		if _, err := os.Lstat(filepath.Join(inputs, name)); !os.IsNotExist(err) {
			t.Errorf("%s must not be seeded (non-listed file or symlink); stat err=%v", name, err)
		}
	}

	// Seeding happens once: later starts restore the record, not the home.
	if err := os.WriteFile(filepath.Join(inputs, "instructions.md"), []byte("unrecorded content"), 0o644); err != nil {
		t.Fatal(err)
	}
	logBuf.Reset()
	if _, err := mgr.Start(context.Background(), opts); err != nil {
		t.Fatalf("third start: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(inputs, "instructions.md")); string(got) != "legacy instructions" {
		t.Errorf("later start must restore the seeded record, got %q", got)
	}
	if strings.Contains(logBuf.String(), "seeded once from pre-existing inputs") {
		t.Error("seeding must happen only once")
	}
}

// B1: a staging failure for a container-script harness fails the launch and
// leaves no provisioner wrapper from an earlier launch.
func TestHarnessConfigPolicy_StagingFailureFailsLaunch(t *testing.T) {
	e := newPolicyTestEnv(t)
	e.projectHC(t, "hc-scripted", policyTestScripted)
	runs := 0
	mgr := policyTestManager(&runs)
	opts := api.StartOptions{Name: "stagefail", ProjectPath: e.scion, HarnessConfig: "hc-scripted", NoAuth: true}
	if _, err := mgr.Start(context.Background(), opts); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	if !wrapperStaged(e, "stagefail") {
		t.Fatal("fixture: wrapper should be staged")
	}
	// provision.py becomes a directory: it exists, but staging cannot copy it.
	prov := filepath.Join(e.scion, "harness-configs", "hc-scripted", "provision.py")
	if err := os.Remove(prov); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(prov, 0o755); err != nil {
		t.Fatal(err)
	}
	runsBefore := runs
	_, err := mgr.Start(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), "stage harness bundle") {
		t.Fatalf("expected the launch to fail with a staging error, got %v", err)
	}
	if runs != runsBefore {
		t.Error("the container must not run after a staging failure")
	}
	if wrapperStaged(e, "stagefail") {
		t.Error("a provisioner wrapper remains after a staging failure")
	}
}

func writeUnrecordedInputs(t *testing.T, home string) {
	t.Helper()
	inputs := filepath.Join(home, ".scion", "harness", "inputs")
	if err := os.MkdirAll(inputs, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"system-prompt.md", "instructions.md"} {
		if err := os.WriteFile(filepath.Join(inputs, name), []byte("unrecorded content"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func assertUnrecordedInputsNotRestored(t *testing.T, e *policyTestEnv, agentName string) {
	t.Helper()
	home := config.GetAgentHomePath(e.scion, agentName)
	record := filepath.Join(config.ResolveAgentDir(e.scion, agentName), controlPlaneInputsDirName)
	for _, dir := range []string{filepath.Join(home, ".scion", "harness", "inputs"), record} {
		for _, name := range []string{"system-prompt.md", "instructions.md"} {
			if data, err := os.ReadFile(filepath.Join(dir, name)); err == nil && string(data) == "unrecorded content" {
				t.Errorf("an unrecorded %s was restored into %s", name, dir)
			}
		}
	}
}

// B2/S3a: once the record has been checked it exists, empty if nothing
// qualified, and an empty record restores nothing and never re-arms the
// seed. Inputs the control plane did not stage are not restored across restarts, for a
// legacy agent with no staged inputs and for an agent whose harness-config
// gains a provisioner after create.
func TestHarnessConfigPolicy_EmptyRecordRestoresNoUnrecordedInputs(t *testing.T) {
	t.Run("legacy agent with no staged inputs", func(t *testing.T) {
		e := newPolicyTestEnv(t)
		e.projectHC(t, "hc-scripted", policyTestScripted)
		mgr := policyTestManager(nil)
		opts := api.StartOptions{Name: "legacy-empty", ProjectPath: e.scion, HarnessConfig: "hc-scripted", NoAuth: true}
		if _, err := mgr.Start(context.Background(), opts); err != nil {
			t.Fatalf("Start: %v", err)
		}
		agentDir := config.ResolveAgentDir(e.scion, "legacy-empty")
		home := config.GetAgentHomePath(e.scion, "legacy-empty")
		// Legacy: no record, and no inputs staged in the home.
		if err := os.RemoveAll(filepath.Join(agentDir, controlPlaneInputsDirName)); err != nil {
			t.Fatal(err)
		}
		if err := os.RemoveAll(filepath.Join(home, ".scion", "harness", "inputs")); err != nil {
			t.Fatal(err)
		}
		if _, err := mgr.Start(context.Background(), opts); err != nil {
			t.Fatalf("legacy start: %v", err)
		}
		entries, err := os.ReadDir(filepath.Join(agentDir, controlPlaneInputsDirName))
		if err != nil || len(entries) != 0 {
			t.Fatalf("expected an empty record after the first check, got %v (err=%v)", entries, err)
		}
		for i := 0; i < 2; i++ {
			writeUnrecordedInputs(t, home)
			if _, err := mgr.Start(context.Background(), opts); err != nil {
				t.Fatalf("restart %d: %v", i, err)
			}
			assertUnrecordedInputsNotRestored(t, e, "legacy-empty")
		}
	})

	t.Run("harness-config gains a provisioner after create", func(t *testing.T) {
		e := newPolicyTestEnv(t)
		e.projectHC(t, "hc-later", policyTestDecl)
		mgr := policyTestManager(nil)
		opts := api.StartOptions{Name: "gains", ProjectPath: e.scion, HarnessConfig: "hc-later", NoAuth: true}
		if _, err := mgr.Start(context.Background(), opts); err != nil {
			t.Fatalf("declarative Start: %v", err)
		}
		home := config.GetAgentHomePath(e.scion, "gains")
		writeUnrecordedInputs(t, home)
		// The harness-config gains a provisioner.
		e.projectHC(t, "hc-later", policyTestScripted)
		for i := 0; i < 2; i++ {
			if _, err := mgr.Start(context.Background(), opts); err != nil {
				t.Fatalf("container-script start %d: %v", i, err)
			}
			assertUnrecordedInputsNotRestored(t, e, "gains")
			writeUnrecordedInputs(t, home)
		}
	})
}

// NB4: ProvisionAgent records the inputs it stages; the record, not the agent
// home, is what the first start restores.
func TestHarnessConfigPolicy_ProvisionRecordsInputs(t *testing.T) {
	e := newPolicyTestEnv(t)
	e.projectHC(t, "hc-scripted", policyTestScripted)
	if _, _, _, err := ProvisionAgent(context.Background(), "recorded", "", "", "hc-scripted", e.scion, "", "", "", ""); err != nil {
		t.Fatalf("ProvisionAgent: %v", err)
	}
	home := config.GetAgentHomePath(e.scion, "recorded")
	staged, err := os.ReadFile(filepath.Join(home, ".scion", "harness", "inputs", "instructions.md"))
	if err != nil {
		t.Fatalf("fixture: provisioning should stage instructions.md: %v", err)
	}
	record := filepath.Join(config.ResolveAgentDir(e.scion, "recorded"), controlPlaneInputsDirName)
	recorded, err := os.ReadFile(filepath.Join(record, "instructions.md"))
	if err != nil || string(recorded) != string(staged) {
		t.Fatalf("provisioning did not record the staged instructions.md (err=%v)", err)
	}

	// The staged input changes before the first start; the
	// first start restores the control plane's copy.
	writeUnrecordedInputs(t, home)
	if _, err := policyTestManager(nil).Start(context.Background(), api.StartOptions{Name: "recorded", ProjectPath: e.scion, HarnessConfig: "hc-scripted", NoAuth: true}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(home, ".scion", "harness", "inputs", "instructions.md"))
	if err != nil || string(got) != string(staged) {
		t.Errorf("first start did not restore the recorded instructions.md: %q (err=%v)", got, err)
	}
}

const policyTestScriptedAuth = policyTestScripted + `auth:
  default_type: api-key
  types:
    api-key:
      required_env:
        - any_of: ["POLICY_TEST_KEY"]
`

// Secrets record: the secret files ApplyAuthSettings stages are recorded in
// the agent directory (0700/0600) and restored on the next start; a
// safe-named file in secrets/ that was not recorded is not referenced.
func TestHarnessConfigPolicy_SecretsRecordedAndRestored(t *testing.T) {
	e := newPolicyTestEnv(t)
	e.projectHC(t, "hc-auth", policyTestScriptedAuth)
	mgr := policyTestManager(nil)
	opts := api.StartOptions{Name: "secrets", ProjectPath: e.scion, HarnessConfig: "hc-auth", HarnessAuth: "api-key", Env: map[string]string{"POLICY_TEST_KEY": "k1"}}
	if _, err := mgr.Start(context.Background(), opts); err != nil {
		t.Fatalf("Start: %v", err)
	}
	record := filepath.Join(config.ResolveAgentDir(e.scion, "secrets"), config.HarnessSecretsRecordDirName)
	info, err := os.Stat(record)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("secrets record missing or not 0700: %v %v", info, err)
	}
	fi, err := os.Stat(filepath.Join(record, "POLICY_TEST_KEY"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("staged secret not recorded 0600: %v %v", fi, err)
	}

	home := config.GetAgentHomePath(e.scion, "secrets")
	secrets := filepath.Join(home, ".scion", "harness", "secrets")
	if err := os.WriteFile(filepath.Join(secrets, "UNRECORDED_TOKEN"), []byte("unrelated"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Start(context.Background(), opts); err != nil {
		t.Fatalf("restart: %v", err)
	}
	if _, err := os.Stat(filepath.Join(secrets, "UNRECORDED_TOKEN")); !os.IsNotExist(err) {
		t.Errorf("an unrecorded secret file remained after the restart (stat err=%v)", err)
	}
	cands, err := os.ReadFile(filepath.Join(home, ".scion", "harness", "inputs", "auth-candidates.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(cands), "UNRECORDED_TOKEN") {
		t.Error("an unrecorded secret file was referenced in auth-candidates.json")
	}
	if !strings.Contains(string(cands), "POLICY_TEST_KEY") {
		t.Errorf("the recorded secret is not referenced: %s", cands)
	}
	if _, err := os.Stat(filepath.Join(record, "UNRECORDED_TOKEN")); !os.IsNotExist(err) {
		t.Error("an unrecorded secret file was recorded")
	}
}

// An agent provisioned before secrets records existed gets no restored
// secret files, a warning naming it, and an empty record (which never
// re-arms the warning).
func TestHarnessConfigPolicy_LegacyAgentSecretsNotRestored(t *testing.T) {
	e := newPolicyTestEnv(t)
	e.projectHC(t, "hc-scripted", policyTestScripted)
	mgr := policyTestManager(nil)
	opts := api.StartOptions{Name: "legacy-secrets", ProjectPath: e.scion, HarnessConfig: "hc-scripted", NoAuth: true}
	if _, err := mgr.Start(context.Background(), opts); err != nil {
		t.Fatalf("Start: %v", err)
	}
	record := filepath.Join(config.ResolveAgentDir(e.scion, "legacy-secrets"), config.HarnessSecretsRecordDirName)
	if _, err := os.Stat(record); err != nil {
		t.Fatalf("fixture: provisioning should create the secrets record: %v", err)
	}
	if err := os.RemoveAll(record); err != nil {
		t.Fatal(err)
	}
	home := config.GetAgentHomePath(e.scion, "legacy-secrets")
	secrets := filepath.Join(home, ".scion", "harness", "secrets")
	if err := os.MkdirAll(secrets, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secrets, "OLD_TOKEN"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	var logBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	if _, err := mgr.Start(context.Background(), opts); err != nil {
		t.Fatalf("legacy start: %v", err)
	}
	if !strings.Contains(logBuf.String(), "file-type auth secrets are not restored for agents provisioned before harness secrets were recorded; re-create or re-supply credentials") || !strings.Contains(logBuf.String(), "agent_id=legacy-secrets") {
		t.Errorf("expected the legacy secrets warning with the agent id, got: %s", logBuf.String())
	}
	if _, err := os.Stat(filepath.Join(secrets, "OLD_TOKEN")); !os.IsNotExist(err) {
		t.Error("a secret file from before the record existed was kept")
	}
	if entries, err := os.ReadDir(record); err != nil || len(entries) != 0 {
		t.Errorf("expected an empty secrets record, got %v (err=%v)", entries, err)
	}
	logBuf.Reset()
	if _, err := mgr.Start(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logBuf.String(), "file-type auth secrets are not restored") {
		t.Error("the legacy secrets warning must not repeat once the record exists")
	}
}

const policyTestScriptedADC = policyTestScripted + `auth:
  default_type: auth-file
  types:
    auth-file:
      required_files:
        - name: ADC_FILE
          type: file
          target_suffix: .config/gcloud/application_default_credentials.json
`

// Start restores the secrets record: a file-type secret staged on the first
// start from a host credential file is restored from the record on a restart
// whose auth inputs do not supply it, and auth-candidates.json references
// it.
func TestHarnessConfigPolicy_StartRestoresSecretsRecord(t *testing.T) {
	e := newPolicyTestEnv(t)
	e.projectHC(t, "hc-adc", policyTestScriptedADC)
	adc := filepath.Join(t.TempDir(), "adc.json")
	if err := os.WriteFile(adc, []byte(`{"type":"authorized_user"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	mgr := policyTestManager(nil)
	opts := api.StartOptions{Name: "restore", ProjectPath: e.scion, HarnessConfig: "hc-adc", HarnessAuth: "auth-file",
		Env: map[string]string{"GOOGLE_APPLICATION_CREDENTIALS": adc}}
	if _, err := mgr.Start(context.Background(), opts); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	home := config.GetAgentHomePath(e.scion, "restore")
	secret := filepath.Join(home, ".scion", "harness", "secrets", "ADC_FILE")
	if data, err := os.ReadFile(secret); err != nil || string(data) != `{"type":"authorized_user"}` {
		t.Fatalf("fixture: the first start should stage ADC_FILE from the host file: %q (err=%v)", data, err)
	}

	// Restart: the host credential is removed and not supplied.
	if err := os.Remove(adc); err != nil {
		t.Fatal(err)
	}
	opts.Env = nil
	if _, err := mgr.Start(context.Background(), opts); err != nil {
		t.Fatalf("restart: %v", err)
	}
	data, err := os.ReadFile(secret)
	if err != nil || string(data) != `{"type":"authorized_user"}` {
		t.Fatalf("ADC_FILE not restored from the record: %q (err=%v)", data, err)
	}
	cands, err := os.ReadFile(filepath.Join(home, ".scion", "harness", "inputs", "auth-candidates.json"))
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		FileSecretFiles map[string]string `json:"file_secret_files"`
	}
	if err := json.Unmarshal(cands, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.FileSecretFiles["ADC_FILE"] != "$HOME/.scion/harness/secrets/ADC_FILE" {
		t.Errorf("restored ADC_FILE not referenced in file_secret_files: %v", payload.FileSecretFiles)
	}
}

// restoreSecretsRecord restores only regular files, into a 0700 directory
// with 0600 files, and returns their names, and only when the recorded
// harness-config identity matches the current one; any ambiguity restores
// nothing.
func TestRestoreSecretsRecord(t *testing.T) {
	agentDir := t.TempDir()
	home := filepath.Join(t.TempDir(), "home")
	record := filepath.Join(agentDir, config.HarnessSecretsRecordDirName)
	if err := os.MkdirAll(record, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(record, "TOKEN_A"), []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(record, "TOKEN_A"), filepath.Join(record, "LINKED")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(record, "subdir"), 0o700); err != nil {
		t.Fatal(err)
	}
	id := &harnessConfigIdentity{Name: "hc", Source: "broker-local", Revision: "sha256:abc"}
	idData, _ := json.Marshal(id)
	if err := os.WriteFile(filepath.Join(record, secretsIdentityFile), idData, 0o600); err != nil {
		t.Fatal(err)
	}

	// Ambiguity or mismatch restores nothing.
	for name, current := range map[string]*harnessConfigIdentity{
		"unknown current identity": nil,
		"different name":           {Name: "other", Source: "broker-local", Revision: "sha256:abc"},
		"different revision":       {Name: "hc", Source: "broker-local", Revision: "sha256:def"},
		"different hub record":     {Name: "hc", Source: "broker-local", HubRecordID: "rec-2", Revision: "sha256:abc"},
	} {
		got, err := restoreSecretsRecord(agentDir, home, "agent-x", current)
		if err != nil || len(got) != 0 {
			t.Errorf("%s: expected nothing restored, got %v (err=%v)", name, got, err)
		}
	}
	for name, content := range map[string]string{"missing identity": "", "unparseable identity": "{not json"} {
		if content == "" {
			_ = os.Remove(filepath.Join(record, secretsIdentityFile))
		} else if err := os.WriteFile(filepath.Join(record, secretsIdentityFile), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := restoreSecretsRecord(agentDir, home, "agent-x", id)
		if err != nil || len(got) != 0 {
			t.Errorf("%s: expected nothing restored, got %v (err=%v)", name, got, err)
		}
	}
	if err := os.WriteFile(filepath.Join(record, secretsIdentityFile), idData, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".scion", "harness", "secrets", "TOKEN_A")); !os.IsNotExist(err) {
		t.Fatalf("nothing should have been restored yet (stat err=%v)", err)
	}

	names, err := restoreSecretsRecord(agentDir, home, "agent-x", id)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(names, ",") != "TOKEN_A" {
		t.Errorf("restored names = %v, want [TOKEN_A]", names)
	}
	dir := filepath.Join(home, ".scion", "harness", "secrets")
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("secrets dir not 0700: %v %v", info, err)
	}
	if info, err := os.Stat(filepath.Join(dir, "TOKEN_A")); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("restored file not 0600: %v %v", info, err)
	}
	for _, name := range []string{"LINKED", "subdir", secretsIdentityFile} {
		if _, err := os.Lstat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("%s must not be restored (stat err=%v)", name, err)
		}
	}
}

// ProvisionAgent creates the secrets record (0700), so a new agent's first
// start does not take the path for agents provisioned before records existed.
func TestHarnessConfigPolicy_ProvisionCreatesSecretsRecord(t *testing.T) {
	e := newPolicyTestEnv(t)
	e.projectHC(t, "hc-scripted", policyTestScripted)
	if _, _, _, err := ProvisionAgent(context.Background(), "secrec", "", "", "hc-scripted", e.scion, "", "", "", ""); err != nil {
		t.Fatalf("ProvisionAgent: %v", err)
	}
	record := filepath.Join(config.ResolveAgentDir(e.scion, "secrec"), config.HarnessSecretsRecordDirName)
	if info, err := os.Stat(record); err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("ProvisionAgent did not create a 0700 secrets record: %v %v", info, err)
	}
	var logBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	if _, err := policyTestManager(nil).Start(context.Background(), api.StartOptions{Name: "secrec", ProjectPath: e.scion, HarnessConfig: "hc-scripted", NoAuth: true}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if strings.Contains(logBuf.String(), "not restored for agents provisioned before") {
		t.Errorf("a new agent's first start logged the legacy secrets warning: %s", logBuf.String())
	}
}

// adcStart starts agent name with harness-config hc, supplying the host ADC
// file at adc when non-empty.
func adcStart(t *testing.T, mgr Manager, e *policyTestEnv, name, hc, adc string, extra func(*api.StartOptions)) {
	t.Helper()
	opts := api.StartOptions{Name: name, ProjectPath: e.scion, HarnessConfig: hc, HarnessAuth: "auth-file"}
	if adc != "" {
		opts.Env = map[string]string{"GOOGLE_APPLICATION_CREDENTIALS": adc}
	}
	if extra != nil {
		extra(&opts)
	}
	if _, err := mgr.Start(context.Background(), opts); err != nil {
		t.Fatalf("Start %s with %s: %v", name, hc, err)
	}
}

// restoredADC reports whether the agent's staged secrets hold ADC_FILE and
// auth-candidates.json references it.
func restoredADC(t *testing.T, e *policyTestEnv, name string) (staged, referenced bool) {
	t.Helper()
	home := config.GetAgentHomePath(e.scion, name)
	_, err := os.Stat(filepath.Join(home, ".scion", "harness", "secrets", "ADC_FILE"))
	staged = err == nil
	if data, err := os.ReadFile(filepath.Join(home, ".scion", "harness", "inputs", "auth-candidates.json")); err == nil {
		referenced = strings.Contains(string(data), "ADC_FILE")
	}
	return staged, referenced
}

func newADCFile(t *testing.T) string {
	t.Helper()
	adc := filepath.Join(t.TempDir(), "adc.json")
	if err := os.WriteFile(adc, []byte(`{"type":"authorized_user"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return adc
}

// Recorded secrets are restored only for the harness-config revision that
// staged them: a same-config restart restores; a switch to a different
// harness-config (name, source or content), a revision change of the same
// one, or a different hub record restores nothing.
func TestHarnessConfigPolicy_SecretsRestoredOnlyForStagingConfig(t *testing.T) {
	t.Run("switch to a different name restores nothing", func(t *testing.T) {
		e := newPolicyTestEnv(t)
		e.projectHC(t, "hc-a", policyTestScriptedADC)
		e.projectHC(t, "hc-b", policyTestScriptedADC) // same content, different name
		mgr := policyTestManager(nil)
		adcStart(t, mgr, e, "sw", "hc-a", newADCFile(t), nil)
		adcStart(t, mgr, e, "sw", "hc-b", "", nil)
		if staged, ref := restoredADC(t, e, "sw"); staged || ref {
			t.Errorf("hc-a's recorded secret was carried to hc-b (staged=%v referenced=%v)", staged, ref)
		}
	})

	t.Run("switch does not carry env secrets", func(t *testing.T) {
		e := newPolicyTestEnv(t)
		e.projectHC(t, "hc-env-a", policyTestScriptedAuth)
		e.projectHC(t, "hc-env-b", policyTestScriptedAuth)
		mgr := policyTestManager(nil)
		if _, err := mgr.Start(context.Background(), api.StartOptions{Name: "swenv", ProjectPath: e.scion, HarnessConfig: "hc-env-a", HarnessAuth: "api-key", Env: map[string]string{"POLICY_TEST_KEY": "k1"}}); err != nil {
			t.Fatalf("Start A: %v", err)
		}
		if _, err := mgr.Start(context.Background(), api.StartOptions{Name: "swenv", ProjectPath: e.scion, HarnessConfig: "hc-env-b", HarnessAuth: "api-key"}); err != nil {
			t.Fatalf("Start B: %v", err)
		}
		home := config.GetAgentHomePath(e.scion, "swenv")
		if _, err := os.Stat(filepath.Join(home, ".scion", "harness", "secrets", "POLICY_TEST_KEY")); !os.IsNotExist(err) {
			t.Errorf("hc-env-a's recorded env secret was restored for hc-env-b (stat err=%v)", err)
		}
		if data, err := os.ReadFile(filepath.Join(home, ".scion", "harness", "inputs", "auth-candidates.json")); err == nil && strings.Contains(string(data), "POLICY_TEST_KEY\":") {
			t.Errorf("hc-env-b references hc-env-a's env secret: %s", data)
		}
	})

	t.Run("same-config restart restores (local)", func(t *testing.T) {
		e := newPolicyTestEnv(t)
		e.projectHC(t, "hc-a", policyTestScriptedADC)
		mgr := policyTestManager(nil)
		adcStart(t, mgr, e, "same", "hc-a", newADCFile(t), nil)
		adcStart(t, mgr, e, "same", "hc-a", "", nil)
		if staged, ref := restoredADC(t, e, "same"); !staged || !ref {
			t.Errorf("same-config restart did not restore (staged=%v referenced=%v)", staged, ref)
		}
	})

	t.Run("content change of the resolved dir restores nothing", func(t *testing.T) {
		e := newPolicyTestEnv(t)
		e.projectHC(t, "hc-a", policyTestScriptedADC)
		mgr := policyTestManager(nil)
		adcStart(t, mgr, e, "rev", "hc-a", newADCFile(t), nil)
		e.projectHC(t, "hc-a", policyTestScriptedADC+"# revised\n")
		adcStart(t, mgr, e, "rev", "hc-a", "", nil)
		if staged, ref := restoredADC(t, e, "rev"); staged || ref {
			t.Errorf("a revised harness-config restored the earlier revision's secret (staged=%v referenced=%v)", staged, ref)
		}
	})

	t.Run("different source restores nothing", func(t *testing.T) {
		e := newPolicyTestEnv(t)
		e.projectHC(t, "hc-a", policyTestScriptedADC)
		tplDir := e.template(t, "tplx", "harness_config: hc-a\n")
		writePolicyHC(t, filepath.Join(tplDir, "harness-configs", "hc-a"), policyTestScriptedADC) // same name and content, template-bundled
		mgr := policyTestManager(nil)
		// Staged with the template-bundled copy, restarted with the
		// broker-local copy of the same name and content.
		adcStart(t, mgr, e, "src", "hc-a", newADCFile(t), func(o *api.StartOptions) { o.Template = "tplx" })
		record := filepath.Join(config.ResolveAgentDir(e.scion, "src"), config.HarnessSecretsRecordDirName)
		if id := readSecretsIdentity(record); id == nil || id.Source != string(config.HarnessConfigSourceTemplateBundled) {
			t.Fatalf("fixture: expected the record staged from the template-bundled copy, got %+v", id)
		}
		adcStart(t, mgr, e, "src", "hc-a", "", nil)
		if staged, ref := restoredADC(t, e, "src"); staged || ref {
			t.Errorf("the broker-local copy restored the template-bundled copy's secret (staged=%v referenced=%v)", staged, ref)
		}
	})

	t.Run("hub record identity", func(t *testing.T) {
		e := newPolicyTestEnv(t)
		hydrated := filepath.Join(t.TempDir(), "hydrated", "hc-hub")
		writePolicyHC(t, hydrated, policyTestScriptedADC)
		mgr := policyTestManager(nil)
		hub := func(id string) func(*api.StartOptions) {
			return func(o *api.StartOptions) { o.HarnessConfigPath = hydrated; o.HarnessConfigID = id }
		}
		adcStart(t, mgr, e, "hub", "hc-hub", newADCFile(t), hub("rec-1"))
		adcStart(t, mgr, e, "hub", "hc-hub", "", hub("rec-1"))
		if staged, ref := restoredADC(t, e, "hub"); !staged || !ref {
			t.Fatalf("same hub record restart did not restore (staged=%v referenced=%v)", staged, ref)
		}
		adcStart(t, mgr, e, "hub", "hc-hub", "", hub("rec-2"))
		if staged, ref := restoredADC(t, e, "hub"); staged || ref {
			t.Errorf("a different hub record restored the earlier record's secret (staged=%v referenced=%v)", staged, ref)
		}
	})
}

// currentHarnessConfigIdentity establishes no identity for a missing or
// partly unreadable resolved directory (nothing is then restored).
func TestCurrentHarnessConfigIdentity_Unestablished(t *testing.T) {
	if id := currentHarnessConfigIdentity("hc", &config.HarnessConfigDir{Path: filepath.Join(t.TempDir(), "missing")}, ""); id != nil {
		t.Errorf("missing dir: expected no identity, got %+v", id)
	}
	dir := t.TempDir()
	writePolicyHC(t, dir, policyTestScripted)
	if id := currentHarnessConfigIdentity("hc", &config.HarnessConfigDir{Path: dir, Source: config.HarnessConfigSourceHubHydrated}, ""); id != nil {
		t.Errorf("hub-hydrated without a record ID: expected no identity, got %+v", id)
	}
	if id := currentHarnessConfigIdentity("hc", &config.HarnessConfigDir{Path: dir, Source: config.HarnessConfigSourceBrokerLocal}, ""); id == nil {
		t.Fatal("readable local dir: expected an identity")
	}
	if os.Geteuid() != 0 {
		if err := os.Chmod(filepath.Join(dir, "provision.py"), 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(filepath.Join(dir, "provision.py"), 0o644) })
		if id := currentHarnessConfigIdentity("hc", &config.HarnessConfigDir{Path: dir, Source: config.HarnessConfigSourceBrokerLocal}, ""); id != nil {
			t.Errorf("partly unreadable dir: expected no identity, got %+v", id)
		}
	}
}

// Agent state for shared-workspace projects is always resolved from the
// broker-side agent dir: a start without the shared-workspace flag (as a
// restart dispatch may be) still uses the external agent directory and
// restores its records, and an in-project copy of the agent directory
// (scion-agent.json plus records) is ignored.
func TestSharedWorkspaceAgentResolvesBrokerSideDir(t *testing.T) {
	e := newPolicyTestEnv(t)
	e.projectHC(t, "hc-scripted", policyTestScripted)
	mgr := policyTestManager(nil)
	opts := api.StartOptions{Name: "sw-agent", ProjectPath: e.scion, HarnessConfig: "hc-scripted", NoAuth: true, SharedWorkspace: true}
	info, err := mgr.Start(context.Background(), opts)
	if err != nil {
		t.Fatalf("shared-workspace Start: %v", err)
	}
	external := config.GetAgentDir(e.scion, "sw-agent", true)
	inProject := config.GetAgentDir(e.scion, "sw-agent", false)
	if external == inProject {
		t.Fatalf("fixture: expected distinct external and in-project agent dirs, got %s", external)
	}
	if _, err := os.Stat(filepath.Join(external, "scion-agent.json")); err != nil {
		t.Fatalf("fixture: shared-workspace agent state should be external: %v", err)
	}
	controlPlane, err := os.ReadFile(filepath.Join(external, controlPlaneInputsDirName, "instructions.md"))
	if err != nil {
		t.Fatalf("fixture: external inputs record: %v", err)
	}

	// An in-project copy of the agent directory, inside the shared
	// workspace mount.
	writeInProjectCopy := func(rel, body string) {
		t.Helper()
		p := filepath.Join(inProject, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeInProjectCopy("scion-agent.json", `{"harness_config":"hc-in-project","image":"in-project:latest","volumes":[{"source":"/etc","target":"/in-project"}]}`)
	writeInProjectCopy(filepath.Join(controlPlaneInputsDirName, "instructions.md"), "in-project instructions")
	writeInProjectCopy(filepath.Join(config.HarnessSecretsRecordDirName, "IN_PROJECT_TOKEN"), "in-project")

	// Restart without the shared-workspace flag (an older hub), capturing
	// the run config to show the in-project copy's volumes are never read.
	var runCfg runtime.RunConfig
	mgr = NewManager(&runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, cfg runtime.RunConfig) (string, error) {
			runCfg = cfg
			return "mock-id", nil
		},
	})
	opts.SharedWorkspace = false
	info2, err := mgr.Start(context.Background(), opts)
	if err != nil {
		t.Fatalf("restart without the flag: %v", err)
	}
	for _, v := range runCfg.Volumes {
		if v.Source == "/etc" || v.Target == "/in-project" {
			t.Errorf("a volume from the in-project scion-agent.json reached the run config: %+v", v)
		}
	}
	if runCfg.Image == "in-project:latest" {
		t.Error("the in-project scion-agent.json image was used")
	}
	if info2.HarnessConfig != info.HarnessConfig {
		t.Errorf("restart used a different harness-config %q (want %q): in-project config read?", info2.HarnessConfig, info.HarnessConfig)
	}
	home := config.GetAgentHomePath(e.scion, "sw-agent")
	got, err := os.ReadFile(filepath.Join(home, ".scion", "harness", "inputs", "instructions.md"))
	if err != nil || string(got) != string(controlPlane) {
		t.Errorf("inputs not restored from the broker-side record: %q (err=%v)", got, err)
	}
	if _, err := os.Stat(filepath.Join(home, ".scion", "harness", "secrets", "IN_PROJECT_TOKEN")); !os.IsNotExist(err) {
		t.Errorf("an in-project secrets record was restored (stat err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(inProject, controlPlaneInputsDirName, "instructions.md")); err == nil {
		if data, _ := os.ReadFile(filepath.Join(inProject, controlPlaneInputsDirName, "instructions.md")); string(data) != "in-project instructions" {
			t.Error("the restart wrote state into the in-project agent dir")
		}
	}
}

func TestEffectiveSharedWorkspace(t *testing.T) {
	e := newPolicyTestEnv(t)
	external := config.GetAgentDir(e.scion, "ext-agent", true)
	if external == config.GetAgentDir(e.scion, "ext-agent", false) {
		t.Fatal("fixture: expected an external agents root")
	}
	if effectiveSharedWorkspace(e.scion, "ext-agent", false, "") {
		t.Error("no external scion-agent.json: expected in-project")
	}
	if err := os.MkdirAll(external, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(external, "scion-agent.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !effectiveSharedWorkspace(e.scion, "ext-agent", false, "") {
		t.Error("external scion-agent.json present: expected the broker-side agent dir")
	}
	for _, bad := range []string{"", ".", "..", "a/b", "../ext-agent"} {
		if effectiveSharedWorkspace(e.scion, bad, false, "") {
			t.Errorf("name %q must not resolve externally", bad)
		}
	}
	if !effectiveSharedWorkspace(e.scion, "anything", true, "") {
		t.Error("an explicit shared-workspace flag stays shared")
	}
}

// Start/restart parity: a restart that carries the shared-workspace flag
// resolves the same broker-side agent dir as the start.
func TestSharedWorkspaceRestartWithFlagResolvesBrokerSideDir(t *testing.T) {
	e := newPolicyTestEnv(t)
	e.projectHC(t, "hc-scripted", policyTestScripted)
	mgr := policyTestManager(nil)
	opts := api.StartOptions{Name: "sw-flag", ProjectPath: e.scion, HarnessConfig: "hc-scripted", NoAuth: true, SharedWorkspace: true}
	if _, err := mgr.Start(context.Background(), opts); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := mgr.Start(context.Background(), opts); err != nil {
		t.Fatalf("restart: %v", err)
	}
	if _, err := os.Stat(config.GetAgentDir(e.scion, "sw-flag", false)); !os.IsNotExist(err) {
		t.Errorf("a start or restart created an in-project agent dir (stat err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(config.GetAgentDir(e.scion, "sw-flag", true), "scion-agent.json")); err != nil {
		t.Errorf("broker-side agent dir missing: %v", err)
	}
}

// A broker start or restart of a shared-workspace agent whose broker-side
// agent dir is missing fails closed (config.ErrAgentStateDirUnavailable,
// 409 at the broker) and creates nothing, in-project or external.
func TestSharedWorkspaceRestartMissingStateDirFailsClosed(t *testing.T) {
	e := newPolicyTestEnv(t)
	e.projectHC(t, "hc-scripted", policyTestScripted)
	runs := 0
	_, err := policyTestManager(&runs).Start(context.Background(), api.StartOptions{
		Name: "sw-missing", ProjectPath: e.scion, HarnessConfig: "hc-scripted", NoAuth: true,
		SharedWorkspace: true, BrokerMode: true,
	})
	if !errors.Is(err, config.ErrAgentStateDirUnavailable) {
		t.Fatalf("expected ErrAgentStateDirUnavailable, got %v", err)
	}
	if runs != 0 {
		t.Error("the container must not run")
	}
	for _, shared := range []bool{false, true} {
		if _, err := os.Stat(config.GetAgentDir(e.scion, "sw-missing", shared)); !os.IsNotExist(err) {
			t.Errorf("an agent dir was created (shared=%v, stat err=%v)", shared, err)
		}
	}
}

// With a hub-supplied project ID, the broker-side agents root comes from
// that ID: a project-id marker that disagrees with the hub project ID does not move it.
func TestAgentStateDirUsesHubProjectID(t *testing.T) {
	e := newPolicyTestEnv(t)
	hubID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	want, err := config.AgentDirForProject(e.scion, "hub-agent", true, hubID)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.WriteProjectID(e.scion, "99999999-8888-7777-6666-555555555555"); err != nil {
		t.Fatal(err)
	}
	dir, shared, err := agentStateDir(e.scion, "hub-agent", true, hubID, true)
	if err != nil || !shared || dir != want {
		t.Errorf("agentStateDir = %q (shared=%v, err=%v), want %q", dir, shared, err, want)
	}
	// An agent whose hub-ID external dir holds scion-agent.json stays
	// external without the flag; a scion-agent.json only under the
	// mismatched marker's root does not make it external.
	if err := os.MkdirAll(want, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(want, "scion-agent.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if dir, shared, err := agentStateDir(e.scion, "hub-agent", false, hubID, true); err != nil || !shared || dir != want {
		t.Errorf("without the flag: agentStateDir = %q (shared=%v, err=%v), want %q", dir, shared, err, want)
	}
	markerDir, err := config.AgentDirForProject(e.scion, "marker-agent", true, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(markerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(markerDir, "scion-agent.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, shared, _ := agentStateDir(e.scion, "marker-agent", false, hubID, true); shared {
		t.Error("a scion-agent.json under the marker-derived root made the agent external despite the hub project ID")
	}
}

// In broker mode, or with a hub project ID, a shared-workspace agent whose
// broker-side root cannot be determined is an error, never the in-project
// root; a local CLI start with neither keeps the in-project root.
func TestAgentStateDirStrictness(t *testing.T) {
	projectScion := filepath.Join(t.TempDir(), ".scion") // no project-id marker
	if err := os.MkdirAll(projectScion, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := agentStateDir(projectScion, "a", true, "", true); !errors.Is(err, config.ErrAgentStateDirUnavailable) {
		t.Errorf("strict: expected ErrAgentStateDirUnavailable, got %v", err)
	}
	dir, _, err := agentStateDir(projectScion, "a", true, "", false)
	if err != nil || dir != filepath.Join(projectScion, "agents", "a") {
		t.Errorf("local without a project ID: got %q (err=%v), want the in-project dir", dir, err)
	}
}
