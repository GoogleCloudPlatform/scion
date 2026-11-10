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

//go:build !no_sqlite

package hub

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// localGateFakeGit puts a fake git first on PATH for the rest of the test.
// Every invocation's argv is appended to the returned argv log, one
// argument per line, followed by an "--END--" line, and every clone's
// GIT_ALLOW_PROTOCOL to the env log. A clone becomes a local init of its
// last argument (the destination); anything else runs the real git.
func localGateFakeGit(t *testing.T) (argvLog, envLog string) {
	t.Helper()
	realGit, err := exec.LookPath("git")
	require.NoError(t, err, "git is required for this test")
	root := t.TempDir()
	argvLog = filepath.Join(root, "argv.log")
	envLog = filepath.Join(root, "env.log")
	binDir := filepath.Join(root, "bin")
	require.NoError(t, os.MkdirAll(binDir, 0o755))
	script := "#!/bin/sh\n" +
		"for a in \"$@\"; do printf '%s\\n' \"$a\" >> '" + argvLog + "'; done\n" +
		"printf '%s\\n' '--END--' >> '" + argvLog + "'\n" +
		"last=''; isclone=0\n" +
		"for a in \"$@\"; do last=\"$a\"; [ \"$a\" = clone ] && isclone=1; done\n" +
		"if [ \"$isclone\" = 1 ]; then\n" +
		"  printf 'GIT_ALLOW_PROTOCOL=%s\\n' \"${GIT_ALLOW_PROTOCOL-<unset>}\" >> '" + envLog + "'\n" +
		"  exec '" + realGit + "' init -q \"$last\"\n" +
		"fi\n" +
		"exec '" + realGit + "' \"$@\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "git"), []byte(script), 0o755))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return argvLog, envLog
}

// localGateCloneInvocations returns the argv of every logged git invocation
// that runs the clone subcommand (none when the log does not exist).
func localGateCloneInvocations(t *testing.T, argvLog string) [][]string {
	t.Helper()
	data, err := os.ReadFile(argvLog)
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(t, err)
	var out [][]string
	var cur []string
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if line == "--END--" {
			if slices.Contains(cur, "clone") {
				out = append(out, cur)
			}
			cur = nil
			continue
		}
		cur = append(cur, line)
	}
	return out
}

// localGateAssertHardenedClone asserts that argv is a clone of cloneURL into
// dest carrying the hardened host-clone settings, and that every clone ran
// with the hardened transport allow-list in its environment.
func localGateAssertHardenedClone(t *testing.T, argv []string, cloneURL, dest, envLog string) {
	t.Helper()
	clone := slices.Index(argv, "clone")
	require.GreaterOrEqual(t, clone, 0, "argv: %q", argv)
	lastFile := ""
	for _, a := range argv[:clone] {
		if strings.HasPrefix(a, "protocol.file.allow=") {
			lastFile = a
		}
	}
	assert.Equal(t, "protocol.file.allow=never", lastFile,
		"the last protocol.file.allow ahead of clone must be never: %q", argv)
	assert.Contains(t, argv[:clone], "protocol.allow=never", "argv: %q", argv)
	assert.Equal(t, 1, strings.Count(strings.Join(argv[:clone], "\n")+"\n", "protocol.file.allow=user\n"),
		"the base host pins appear once: %q", argv)
	assert.Contains(t, argv[:clone], "http.followRedirects=false", "argv: %q", argv)
	n := len(argv)
	require.GreaterOrEqual(t, n, 3)
	assert.Equal(t, []string{"--", cloneURL, dest}, argv[n-3:], "the URL must follow --")
	env, err := os.ReadFile(envLog)
	require.NoError(t, err)
	for _, line := range strings.Split(strings.TrimSpace(string(env)), "\n") {
		assert.Equal(t, "GIT_ALLOW_PROTOCOL="+provision.HostCloneProtocols, line)
	}
}

// localGateSourceRepo returns a local git repository with one commit.
func localGateSourceRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "test@test.com"},
		{"config", "user.name", "Test"},
		{"commit", "-q", "--allow-empty", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
	}
	return dir
}

