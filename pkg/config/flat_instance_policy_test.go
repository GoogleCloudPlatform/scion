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

package config

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
)

// Flat Runtime Broker instance policy
// (.design/flat-runtime-brokers/contract-appendix-p3.md FC5).

func k8sPolicy(key, class, size string) *FlatInstancePolicy {
	return NewFlatInstancePolicy(V1RuntimeBrokerInstanceConfig{Key: key, Name: key,
		RuntimeTarget: &V1RuntimeTargetConfig{Type: RuntimeTargetTypeKubernetes, SharedDirStorageClass: class, SharedDirSize: size}})
}

// legacySharedDirSettings has an active profile and runtime entry whose
// shared-dir defaults must never reach a flat view.
func legacySharedDirSettings() *VersionedSettings {
	return &VersionedSettings{
		ActiveProfile: "batch",
		Runtimes:      map[string]V1RuntimeConfig{"cluster": {Type: "kubernetes", SharedDirStorageClass: "rt-rwx", SharedDirSize: "20Gi"}},
		Profiles:      map[string]V1ProfileConfig{"batch": {Runtime: "cluster", SharedDirStorageClass: "prof-rwx", SharedDirSize: "30Gi"}},
	}
}

func TestFlatInstancePolicy_ContextCarriesPolicyAndDerivesMode(t *testing.T) {
	if FlatInstancePolicyFrom(context.Background()) != nil {
		t.Fatal("a plain context carries no policy")
	}
	if ProfileResolutionFrom(context.Background()) != ProfileResolutionLegacy {
		t.Fatal("a plain context is legacy resolution")
	}
	p := k8sPolicy("k8s-1", "", "40Gi")
	ctx := WithFlatInstancePolicy(context.Background(), p)
	if FlatInstancePolicyFrom(ctx) != p {
		t.Fatal("the context must return the attached policy itself")
	}
	if ProfileResolutionFrom(ctx) != ProfileResolutionFlatInstance {
		t.Fatal("a context with a policy is flat instance resolution")
	}
	if ProfileResolutionFrom(WithFlatInstancePolicy(context.Background(), nil)) != ProfileResolutionLegacy {
		t.Fatal("a nil policy is legacy resolution")
	}
}

// The policy is a deep copy: changing the loaded entry afterwards never
// changes it.
func TestNewFlatInstancePolicy_DeepCopies(t *testing.T) {
	inst := V1RuntimeBrokerInstanceConfig{Key: "k8s-1", RuntimeTarget: &V1RuntimeTargetConfig{Type: "kubernetes",
		SharedDirSize: "40Gi", KubernetesServiceAccountMappings: map[string]string{"a@p.iam.gserviceaccount.com": "ksa"}}}
	p := NewFlatInstancePolicy(inst)
	inst.RuntimeTarget.SharedDirSize = "1Gi"
	inst.RuntimeTarget.KubernetesServiceAccountMappings["a@p.iam.gserviceaccount.com"] = "changed"
	if p.Target.SharedDirSize != "40Gi" || p.Target.KubernetesServiceAccountMappings["a@p.iam.gserviceaccount.com"] != "ksa" {
		t.Fatalf("policy shares state with the entry: %+v", p.Target)
	}
	if got := NewFlatInstancePolicy(V1RuntimeBrokerInstanceConfig{Key: "bare"}); got.InstanceKey != "bare" || !reflect.DeepEqual(got.Target, V1RuntimeTargetConfig{}) {
		t.Fatalf("an entry without a target gives the zero target: %+v", got)
	}
}

