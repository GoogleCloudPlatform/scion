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
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

const serverSecretEnvHelperVar = "SCION_TEST_SERVER_SECRET_ENV_HELPER"

// isolateServerSecretEnv clears the flag, the captured value and every scrubbed
// name for the duration of a test, restoring them afterwards.
func isolateServerSecretEnv(t *testing.T) {
	t.Helper()
	oldFlag, oldCaptured := webSessionSecret, capturedSessionSecret
	webSessionSecret, capturedSessionSecret = "", ""
	t.Cleanup(func() { webSessionSecret, capturedSessionSecret = oldFlag, oldCaptured })
	for _, name := range serverSecretEnvNames {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
}

func captureSecretEnvSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &buf
}

func TestScrubServerSecretEnv_UnsetsNamesAndKeepsSecret(t *testing.T) {
	isolateServerSecretEnv(t)
	for _, name := range serverSecretEnvNames {
		t.Setenv(name, "value-of-"+name)
	}

	scrubServerSecretEnv()

	for _, name := range serverSecretEnvNames {
		if _, ok := os.LookupEnv(name); ok {
			t.Errorf("%s is still set in the process env after the scrub", name)
		}
	}
	if got := resolveSessionSecret(); got != "value-of-"+sessionSecretEnv {
		t.Errorf("resolveSessionSecret() after the scrub returned a value of length %d, want the prefixed value", len(got))
	}
}

func TestScrubServerSecretEnv_FlagStillWins(t *testing.T) {
	isolateServerSecretEnv(t)
	t.Setenv(sessionSecretEnv, "from-env")
	scrubServerSecretEnv()
	webSessionSecret = "from-flag"
	if got := resolveSessionSecret(); got != "from-flag" {
		t.Errorf("resolveSessionSecret() = %q, want the flag value", got)
	}
}

func TestScrubServerSecretEnv_LegacySessionSecretStillStartsHubAndWarns(t *testing.T) {
	isolateServerSecretEnv(t)
	logs := captureSecretEnvSlog(t)
	t.Setenv(legacySessionSecretEnv, "legacy-secret-value")

	scrubServerSecretEnv()

	if _, ok := os.LookupEnv(legacySessionSecretEnv); ok {
		t.Error("SESSION_SECRET is still set in the process env after the scrub")
	}
	hubCfg := buildHubServerConfig(&config.GlobalConfig{}, "https://hub.example.com", "", nil, false, "", nil)
	if hubCfg.SharedSigningSecret != "legacy-secret-value" {
		t.Errorf("hub SharedSigningSecret has length %d, want the SESSION_SECRET value", len(hubCfg.SharedSigningSecret))
	}
	out := logs.String()
	if !strings.Contains(out, "SESSION_SECRET is deprecated") || !strings.Contains(out, sessionSecretEnv) {
		t.Errorf("no deprecation warning naming %s; logs: %s", sessionSecretEnv, out)
	}
	if strings.Contains(out, "legacy-secret-value") {
		t.Error("the deprecation warning logged the secret value")
	}
}

func TestScrubServerSecretEnv_PrefixedNameWinsWithoutWarning(t *testing.T) {
	isolateServerSecretEnv(t)
	logs := captureSecretEnvSlog(t)
	t.Setenv(sessionSecretEnv, "prefixed")
	t.Setenv(legacySessionSecretEnv, "legacy")

	scrubServerSecretEnv()

	if got := resolveSessionSecret(); got != "prefixed" {
		t.Errorf("resolveSessionSecret() = %q, want the prefixed value", got)
	}
	if strings.Contains(logs.String(), "deprecated") {
		t.Errorf("deprecation warning logged although %s is set", sessionSecretEnv)
	}
}