// On the local backend, cloneSharedWorkspaceProject refuses a clone URL a
// hardened host clone may not use, before any git process or filesystem
// work, with the same error the shared backend returns.
func TestLocalBackendClone_RefusesHostCloneURLs(t *testing.T) {
	identityTestHome(t)
	srv, _ := testServer(t)
	require.False(t, srv.workspaceBackendShared(), "the test server uses the local backend")
	source := localGateSourceRepo(t)

	tests := []struct {
		name  string
		label string
		// verbatim: the label reaches the check unchanged (not rewritten
		// or dropped by clone URL normalization).
		verbatim bool
	}{
		{"local path", source, true},
		{"file URL", "file://" + source, true},
		{"http URL", "http://git.example.invalid/org/repo.git", true},
		{"transport form", "fd::3", false},
		{"transport form ext", "ext::git", false},
		{"leading dash", "-uoption-shaped", false},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			argvLog, _ := localGateFakeGit(t)
			if tt.verbatim {
				require.Equal(t, tt.label, resolveCloneURL(tt.label, ""))
			}
			project := sharedWorkspaceProject("local-gate-refuse-" + string(rune('a'+i)))
			project.GitRemote = ""
			project.Labels[store.LabelCloneURL] = tt.label
			workspacePath, err := localProjectPath(project.Slug)
			require.NoError(t, err)

			err = srv.cloneSharedWorkspaceProject(context.Background(), project)
			require.ErrorIs(t, err, provision.ErrHostCloneURLRefused)
			assert.Equal(t, provision.ErrHostCloneURLRefused.Error(), err.Error(), "fixed text, no part of the URL")
			assert.Empty(t, localGateCloneInvocations(t, argvLog), "no clone may run for a refused URL")
			_, statErr := os.Lstat(workspacePath)
			assert.True(t, os.IsNotExist(statErr), "a refused URL must not create the workspace")
		})
	}
}

// Legitimate https and ssh clone URLs still clone on the local backend,
// through the hardened host-clone settings. An ssh clone URL needs a project
// git secret, so the ssh cases give the project one.
func TestLocalBackendClone_HTTPSAndSSHCloneHardened(t *testing.T) {
	for i, cloneURL := range []string{
		"https://git.example.invalid/org/repo.git",
		"ssh://git@git.example.invalid/org/repo.git",
		"git@git.example.invalid:org/repo.git",
	} {
		t.Run(cloneURL, func(t *testing.T) {
			identityTestHome(t)
			srv, st := testServer(t)
			argvLog, envLog := localGateFakeGit(t)
			project := sharedWorkspaceProject("local-gate-ok-" + string(rune('a'+i)))
			project.Labels[store.LabelCloneURL] = cloneURL
			if !strings.HasPrefix(cloneURL, "https://") {
				backend := secret.NewLocalBackend(st, "local-gate-hub", "local-gate-secret")
				srv.SetSecretBackend(backend)
				_, _, err := backend.Set(context.Background(), &secret.SetSecretInput{
					Name: "GITHUB_TOKEN", Value: "local-gate-token", SecretType: store.SecretTypeEnvironment,
					Target: "GITHUB_TOKEN", Scope: store.ScopeProject, ScopeID: project.ID,
					CreatedBy: DevUserID, UpdatedBy: DevUserID,
				})
				require.NoError(t, err)
			}
			workspacePath, err := localProjectPath(project.Slug)
			require.NoError(t, err)

			require.NoError(t, srv.cloneSharedWorkspaceProject(context.Background(), project))
			clones := localGateCloneInvocations(t, argvLog)
			require.Len(t, clones, 1)
			localGateAssertHardenedClone(t, clones[0], cloneURL, workspacePath, envLog)
			assert.True(t, util.IsGitRepoDir(workspacePath), "the workspace should be a git repo")
			assert.DirExists(t, filepath.Join(workspacePath, ".scion"))
		})
	}
}