// TestNewFlatInstancePolicy_CopiesEveryReferenceField: every pointer, map
// or slice field of V1RuntimeTargetConfig is copied, so a field added later
// cannot be shared between the policy and the entry. A field of a kind this
// test cannot populate fails it, so the test is extended with the field.
func TestNewFlatInstancePolicy_CopiesEveryReferenceField(t *testing.T) {
	target := &V1RuntimeTargetConfig{}
	v := reflect.ValueOf(target).Elem()
	var refFields []string
	for i := 0; i < v.NumField(); i++ {
		f, name := v.Field(i), v.Type().Field(i).Name
		switch f.Kind() {
		case reflect.Map:
			if f.Type().Key().Kind() != reflect.String || f.Type().Elem().Kind() != reflect.String {
				t.Fatalf("field %s: unsupported map type %s; extend this test", name, f.Type())
			}
			f.Set(reflect.ValueOf(map[string]string{"k": "v"}))
		case reflect.Pointer:
			if f.Type().Elem().Kind() != reflect.Bool {
				t.Fatalf("field %s: unsupported pointer type %s; extend this test", name, f.Type())
			}
			b := true
			f.Set(reflect.ValueOf(&b))
		case reflect.Slice, reflect.Interface, reflect.Chan, reflect.Func,
			reflect.Struct, reflect.Array, reflect.UnsafePointer:
			// A struct or array can hold a map or pointer of its own.
			t.Fatalf("field %s: unsupported kind %s; extend this test", name, f.Kind())
		default:
			continue
		}
		refFields = append(refFields, name)
	}
	p := NewFlatInstancePolicy(V1RuntimeBrokerInstanceConfig{Key: "k", RuntimeTarget: target})
	pv := reflect.ValueOf(&p.Target).Elem()
	for _, name := range refFields {
		f := v.FieldByName(name)
		switch f.Kind() {
		case reflect.Map:
			f.SetMapIndex(reflect.ValueOf("k"), reflect.ValueOf("changed"))
			if got := pv.FieldByName(name).MapIndex(reflect.ValueOf("k")); !got.IsValid() || got.String() != "v" {
				t.Errorf("field %s is shared between the policy and the entry", name)
			}
		case reflect.Pointer:
			f.Elem().SetBool(false)
			if got := pv.FieldByName(name); got.IsNil() || !got.Elem().Bool() {
				t.Errorf("field %s is shared between the policy and the entry", name)
			}
		}
	}
	if len(refFields) == 0 {
		t.Fatal("V1RuntimeTargetConfig has no reference field; the test checks nothing")
	}
	// Not vacuous for the known reference fields.
	for _, want := range []string{"KubernetesServiceAccountMappings", "SafeToEvict", "SharedDirStorageBackends"} {
		if !slices.Contains(refFields, want) {
			t.Fatalf("reference field %s was not checked (checked: %v)", want, refFields)
		}
	}
}

func TestForFlatInstance_ViewAndNoMutation(t *testing.T) {
	vs := legacySharedDirSettings()
	vs.ImageRegistry = "registry.example/global"
	p := k8sPolicy("k8s-1", "inst-rwx", "40Gi")
	view := vs.ForFlatInstance(p)
	if view == vs {
		t.Fatal("a flat view must be a copy")
	}
	if view.ActiveProfile != "" || view.Profiles != nil {
		t.Fatalf("a flat view has no profile tier: %+v", view)
	}
	if view.ImageRegistry != "registry.example/global" {
		t.Fatal("non-profile settings are shared unchanged")
	}
	if vs.ActiveProfile != "batch" || len(vs.Profiles) != 1 || vs.flat != nil {
		t.Fatal("the loaded settings were modified")
	}
	if vs.ForFlatInstance(nil) != vs {
		t.Fatal("a nil policy returns the settings themselves")
	}
	var nilVS *VersionedSettings
	if nilVS.ForFlatInstance(p) != nil {
		t.Fatal("a nil settings stays nil")
	}
}

// Behavioural guard: with profiles and runtime entries present in the
// underlying settings, a flat view returns only policy values; the legacy
// view still returns the profile tier.
func TestResolveSharedDirDefaults_FlatBranchReadsOnlyPolicy(t *testing.T) {
	vs := legacySharedDirSettings()
	view := vs.ForFlatInstance(k8sPolicy("k8s-nfs-1", "inst-rwx", "40Gi"))
	for _, profile := range []string{"", "batch"} {
		class, size, key := view.ResolveSharedDirDefaultsWithSource(profile)
		if class != "inst-rwx" || size != "40Gi" || key != "server.broker.instances[k8s-nfs-1].runtime_target.shared_dir_size" {
			t.Fatalf("profile %q: got (%q, %q, %q), want the instance values and path", profile, class, size, key)
		}
	}
	if class, size := view.ResolveSharedDirDefaults("batch"); class != "inst-rwx" || size != "40Gi" {
		t.Fatalf("ResolveSharedDirDefaults: got (%q, %q)", class, size)
	}
	if _, size, key := vs.ResolveSharedDirDefaultsWithSource(""); size != "30Gi" || key != "profiles.batch.shared_dir_size" {
		t.Fatalf("legacy resolution changed: (%q, %q)", size, key)
	}
}

