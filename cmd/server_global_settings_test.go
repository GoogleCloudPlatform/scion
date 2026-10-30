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

package cmd

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	scionruntime "github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// serverStartupSettingsView is what the server's startup settings loads
// read, as observed from one working directory.
type serverStartupSettingsView struct {
	versionedActiveProfile string
	versionedRegistry      string
	versionedBrokerID      string
	effectiveActiveProfile string
	effectiveRegistry      string
	effectiveBrokerID      string
	colocatedActiveProfile string
	registryCheckErr       bool
	defaultRuntimePath     string
	runtimeProjectDir      string
}

// TestServerStartupSettings_GlobalOnlyFromProjectDir checks that starting
// the server from inside a project directory changes none of its startup
// settings loads (ptone/scion#5362): the versioned and effective settings
// loaders, the co-located active profile, the image registry check and the
// project path the default runtime is resolved for all read the global
// settings directory, not the project's settings.
func TestServerStartupSettings_GlobalOnlyFromProjectDir(t *testing.T) {
	home, globalDir := brokerTestHome(t)
	t.Setenv("SCION_IMAGE_REGISTRY", "")
	t.Setenv("SCION_MAINTENANCE_IMAGE_REGISTRY", "")
	t.Setenv("SCION_ACTIVE_PROFILE", "")
	t.Setenv("SCION_PROJECT", "")
	t.Setenv("SCION_PROJECT_ID", "")
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(
		"schema_version: \"1\"\nactive_profile: global-profile\nprofiles:\n  global-profile:\n    runtime: docker\n"+
			"server:\n  broker:\n    broker_id: global-broker\n"), 0o644))

	project := t.TempDir()
	projectScion := filepath.Join(project, ".scion")
	require.NoError(t, os.MkdirAll(projectScion, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(projectScion, "settings.yaml"), []byte(
		"schema_version: \"1\"\nactive_profile: project-profile\nprofiles:\n  project-profile:\n    runtime: docker\n"+
			"image_registry: registry.example.com/project-only\nserver:\n  broker:\n    broker_id: project-broker\n"), 0o644))

	observe := func(t *testing.T, dir string) serverStartupSettingsView {
		t.Helper()
		t.Chdir(dir)
		var v serverStartupSettingsView
		vs, err := loadServerVersionedSettings()
		require.NoError(t, err)
		v.versionedActiveProfile, v.versionedRegistry = vs.ActiveProfile, vs.SettingsImageRegistry("")
		if vs.Server != nil && vs.Server.Broker != nil {
			v.versionedBrokerID = vs.Server.Broker.BrokerID
		}
		es, _, err := loadServerEffectiveSettings()
		require.NoError(t, err)
		v.effectiveActiveProfile, v.effectiveRegistry = es.ActiveProfile, es.SettingsImageRegistry("")
		if es.Server != nil && es.Server.Broker != nil {
			v.effectiveBrokerID = es.Server.Broker.BrokerID
		}
		v.colocatedActiveProfile = colocatedActiveProfile()
		v.registryCheckErr = requireImageRegistryForBrokerAt(globalDir) != nil
		_, _, err = resolveBrokerDefaultRuntime(func(projectPath, _ string) (scionruntime.Runtime, config.RuntimeBuildRecord) {
			v.defaultRuntimePath = projectPath
			return scionruntime.NewDockerRuntime(), config.RuntimeBuildRecord{}
		}, func(string, ...interface{}) {})
		require.NoError(t, err)
		v.runtimeProjectDir, err = config.GetResolvedProjectDir(v.defaultRuntimePath)
		require.NoError(t, err)
		return v
	}

	// The project layer is really there: a cwd-resolved load merges it.
	t.Chdir(project)
	merged, _, err := config.LoadEffectiveSettings("")
	require.NoError(t, err)
	require.Equal(t, "project-profile", merged.ActiveProfile, "test setup: the project settings must be visible to a cwd-resolved load")
	require.NoError(t, requireImageRegistryForBrokerAt(""), "test setup: the project registry must be visible to a cwd-resolved check")

	fromHome := observe(t, home)
	fromProject := observe(t, project)
	assert.Equal(t, serverStartupSettingsView{
		versionedActiveProfile: "global-profile",
		versionedBrokerID:      "global-broker",
		effectiveActiveProfile: "global-profile",
		effectiveBrokerID:      "global-broker",
		colocatedActiveProfile: "global-profile",
		registryCheckErr:       true,
		defaultRuntimePath:     serverRuntimeProjectPath,
		runtimeProjectDir:      globalDir,
	}, fromHome)
	assert.Equal(t, fromHome, fromProject, "starting from inside a project changed the server's startup settings")
}

