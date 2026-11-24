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
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/internal/writesonread"
	"github.com/GoogleCloudPlatform/scion/pkg/k8s"
)

// TestMain disables the UTC pin for the whole cmd test binary.
//
// Production run functions pin time.Local to UTC; in the shared test binary
// that would race leaked goroutines (other tests' background work reads
// time.Local concurrently) and would silently turn every later TZ=... test
// in this binary into a UTC test, masking real timezone bugs. pkg/util's
// TestPinProcessUTC keeps covering PinProcessUTC itself, and
// pin_process_utc_test.go's AST test covers where the seam is called from.
func TestMain(m *testing.M) {
	pinProcessUTC = func() {}
	clearAmbientScionEnv()
	cleanupTestEnv := isolateTestHomeAndTemp()
	// The CLI imports the global settings' hub.endpoint (and a linked
	// project's hub.endpoint) into ~/.scion/context.yaml the first time it
	// reads the context, and that import writes the file. Most cmd tests
	// run the root command without an isolated HOME, so with the import on
	// they would write the real user's context.yaml, and tests that write
	// a hub.endpoint would change behaviour. Tests of the imports turn them
	// back on with a temp HOME (enableGlobalHubImport). This is the only
	// place they are turned off.
	importGlobalHubSettings = false
	// A flat Kubernetes instance with no kubeconfig builds its client
	// from the default loading rules only: no test uses the in-cluster
	// config of the pod it runs in (ptone/scion#5088). This is the only
	// place it is set.
	productionFlatDefaultKubernetesClient = flatDefaultKubernetesClient
	flatDefaultKubernetesClient = k8s.NewClientWithContextNoInCluster
	teardown := integrationTestSetup()
	code := m.Run()
	teardown()
	cleanupTestEnv()
	// The no-write switch (internal/writesonread) is process-wide: a test
	// that sets it without runPreflightForTest, whose isolation turns it
	// off again, would leave later tests running without the migrations.
	if writesonread.Disabled() {
		fmt.Fprintln(os.Stderr, "a cmd test left the no-write switch on; set it only through runPreflightForTest")
		code = 1
	}
	os.Exit(code)
}

// productionFlatDefaultKubernetesClient is flatDefaultKubernetesClient's
// value before TestMain replaces it, so a test can pin the production value.
var productionFlatDefaultKubernetesClient func(kubeconfigPath, contextName string) (*k8s.Client, error)

// integrationTestSetup runs before the cmd tests and returns their teardown.
// It does nothing by default; main_integration_test.go replaces it under
// -tags integration to provision the enttest PostgreSQL database.
var integrationTestSetup = func() (teardown func()) { return func() {} }

// clearAmbientScionEnv unsets every SCION_* variable inherited from the
// environment before any cmd test runs. Inside an agent container these
// point at the live hub (SCION_HUB_ENDPOINT, SCION_AUTH_TOKEN, ...) and take
// precedence over the mock servers and temp projects the tests set up, so
// tests would call the real hub and fail with 401s or wrong request counts
// (ptone/scion#2102). Tests that need a value set it themselves with
// t.Setenv. SCION_TEST_* variables are deliberate test opt-ins (for example
// SCION_TEST_BASH) and are kept.
func clearAmbientScionEnv() {
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "SCION_") && !strings.HasPrefix(name, "SCION_TEST_") {
			if err := os.Unsetenv(name); err != nil {
				panic("clearAmbientScionEnv: unset " + name + ": " + err.Error())
			}
		}
	}
}