// Fall-through: shared-dir class and size have no settings rung below the
// profile/runtime-entry rung, so an unset instance value is unset (the
// runtime default applies), never a profile value.
func TestResolveSharedDirDefaults_FlatUnsetFallsToRuntimeDefault(t *testing.T) {
	view := legacySharedDirSettings().ForFlatInstance(k8sPolicy("k8s-1", "", ""))
	if class, size, key := view.ResolveSharedDirDefaultsWithSource("batch"); class != "" || size != "" || key != "" {
		t.Fatalf("got (%q, %q, %q), want unset", class, size, key)
	}
	view = legacySharedDirSettings().ForFlatInstance(k8sPolicy("k8s-1", "inst-rwx", ""))
	if class, size, key := view.ResolveSharedDirDefaultsWithSource(""); class != "inst-rwx" || size != "" || key != "" {
		t.Fatalf("class only: got (%q, %q, %q)", class, size, key)
	}
}

// --- Guard test: no flat branch reaches the profile tier. ---

// flatGuardForbidden are the identifiers a flat branch, and every function
// it reaches in this package, must not reference.
var flatGuardForbidden = map[string]bool{"ResolveProfileValue": true, "Profiles": true, "Runtimes": true}

// isFlatBranchCond reports whether cond tests "<x>.flat != nil".
func isFlatBranchCond(cond ast.Expr) bool {
	found := false
	ast.Inspect(cond, func(n ast.Node) bool {
		if b, ok := n.(*ast.BinaryExpr); ok && b.Op == token.NEQ {
			if sel, ok := b.X.(*ast.SelectorExpr); ok && sel.Sel.Name == "flat" {
				if id, ok := b.Y.(*ast.Ident); ok && id.Name == "nil" {
					found = true
				}
			}
		}
		return !found
	})
	return found
}

// flatBranches returns the bodies of fn's "if <x>.flat != nil" statements.
func flatBranches(fn *ast.FuncDecl) []*ast.BlockStmt {
	var out []*ast.BlockStmt
	if fn.Body == nil {
		return nil
	}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if s, ok := n.(*ast.IfStmt); ok && isFlatBranchCond(s.Cond) {
			out = append(out, s.Body)
		}
		return true
	})
	return out
}

func funcDeclName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	t := fn.Recv.List[0].Type
	if st, ok := t.(*ast.StarExpr); ok {
		t = st.X
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name + "." + fn.Name.Name
	}
	return fn.Name.Name
}