// TestServerStartup_NoWorkingDirectorySettingsLoads guards the server's
// startup code, and the hub's system runtime handlers, against a new
// settings load that resolves the project from the working directory. It is
// a literal-only tripwire: it flags a call whose project-path argument is
// the literal "" (a variable holding "" is not caught):
//   - config.LoadSettings, LoadSettingsIgnoringEnvProjectID,
//     LoadVersionedSettings, LoadEffectiveSettings, LoadEffectiveSettingsFor
//     and GetResolvedProjectDir
//   - runtime.GetRuntime and runtime.GetRuntimeWithRecord
//   - requireImageRegistryForBrokerAt and imageRegistryConfiguredEnvOrFile
//
// and any call of requireImageRegistryForBroker (the standalone broker's
// working-directory check).
func TestServerStartup_NoWorkingDirectorySettingsLoads(t *testing.T) {
	configFns := map[string]bool{
		"LoadSettings": true, "LoadSettingsIgnoringEnvProjectID": true,
		"LoadVersionedSettings": true, "LoadEffectiveSettings": true, "LoadEffectiveSettingsFor": true,
		"GetResolvedProjectDir": true,
	}
	localFns := map[string]bool{"requireImageRegistryForBrokerAt": true, "imageRegistryConfiguredEnvOrFile": true}
	// The argument that names the project path, by function.
	pathArg := func(pkg, fn string) int {
		if pkg == "config" && fn == "LoadEffectiveSettingsFor" {
			return 1 // (ctx, projectPath)
		}
		return 0
	}
	files := []string{
		"server_foreground.go", "server_flat_host.go", "server_global_settings.go", "image_registry_presence.go",
		filepath.Join("..", "pkg", "hub", "system_handlers.go"),
	}
	for _, file := range files {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, file, nil, 0)
		require.NoError(t, err)
		ast.Inspect(f, func(n ast.Node) bool {
			if fn, ok := n.(*ast.FuncDecl); ok {
				switch {
				case file == "image_registry_presence.go" && fn.Name.Name != "colocatedActiveProfile":
					// Only colocatedActiveProfile in this file runs at server
					// startup; the rest serves the standalone broker too.
					return false
				case fn.Name.Name == "requireImageRegistryForBroker":
					// The standalone broker's working-directory check.
					return false
				}
			}
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			isEmptyLit := func(i int) bool {
				if i >= len(call.Args) {
					return false
				}
				lit, ok := call.Args[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return false
				}
				v, err := strconv.Unquote(lit.Value)
				return err == nil && v == ""
			}
			switch fun := call.Fun.(type) {
			case *ast.Ident:
				if fun.Name == "requireImageRegistryForBroker" {
					t.Errorf("%s: requireImageRegistryForBroker resolves the project from the working directory; use the global directory", fset.Position(call.Pos()))
				}
				if localFns[fun.Name] && isEmptyLit(0) {
					t.Errorf("%s: %s(\"\") resolves the project from the working directory; use the global directory", fset.Position(call.Pos()), fun.Name)
				}
			case *ast.SelectorExpr:
				pkg, ok := fun.X.(*ast.Ident)
				if !ok {
					return true
				}
				if (pkg.Name == "config" && configFns[fun.Sel.Name]) || (pkg.Name == "runtime" && (fun.Sel.Name == "GetRuntime" || fun.Sel.Name == "GetRuntimeWithRecord")) {
					if isEmptyLit(pathArg(pkg.Name, fun.Sel.Name)) {
						t.Errorf("%s: %s.%s(\"\") resolves the project from the working directory; use the global directory", fset.Position(call.Pos()), pkg.Name, fun.Sel.Name)
					}
				}
			}
			return true
		})
	}
}
