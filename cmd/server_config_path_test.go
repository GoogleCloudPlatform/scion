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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// keepServerConfigPath restores the --config global after the test.
func keepServerConfigPath(t *testing.T) {
	t.Helper()
	saved := serverConfigPath
	t.Cleanup(func() { serverConfigPath = saved })
}

func TestAbsolutizeServerConfigPath(t *testing.T) {
	keepServerConfigPath(t)
	launch := t.TempDir()
	t.Chdir(launch)

	serverConfigPath = ""
	require.NoError(t, absolutizeServerConfigPath())
	assert.Empty(t, serverConfigPath, "an empty --config stays empty")

	serverConfigPath = filepath.Join("conf", "hub.yaml")
	require.NoError(t, absolutizeServerConfigPath())
	assert.Equal(t, filepath.Join(launch, "conf", "hub.yaml"), serverConfigPath)

	abs := filepath.Join(t.TempDir(), "server")
	serverConfigPath = abs
	require.NoError(t, absolutizeServerConfigPath())
	assert.Equal(t, abs, serverConfigPath, "an absolute --config is unchanged")
}

// A relative --config resolved at start names the launch directory's file
// in the hub's config, even after the server changes directory.
func TestAbsolutizeServerConfigPath_ThenChdir_HubConfigPath(t *testing.T) {
	keepServerConfigPath(t)
	launch := t.TempDir()
	t.Chdir(launch)
	serverConfigPath = filepath.Join("conf", "hub.yaml")

	require.NoError(t, absolutizeServerConfigPath())
	t.Chdir(t.TempDir()) // the server changes directory (--global)
	hubCfg := buildHubServerConfig(&config.GlobalConfig{}, "https://hub.example.com", "", nil, false, "", nil)

	assert.Equal(t, filepath.Join(launch, "conf", "hub.yaml"), hubCfg.ConfigPath)
}

// A background start hands the daemon child (which runs in the global
// directory) an absolute --config, resolved against the launch directory.
func TestAbsolutizeServerConfigPath_DaemonArgs(t *testing.T) {
	keepServerConfigPath(t)
	launch := t.TempDir()
	t.Chdir(launch)

	c := &cobra.Command{Use: "start", RunE: func(*cobra.Command, []string) error { return nil }}
	c.Flags().StringVarP(&serverConfigPath, "config", "c", "", "")
	c.SetArgs([]string{"--config=conf/hub.yaml"})
	require.NoError(t, c.Execute())

	require.NoError(t, absolutizeServerConfigPath())
	assert.Contains(t, buildDaemonStartArgs(c), "--config="+filepath.Join(launch, "conf", "hub.yaml"))
}

// End to end at the cmd level: with a relative --config that names a
// directory holding a server.yaml, the hub's ConfigPath still lets the
// legacy server.yaml guard find that file after the server changes
// directory, so the guard refuses a save that would drop it. Without the
// absolutize step (the control), the guard finds nothing: the relative path
// resolves against the new directory, and the isolated HOME has no
// server.yaml of its own.
func TestAbsolutizeServerConfigPath_LegacyGuardSeesServerYAMLAfterChdir(t *testing.T) {
	for _, tc := range []struct {
		name       string
		absolutize bool
	}{
		{"absolutized", true},
		{"control without absolutize", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keepServerConfigPath(t)
			clearSettingsEnv(t)
			t.Setenv("HOME", t.TempDir())
			launch := t.TempDir()
			require.NoError(t, os.Mkdir(filepath.Join(launch, "conf"), 0o755))
			serverYAML := filepath.Join(launch, "conf", "server.yaml")
			require.NoError(t, os.WriteFile(serverYAML, []byte("hub:\n  port: 7000\n"), 0o644))
			t.Chdir(launch)
			serverConfigPath = "conf"

			if tc.absolutize {
				require.NoError(t, absolutizeServerConfigPath())
			}
			t.Chdir(t.TempDir()) // the server changes directory (--global)
			hubCfg := buildHubServerConfig(&config.GlobalConfig{}, "https://hub.example.com", "", nil, false, "", nil)
			sources := config.LegacyServerConfigSources(hubCfg.ConfigPath)

			if tc.absolutize {
				assert.Equal(t, []string{serverYAML}, sources, "the guard must see the server.yaml next to --config")
			} else {
				assert.Empty(t, sources, "control: the relative path no longer finds the file")
			}
		})
	}
}

// absolutizeServerConfigPath must run before anything reads --config: as
// the second statement of runServerStart (pinProcessUTC must stay first)
// and the first statement of runServerStartOrDaemon (before the
// --foreground branch, so it covers the daemon child's args too).
func TestAbsolutizeServerConfigPath_CallSites(t *testing.T) {
	want := map[string]int{
		"runServerStart":         1,
		"runServerStartOrDaemon": 0,
	}
	found := map[string]bool{}
	fset := token.NewFileSet()
	for _, name := range []string{"server_foreground.go", "server_daemon.go"} {
		f, err := parser.ParseFile(fset, name, nil, 0)
		require.NoError(t, err)
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Recv != nil {
				continue
			}
			idx, ok := want[fd.Name.Name]
			if !ok {
				continue
			}
			found[fd.Name.Name] = true
			if len(fd.Body.List) <= idx || !isAbsolutizeIfStmt(fd.Body.List[idx]) {
				t.Errorf("%s: statement %d must be `if err := absolutizeServerConfigPath(); err != nil { return err }`", fd.Name.Name, idx)
			}
		}
	}
	for fn := range want {
		assert.True(t, found[fn], "function %s not found", fn)
	}
}

// isAbsolutizeIfStmt reports whether stmt is
// `if err := absolutizeServerConfigPath(); err != nil { return err }`.
func isAbsolutizeIfStmt(stmt ast.Stmt) bool {
	ifs, ok := stmt.(*ast.IfStmt)
	if !ok || ifs.Else != nil || len(ifs.Body.List) != 1 {
		return false
	}
	init, ok := ifs.Init.(*ast.AssignStmt)
	if !ok || len(init.Rhs) != 1 || len(init.Lhs) != 1 {
		return false
	}
	call, ok := init.Rhs[0].(*ast.CallExpr)
	if !ok || len(call.Args) != 0 {
		return false
	}
	fn, ok := call.Fun.(*ast.Ident)
	if !ok || fn.Name != "absolutizeServerConfigPath" {
		return false
	}
	_, isReturn := ifs.Body.List[0].(*ast.ReturnStmt)
	return isReturn
}