// flatGuardViolations scans the flat branches of every function in files
// and, transitively, every package function or method they call. A callee
// that has flat branches of its own is scanned only in those branches (it is
// reached on the flat view); any other callee is scanned whole. Methods are
// matched by name across receivers, which can only over-report. It returns
// the violations and the functions that have flat branches.
func flatGuardViolations(files []*ast.File) (violations, flatFuncs []string) {
	funcs := map[string][]*ast.FuncDecl{} // plain name or method name -> decls
	var all []*ast.FuncDecl
	for _, f := range files {
		for _, d := range f.Decls {
			if fn, ok := d.(*ast.FuncDecl); ok {
				funcs[fn.Name.Name] = append(funcs[fn.Name.Name], fn)
				all = append(all, fn)
			}
		}
	}
	visited := map[*ast.FuncDecl]bool{}
	var scan func(origin string, node ast.Node)
	scanFunc := func(origin string, fn *ast.FuncDecl) {
		if visited[fn] {
			return
		}
		visited[fn] = true
		if branches := flatBranches(fn); len(branches) > 0 {
			for _, b := range branches {
				scan(origin+" -> "+funcDeclName(fn), b)
			}
			return
		}
		if fn.Body != nil {
			scan(origin+" -> "+funcDeclName(fn), fn.Body)
		}
	}
	scan = func(origin string, node ast.Node) {
		ast.Inspect(node, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.Ident:
				if flatGuardForbidden[x.Name] {
					violations = append(violations, origin+": references "+x.Name)
				}
			case *ast.CallExpr:
				var name string
				switch fun := x.Fun.(type) {
				case *ast.Ident:
					name = fun.Name
				case *ast.SelectorExpr:
					name = fun.Sel.Name
				case *ast.IndexExpr: // generic instantiation f[T](...)
					if id, ok := fun.X.(*ast.Ident); ok {
						name = id.Name
					}
				}
				for _, callee := range funcs[name] {
					scanFunc(origin, callee)
				}
			}
			return true
		})
	}
	for _, fn := range all {
		branches := flatBranches(fn)
		if len(branches) == 0 {
			continue
		}
		flatFuncs = append(flatFuncs, funcDeclName(fn))
		visited[fn] = true
		for _, b := range branches {
			scan(funcDeclName(fn), b)
		}
	}
	sort.Strings(violations)
	sort.Strings(flatFuncs)
	return violations, flatFuncs
}

func parsePackageSources(t *testing.T, dir string) []*ast.File {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	return files
}

// TestFlatBranches_NeverReachProfileTier is the frozen guard (FC5): no
// function reachable from a flat branch in pkg/config references
// ResolveProfileValue, Profiles or Runtimes.
func TestFlatBranches_NeverReachProfileTier(t *testing.T) {
	violations, flatFuncs := flatGuardViolations(parsePackageSources(t, "."))
	if len(violations) > 0 {
		t.Fatalf("flat branches reach the profile tier:\n%s", strings.Join(violations, "\n"))
	}
	// Not vacuous: every wired flat branch is found.
	for _, want := range []string{
		"VersionedSettings.ResolveSharedDirDefaultsWithSource",
		"VersionedSettings.ResolveSafeToEvictWithSource",
		"VersionedSettings.ResolveSharedDirStorage",
		"VersionedSettings.ResolveSharedDirStorageBackend",
		"VersionedSettings.ResolveHomeStorage",
	} {
		found := false
		for _, f := range flatFuncs {
			found = found || f == want
		}
		if !found {
			t.Fatalf("guard found no flat branch in %s (flat functions: %v)", want, flatFuncs)
		}
	}
}

// The guard catches a flat branch that reaches the profile tier directly
// or through a helper.
func TestFlatGuard_DetectsViolations(t *testing.T) {
	src := `package config
func (vs *VersionedSettings) Direct() string {
	if vs.flat != nil {
		return vs.Profiles["x"].Runtime
	}
	return ""
}
func (vs *VersionedSettings) Indirect() string {
	if vs != nil && vs.flat != nil {
		return helper(vs)
	}
	return ""
}
func helper(vs *VersionedSettings) string {
	v, _ := ResolveProfileValue(vs, "", "k", nil, nil)
	_ = vs.Runtimes
	return v
}
func (vs *VersionedSettings) Clean() string {
	if vs.flat != nil {
		return vs.flat.path("x")
	}
	return vs.Profiles["x"].Runtime // legacy body: not scanned
}
`
	f, err := parser.ParseFile(token.NewFileSet(), "synthetic.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	violations, flatFuncs := flatGuardViolations([]*ast.File{f})
	want := []string{
		"VersionedSettings.Direct: references Profiles",
		"VersionedSettings.Indirect -> helper: references ResolveProfileValue",
		"VersionedSettings.Indirect -> helper: references Runtimes",
	}
	if !reflect.DeepEqual(violations, want) {
		t.Fatalf("violations:\n got %q\nwant %q", violations, want)
	}
	if !reflect.DeepEqual(flatFuncs, []string{"VersionedSettings.Clean", "VersionedSettings.Direct", "VersionedSettings.Indirect"}) {
		t.Fatalf("flat functions: %v", flatFuncs)
	}
}