// TestScrubServerSecretEnv_ChildProcessEnvIsClean starts a child process after
// the scrub and checks that it inherits none of the scrubbed names.
func TestScrubServerSecretEnv_ChildProcessEnvIsClean(t *testing.T) {
	isolateServerSecretEnv(t)
	for _, name := range serverSecretEnvNames {
		t.Setenv(name, "value-of-"+name)
	}

	scrubServerSecretEnv()

	child := exec.Command(os.Args[0], "-test.run=^TestServerSecretEnvHelperProcess$")
	child.Env = append(os.Environ(), serverSecretEnvHelperVar+"=1")
	out, err := child.CombinedOutput()
	if err != nil {
		t.Fatalf("helper process failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "HELPER-RAN") {
		t.Fatalf("helper process did not run; output:\n%s", out)
	}
	for _, name := range serverSecretEnvNames {
		if strings.Contains(string(out), "PRESENT "+name+"\n") {
			t.Errorf("child process inherited %s", name)
		}
	}
}

// TestServerSecretEnvHelperProcess is the child side of
// TestScrubServerSecretEnv_ChildProcessEnvIsClean; it does nothing unless
// started by it.
func TestServerSecretEnvHelperProcess(t *testing.T) {
	if os.Getenv(serverSecretEnvHelperVar) != "1" {
		return
	}
	for _, name := range serverSecretEnvNames {
		if _, ok := os.LookupEnv(name); ok {
			fmt.Printf("PRESENT %s\n", name)
		}
	}
	fmt.Println("HELPER-RAN")
}

// TestScrubServerSecretEnv_SettingsReloadKeepsSecret re-runs, after the scrub,
// the env-reading settings loaders the server calls later (settings reload,
// settings GET, versioned settings) and checks the in-memory secret and the
// hub signing secret built from it are unchanged.
func TestScrubServerSecretEnv_SettingsReloadKeepsSecret(t *testing.T) {
	isolateServerSecretEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(sessionSecretEnv, "durable-signing-secret")

	scrubServerSecretEnv()

	_, _ = config.LoadGlobalConfig("")
	_ = config.LoadEnvKoanf()
	_, _ = config.LoadVersionedSettings(home)

	if got := resolveSessionSecret(); got != "durable-signing-secret" {
		t.Errorf("resolveSessionSecret() after a settings reload has length %d, want the captured value", len(got))
	}
	hubCfg := buildHubServerConfig(&config.GlobalConfig{}, "https://hub.example.com", "", nil, false, "", nil)
	if hubCfg.SharedSigningSecret != "durable-signing-secret" {
		t.Errorf("hub SharedSigningSecret after a settings reload has length %d, want the captured value", len(hubCfg.SharedSigningSecret))
	}
}

// TestRunServerStart_ScrubsServerSecretEnvBeforeChildProcesses checks the
// call order in runServerStart: the scrub runs right after the config is
// loaded and before every step that can start a child process or reads the
// session secret.
func TestRunServerStart_ScrubsServerSecretEnvBeforeChildProcesses(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "server_foreground.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var body *ast.BlockStmt
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "runServerStart" {
			body = fn.Body
		}
	}
	if body == nil {
		t.Fatal("runServerStart not found")
	}

	first := map[string]token.Pos{}
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		var name string
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			name = fn.Name
		case *ast.SelectorExpr:
			if pkg, ok := fn.X.(*ast.Ident); ok {
				name = pkg.Name + "." + fn.Sel.Name
			}
		}
		if _, seen := first[name]; name != "" && !seen {
			first[name] = call.Pos()
		}
		return true
	})

	scrub, ok := first["scrubServerSecretEnv"]
	if !ok {
		t.Fatal("runServerStart does not call scrubServerSecretEnv")
	}
	if load, ok := first["loadAndReconcileConfig"]; !ok || load > scrub {
		t.Error("scrubServerSecretEnv must run after loadAndReconcileConfig")
	}
	for _, later := range []string{
		"config.InitGlobal",
		"validateHostedBasic",
		"validateHostedHAPreflight",
		"secret.NewBackend",
		"initPluginManager",
		"initHubServerFn",
		"startConduit",
		"initWebServer",
		"startRuntimeBroker",
	} {
		pos, ok := first[later]
		if !ok {
			t.Errorf("runServerStart no longer calls %s; update this test", later)
			continue
		}
		if pos < scrub {
			t.Errorf("%s runs before scrubServerSecretEnv", later)
		}
	}
}