// isolateTestHomeAndTemp isolates the cmd test binary from the host's home
// directory, XDG config/state/data/cache directories, and any TMPDIR located
// under a directory containing (or capable of containing) a .scion project
// (ptone/scion#9118).
//
// In environments where GOTMPDIR or TMPDIR sits inside the real HOME (for
// example GOTMPDIR=/home/runner/.cr-ci/go-tmp on the self-hosted CI runner),
// go test passes TMPDIR=$GOTMPDIR to the test binary. Any test that runs
// t.Setenv("HOME", t.TempDir()) and t.Chdir(t.TempDir()) then executes inside
// a subdirectory of the real HOME; when config.FindProjectRoot walks upward,
// it discovers $REAL_HOME/.scion and treats the real HOME as a directory
// project. Relocating TMPDIR to a clean /tmp root when os.TempDir() is under
// the original HOME or has a .scion ancestor, and pointing HOME/USERPROFILE at
// a sibling temp directory while clearing ambient XDG_* overrides, keeps every
// cmd test isolated regardless of ambient ~/.scion state.
func isolateTestHomeAndTemp() func() {
	// Child CLI helper processes (runSSHCLI) receive an explicit minimal env
	// including their own isolated HOME; keep it untouched.
	if os.Getenv("SCION_TEST_EXEC_CLI") == "1" {
		return func() {}
	}

	var cleanups []string
	cleanup := func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			_ = os.RemoveAll(cleanups[i])
		}
	}

	origHome, _ := os.UserHomeDir()
	origTmp := os.TempDir()
	origGoTmp := os.Getenv("GOTMPDIR")

	// Preserve Go's module and build caches before overriding HOME so tests
	// that run go/packages or `go build` do not redownload modules into
	// testHome.
	if origHome != "" {
		if os.Getenv("GOPATH") == "" {
			if err := os.Setenv("GOPATH", filepath.Join(origHome, "go")); err != nil {
				cleanup()
				panic("isolateTestHomeAndTemp: set GOPATH: " + err.Error())
			}
		}
		if os.Getenv("GOCACHE") == "" {
			var cacheDir string
			switch {
			case os.Getenv("XDG_CACHE_HOME") != "":
				cacheDir = filepath.Join(os.Getenv("XDG_CACHE_HOME"), "go-build")
			case runtime.GOOS == "darwin":
				cacheDir = filepath.Join(origHome, "Library", "Caches", "go-build")
			case runtime.GOOS == "windows":
				if localAppData := os.Getenv("LOCALAPPDATA"); localAppData != "" {
					cacheDir = filepath.Join(localAppData, "go-build")
				}
			default:
				cacheDir = filepath.Join(origHome, ".cache", "go-build")
			}
			if cacheDir != "" {
				if err := os.Setenv("GOCACHE", cacheDir); err != nil {
					cleanup()
					panic("isolateTestHomeAndTemp: set GOCACHE: " + err.Error())
				}
			}
		}
	}

	// Go 1.26's t.TempDir() prefers GOTMPDIR over TMPDIR/os.TempDir(). If
	// either points inside the original HOME or beneath any .scion directory
	// (e.g. GOTMPDIR=/home/runner/.cr-ci/go-tmp on the self-hosted runner),
	// relocate both to a clean directory under /tmp so t.Chdir(t.TempDir())
	// never walks up into $REAL_HOME/.scion.
	needsCleanTmp := func(dir string) bool {
		return dir != "" && (hasScionAncestor(dir) || isUnderDir(dir, origHome))
	}
	if runtime.GOOS != "windows" && (needsCleanTmp(origTmp) || needsCleanTmp(origGoTmp)) {
		if info, err := os.Stat("/tmp"); err == nil && info.IsDir() && !hasScionAncestor("/tmp") {
			cleanTmp, err := os.MkdirTemp("/tmp", "scion-cmd-test-tmp-*")
			if err != nil {
				panic("isolateTestHomeAndTemp: mkdir clean tmp: " + err.Error())
			}
			cleanups = append(cleanups, cleanTmp)
			if err := os.Setenv("TMPDIR", cleanTmp); err != nil {
				cleanup()
				panic("isolateTestHomeAndTemp: set TMPDIR: " + err.Error())
			}
			if origGoTmp != "" {
				if err := os.Setenv("GOTMPDIR", cleanTmp); err != nil {
					cleanup()
					panic("isolateTestHomeAndTemp: set GOTMPDIR: " + err.Error())
				}
			}
		}
	}

	testHome, err := os.MkdirTemp("", "scion-cmd-test-home-*")
	if err != nil {
		cleanup()
		panic("isolateTestHomeAndTemp: mkdir test home: " + err.Error())
	}
	cleanups = append(cleanups, testHome)

	if err := os.Setenv("HOME", testHome); err != nil {
		cleanup()
		panic("isolateTestHomeAndTemp: set HOME: " + err.Error())
	}
	if runtime.GOOS == "windows" {
		if err := os.Setenv("USERPROFILE", testHome); err != nil {
			cleanup()
			panic("isolateTestHomeAndTemp: set USERPROFILE: " + err.Error())
		}
	}
	for _, xdgVar := range []string{
		"XDG_CONFIG_HOME",
		"XDG_STATE_HOME",
		"XDG_DATA_HOME",
		"XDG_CACHE_HOME",
	} {
		if err := os.Unsetenv(xdgVar); err != nil {
			cleanup()
			panic("isolateTestHomeAndTemp: unset " + xdgVar + ": " + err.Error())
		}
	}

	return cleanup
}