// Entry point 1, project create: a shared-workspace git project whose clone
// URL is refused is a 422 with the fixed refusal text, runs no clone and
// leaves no project.
func TestLocalBackendClone_CreateProjectRefusesLocalSource(t *testing.T) {
	identityTestHome(t)
	srv, s := testServer(t)
	argvLog, _ := localGateFakeGit(t)
	source := localGateSourceRepo(t)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects", CreateProjectRequest{
		Name:          "Local Gate Create",
		GitRemote:     "github.com/test/local-gate-create",
		WorkspaceMode: "shared",
		Labels: map[string]string{
			store.LabelCloneURL:      source,
			store.LabelDefaultBranch: "master",
		},
	})
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), provision.ErrHostCloneURLRefused.Error())
	assert.NotContains(t, rec.Body.String(), source)
	assert.Empty(t, localGateCloneInvocations(t, argvLog))

	list, err := s.ListProjects(context.Background(), store.ProjectFilter{}, store.ListOptions{Limit: 1000})
	require.NoError(t, err)
	for _, p := range list.Items {
		assert.NotEqual(t, "Local Gate Create", p.Name, "a refused create must leave no project")
	}
}

// Entry point 1, project create: an https clone URL clones hardened.
func TestLocalBackendClone_CreateProjectClonesHardened(t *testing.T) {
	identityTestHome(t)
	srv, _ := testServer(t)
	argvLog, envLog := localGateFakeGit(t)
	const cloneURL = "https://git.example.invalid/test/local-gate-create-ok.git"

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects", CreateProjectRequest{
		Name:          "Local Gate Create OK",
		GitRemote:     "github.com/test/local-gate-create-ok",
		WorkspaceMode: "shared",
		Labels:        map[string]string{store.LabelCloneURL: cloneURL},
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var project store.Project
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&project))
	workspacePath, err := localProjectPath(project.Slug)
	require.NoError(t, err)

	clones := localGateCloneInvocations(t, argvLog)
	require.Len(t, clones, 1)
	localGateAssertHardenedClone(t, clones[0], cloneURL, workspacePath, envLog)
}

// localGateCloneSource stores a shared-workspace git project, owned by the
// dev user, to clone through POST /projects/{id}/clone.
func localGateCloneSource(t *testing.T, s store.Store, slug, cloneURL string) *store.Project {
	t.Helper()
	src := &store.Project{
		ID:        api.NewUUID(),
		Name:      slug,
		Slug:      slug,
		GitRemote: "https://github.com/test/" + slug + ".git",
		OwnerID:   DevUserID,
		CreatedBy: DevUserID,
		Labels: map[string]string{
			store.LabelWorkspaceMode: store.WorkspaceModeShared,
			store.LabelCloneURL:      cloneURL,
		},
	}
	require.NoError(t, s.CreateProject(context.Background(), src))
	return src
}

// Entry point 2, project clone: a source whose clone URL is refused makes
// the clone a 422 with the fixed refusal text; no clone runs and the new
// project is rolled back.
func TestLocalBackendClone_ProjectCloneRefusesLocalSource(t *testing.T) {
	identityTestHome(t)
	srv, s := testServer(t)
	argvLog, _ := localGateFakeGit(t)
	source := localGateSourceRepo(t)
	src := localGateCloneSource(t, s, "local-gate-clone-src", source)
	ctx := context.Background()

	before, err := s.ListProjects(ctx, store.ProjectFilter{}, store.ListOptions{Limit: 1000})
	require.NoError(t, err)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+src.ID+"/clone",
		map[string]string{"name": "Local Gate Clone"})
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), provision.ErrHostCloneURLRefused.Error())
	assert.NotContains(t, rec.Body.String(), source)
	assert.Empty(t, localGateCloneInvocations(t, argvLog))

	after, err := s.ListProjects(ctx, store.ProjectFilter{}, store.ListOptions{Limit: 1000})
	require.NoError(t, err)
	assert.Equal(t, len(before.Items), len(after.Items), "a refused clone must leave no project")
}