// hasScionAncestor reports whether dir or any of its ancestors contains a
// .scion entry.
func hasScionAncestor(dir string) bool {
	if dir == "" {
		return false
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return false
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	cur := filepath.Clean(abs)
	for {
		if _, err := os.Stat(filepath.Join(cur, ".scion")); err == nil {
			return true
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return false
		}
		cur = parent
	}
}

// isUnderDir reports whether path is equal to root or nested beneath it.
func isUnderDir(path, root string) bool {
	if path == "" || root == "" {
		return false
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	if resolved, err := filepath.EvalSymlinks(absPath); err == nil {
		absPath = resolved
	}
	if resolved, err := filepath.EvalSymlinks(absRoot); err == nil {
		absRoot = resolved
	}
	absPath = filepath.Clean(absPath)
	absRoot = filepath.Clean(absRoot)
	if absPath == absRoot {
		return true
	}
	return strings.HasPrefix(absPath, absRoot+string(filepath.Separator))
}

func TestMain_IsolatesHomeAndTempFromScionAncestor(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("os.UserHomeDir: %v", err)
	}
	if hasScionAncestor(filepath.Dir(home)) {
		t.Fatalf("isolated test HOME %q has a .scion ancestor", home)
	}
	tmp := t.TempDir()
	if hasScionAncestor(tmp) {
		t.Fatalf("isolated t.TempDir() %q has a .scion ancestor", tmp)
	}
	if isUnderDir(tmp, home) {
		t.Fatalf("t.TempDir() %q must not be nested under test HOME %q", tmp, home)
	}
	for _, xdgVar := range []string{
		"XDG_CONFIG_HOME",
		"XDG_STATE_HOME",
		"XDG_DATA_HOME",
		"XDG_CACHE_HOME",
	} {
		if val, ok := os.LookupEnv(xdgVar); ok {
			t.Fatalf("expected %s to be unset by TestMain, got %q", xdgVar, val)
		}
	}
}

func TestHasScionAncestorAndIsUnderDir(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "runner")
	nested := filepath.Join(home, ".cr-ci", "go-tmp", "work")
	sibling := filepath.Join(base, "runner2", "tmp")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}

	if !isUnderDir(nested, home) {
		t.Fatalf("expected %q to be under %q", nested, home)
	}
	if !isUnderDir(home, home) {
		t.Fatalf("expected %q to be under itself", home)
	}
	if isUnderDir(sibling, home) {
		t.Fatalf("expected sibling %q not to be under %q", sibling, home)
	}
	if hasScionAncestor(nested) {
		t.Fatalf("expected no .scion ancestor under %q yet", nested)
	}

	if err := os.MkdirAll(filepath.Join(home, ".scion"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !hasScionAncestor(nested) {
		t.Fatalf("expected .scion ancestor to be detected for %q", nested)
	}
	if hasScionAncestor(sibling) {
		t.Fatalf("expected sibling %q not to see %q/.scion", sibling, home)
	}
}