// Entry point 2, project clone: an https source clone URL clones hardened.
func TestLocalBackendClone_ProjectCloneClonesHardened(t *testing.T) {
	identityTestHome(t)
	srv, s := testServer(t)
	argvLog, envLog := localGateFakeGit(t)
	const cloneURL = "https://git.example.invalid/test/local-gate-clone-ok.git"
	src := localGateCloneSource(t, s, "local-gate-clone-ok-src", cloneURL)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+src.ID+"/clone",
		map[string]string{"name": "Local Gate Clone OK"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var clone store.Project
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&clone))
	require.True(t, clone.IsSharedWorkspace(), "the clone keeps the shared workspace mode")
	workspacePath, err := localProjectPath(clone.Slug)
	require.NoError(t, err)

	clones := localGateCloneInvocations(t, argvLog)
	require.Len(t, clones, 1)
	localGateAssertHardenedClone(t, clones[0], cloneURL, workspacePath, envLog)
}

// The local clone fixture exception holds only inside a test binary, only
// for the exact registered path, and only until the registering test ends.
func TestLocalCloneFixture_TestBinaryExactPathScoped(t *testing.T) {
	dir := t.TempDir()
	assert.False(t, localCloneFixtureAllowed(dir), "unregistered")
	t.Run("registered", func(t *testing.T) {
		allowLocalCloneFixture(t, dir)
		assert.True(t, localCloneFixtureAllowed(dir))
		assert.False(t, localCloneFixtureRegistered(false, dir), "outside a test binary")
		for _, other := range []string{dir + "/", dir + "/.", "file://" + dir, filepath.Dir(dir), dir + "x", ""} {
			assert.False(t, localCloneFixtureAllowed(other), "only the exact path: %q", other)
		}
	})
	assert.False(t, localCloneFixtureAllowed(dir), "removed when the registering test ends")
}

// localGateIsTestingTestingCall reports whether e is the call
// testing.Testing().
func localGateIsTestingTestingCall(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok || len(call.Args) != 0 {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Testing" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "testing"
}

// Production code cannot reach the fixture exception: in the non-test files
// of pkg/hub (and of every area moved out of it) only clone_fixture.go
// touches the registry, its only check passes testing.Testing(), the
// exception is consulted only by the local clone and the workspace pull, and
// nothing calls testing.Init.
func TestLocalCloneFixture_UnreachableFromProductionCode(t *testing.T) {
	// pkgmove:scan-covers guardScanSubdirs
	// scanned through guardScanSubdirs: pkg/hub and every area moved out of it.
	entries, err := readGuardScanDirs(".")
	require.NoError(t, err)
	fset := token.NewFileSet()
	registryUses := map[string]int{}
	allowedCalls := map[string]int{}
	registeredCalls := 0
	parsed := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		require.NoError(t, err, name)
		parsed++
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.Ident:
				if n.Name == "localCloneFixtures" || n.Name == "localCloneFixturesMu" {
					registryUses[name]++
				}
			case *ast.CallExpr:
				switch fn := n.Fun.(type) {
				case *ast.Ident:
					switch fn.Name {
					case "localCloneFixtureAllowed":
						allowedCalls[name]++
					case "localCloneFixtureRegistered":
						assert.Equal(t, "clone_fixture.go", name)
						assert.True(t, len(n.Args) == 2 && localGateIsTestingTestingCall(n.Args[0]),
							"%s: the registry check must be passed testing.Testing()", fset.Position(n.Pos()))
						registeredCalls++
					case "allowLocalCloneFixture":
						t.Errorf("%s: production code calls the test registration helper", fset.Position(n.Pos()))
					}
				case *ast.SelectorExpr:
					if x, ok := fn.X.(*ast.Ident); ok && x.Name == "testing" && fn.Sel.Name == "Init" {
						t.Errorf("%s: production code calls testing.Init", fset.Position(n.Pos()))
					}
				}
			}
			return true
		})
	}
	require.Greater(t, parsed, 50, "the scan must cover the package's production files")
	for file := range registryUses {
		assert.Equal(t, "clone_fixture.go", file, "only clone_fixture.go may touch the fixture registry")
	}
	assert.Equal(t, 1, registeredCalls, "one registry check, in localCloneFixtureAllowed")
	assert.Equal(t, map[string]int{"handlers_projects_core.go": 1, "project_workspace_handlers.go": 1}, allowedCalls,
		"the exception is consulted only by the local-backend clone and the workspace pull")
}
