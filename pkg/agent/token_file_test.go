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
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// Tests for ptone/scion#1348: a failed write of the agent token file must abort
// Start() with an error naming the failed operation, and must NOT fall back to
// leaving the credential in opts.Env.
//
// Before the fix, all three failure branches logged with util.Debugf and fell
// through to the same unconditional delete as the success branch, so the agent
// started with the credential in neither the file nor the environment and
// NewClient() returned nil -- silently dead on arrival.

// Never a real credential in a test. This value is also asserted to be absent
// from every error message the failure paths produce.
const tokenFileSentinel = "FAKE-AUTH-SENTINEL-not-a-real-credential"

// plantedFailure describes one way to make the token write fail, expressed as
// something planted on disk rather than as a permission change.
//
// WHY NOT A READ-ONLY DIRECTORY, which is the obvious way to force these. A
// chmod-based fixture is not deterministic across the environments this suite
// runs in: root ignores the mode bits on a 0500 directory, so the write would
// succeed and the test would pass while asserting nothing. Occupying the target
// path with a directory where a regular file is required produces EISDIR/EEXIST
// for every uid, including root.
type plantedFailure struct {
	name string
	// plant prepares scionDir so the next token write fails. scionDir is
	// <agentHome>/.scion.
	plant func(t *testing.T, scionDir string)
	// wantOp is the operation name the error must carry. It is what makes the
	// three failure modes distinguishable from each other in an operator's log,
	// and it is also what proves the test reached the token-write site rather
	// than failing somewhere earlier in Start().
	wantOp string
}

// plantsReachableFromStart are the failure modes that can be driven all the way
// through Start().
//
// The MkdirAll branch is deliberately NOT here, and its absence is a
// measurement rather than an oversight. Making <agentHome>/.scion a regular
// file does fail MkdirAll -- see TestWriteAgentTokenFile_FailurePaths, which
// covers it directly -- but Start() never gets that far: harness provisioning
// and WriteProjectPreStartHook (run.go:450) both use <agentHome>/.scion first,
// and Start aborts with
//
//	re-stage project pre-start hook: remove stale pre-start hook:
//	.../.scion/hooks/pre-start.d/30-project-custom: not a directory
//
// A Start()-level case for that branch would therefore be a vacuous red: it
// would go red before the fix and after it, for a reason that has nothing to do
// with the token file. The MkdirAll branch remains reachable in production
// (ENOSPC, a read-only remount, a concurrent removal between :450 and :761), so
// it is defended and unit-tested, but it is not claimed to be covered here.
var plantsReachableFromStart = []plantedFailure{
	{
		name: "temp file path is occupied by a directory",
		plant: func(t *testing.T, scionDir string) {
			t.Helper()
			mkdirAll(t, filepath.Join(scionDir, "scion-token.tmp"))
		},
		wantOp: "write agent token file",
	},
	{
		name: "token file path is occupied by a directory",
		plant: func(t *testing.T, scionDir string) {
			t.Helper()
			mkdirAll(t, filepath.Join(scionDir, "scion-token"))
		},
		wantOp: "install agent token file",
	},
}

func mkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0755); err != nil {
		t.Fatal(err)
	}
}

// TestWriteAgentTokenFile_FailurePaths covers all three failure branches
// directly, including the one Start() cannot reach.
func TestWriteAgentTokenFile_FailurePaths(t *testing.T) {
	all := append([]plantedFailure{{
		name: "the .scion directory path is occupied by a regular file",
		plant: func(t *testing.T, scionDir string) {
			t.Helper()
			if err := os.WriteFile(scionDir, []byte("not a directory"), 0600); err != nil {
				t.Fatal(err)
			}
		},
		wantOp: "create agent token directory",
	}}, plantsReachableFromStart...)

	for _, tc := range all {
		t.Run(tc.name, func(t *testing.T) {
			agentHome := t.TempDir()
			scionDir := filepath.Join(agentHome, ".scion")
			if tc.wantOp != "create agent token directory" {
				mkdirAll(t, scionDir)
			}
			tc.plant(t, scionDir)

			err := writeAgentTokenFile(agentHome, tokenFileSentinel)
			if err == nil {
				t.Fatal("writeAgentTokenFile returned nil; the fixture did not force a failure " +
					"and the test asserts nothing")
			}
			assertUsefulTokenWriteError(t, err, tc.wantOp, scionDir)

			// The temp file holds the credential in plaintext. On any failure
			// after the file may have been created, it must not be left behind.
			if _, statErr := os.Stat(filepath.Join(scionDir, "scion-token.tmp")); statErr == nil {
				if tc.wantOp == "write agent token file" || tc.wantOp == "install agent token file" {
					t.Error("temp token file survived the failed " + tc.wantOp + "; a plaintext credential " +
						"was left in the agent home")
				}
			}
		})
	}
}

func TestWriteAgentTokenFile_Success(t *testing.T) {
	agentHome := t.TempDir()
	if err := writeAgentTokenFile(agentHome, tokenFileSentinel); err != nil {
		t.Fatalf("writeAgentTokenFile: %v", err)
	}

	tokenPath := filepath.Join(agentHome, ".scion", "scion-token")
	data, err := os.ReadFile(tokenPath)
	if err != nil {
		t.Fatalf("read token file: %v", err)
	}
	if string(data) != tokenFileSentinel {
		t.Error("token file contents do not match the token that was written")
	}

	fi, err := os.Stat(tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0600 {
		t.Errorf("token file mode = %#o, want 0600", perm)
	}
	if _, err := os.Stat(tokenPath + ".tmp"); err == nil {
		t.Error("temp token file was not consumed by the rename")
	}
}

// assertUsefulTokenWriteError encodes requirement 2 of ptone/scion#1348: the
// error names the operation and the path and wraps the underlying os error.
// "failed to prepare agent credentials" would satisfy none of these.
func assertUsefulTokenWriteError(t *testing.T, err error, wantOp, scionDir string) {
	t.Helper()

	got := err.Error()
	if !strings.Contains(got, wantOp) {
		t.Errorf("error does not name the operation that failed.\n  want substring: %q\n  error: %v",
			wantOp, err)
	}
	if !strings.Contains(got, scionDir) {
		t.Errorf("error does not name the path it failed on.\n  want substring: %q\n  error: %v",
			scionDir, err)
	}

	// Requirement 3: the underlying os error must survive, so callers can tell
	// ENOSPC from EACCES from EISDIR without parsing the message. *fs.PathError
	// covers MkdirAll and WriteFile; *os.LinkError covers Rename.
	var pathErr *fs.PathError
	var linkErr *os.LinkError
	if !errors.As(err, &pathErr) && !errors.As(err, &linkErr) {
		t.Errorf("error does not wrap the underlying os error: %v", err)
	}

	if strings.Contains(got, tokenFileSentinel) {
		t.Errorf("the error message contains the credential value it failed to write")
	}
}

// TestStartRefusesToStartWhenTokenFileWriteFails is the end-to-end half: the
// error reaches Start()'s caller, no container is launched, and -- the part
// that matters most -- the credential does not survive in opts.Env.
func TestStartRefusesToStartWhenTokenFileWriteFails(t *testing.T) {
	for _, tc := range plantsReachableFromStart {
		t.Run(tc.name, func(t *testing.T) {
			fx := newTokenFileFixture(t)

			// The agent home must already exist so the plant survives: GetAgent
			// deletes an agent directory that has no scion-agent.json, treating
			// it as a stale half-provisioned leftover (provision.go:1747).
			mkdirAll(t, fx.scionDir)
			writeFile(t, filepath.Join(fx.agentDir, "scion-agent.json"),
				`{"harness_config": "test-harness", "image": "test-image:latest"}`)
			tc.plant(t, fx.scionDir)

			_, err := fx.start()
			if err == nil {
				t.Fatal("Start() succeeded even though the token file could not be written; " +
					"the agent would be running with no credential in either place")
			}
			assertUsefulTokenWriteError(t, err, tc.wantOp, fx.scionDir)

			if fx.runCalled {
				t.Error("the runtime was invoked despite the credential write failing; " +
					"Start() must refuse to launch, not launch a dead agent")
			}

			// THE ARGV ASSERTION. NOT REDUNDANT WITH THE ERROR ASSERTION ABOVE,
			// AND NOT SAFE TO DELETE AS A DUPLICATE.
			//
			// The two assertions fail under opposite mistakes. The error
			// assertion goes red if a future author makes this failure quiet
			// again. THIS one goes red if a future author makes it loud but
			// "helpful" -- fixing the availability problem by leaving the token
			// in opts.Env as a fallback, which is the first fix anyone reaches
			// for. That fallback looks harmless and is not: opts.Env is
			// serialised into the runtime argv as --env KEY=VALUE, and that
			// argv is written to Cloud Logging on every start (ptone/scion#127,
			// open P0). It would convert a rare, loud availability failure into
			// a silent credential disclosure on a path nobody watches.
			//
			// opts.Env is asserted rather than the argv itself because on this
			// path there IS no argv -- Start() aborts before building one, as
			// the runCalled check above proves. opts.Env is the map the argv is
			// built from, and it is the caller's map: the broker still holds it
			// after Start returns an error. Removing the key here is the
			// property that makes an argv impossible to build with it.
			if v, present := fx.env["SCION_AUTH_TOKEN"]; present {
				t.Errorf("SCION_AUTH_TOKEN survived in opts.Env after Start() failed "+
					"(value length %d); it would reach the runtime argv, and the argv "+
					"reaches Cloud Logging -- see ptone/scion#127", len(v))
			}
		})
	}
}

// TestStartWritesTokenFileAndStripsItFromEnv is the success path of the same
// block: the token still leaves opts.Env, and it still reaches the file.
func TestStartWritesTokenFileAndStripsItFromEnv(t *testing.T) {
	fx := newTokenFileFixture(t)

	if _, err := fx.start(); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}

	if _, present := fx.env["SCION_AUTH_TOKEN"]; present {
		t.Error("SCION_AUTH_TOKEN was not removed from opts.Env on the success path")
	}
	for _, e := range fx.captured.Env {
		if strings.HasPrefix(e, "SCION_AUTH_TOKEN=") {
			t.Error("SCION_AUTH_TOKEN reached the container env; it belongs in the token file only")
		}
	}

	data, err := os.ReadFile(filepath.Join(fx.captured.HomeDir, ".scion", "scion-token"))
	if err != nil {
		t.Fatalf("read token file: %v", err)
	}
	if string(data) != tokenFileSentinel {
		t.Error("token file contents do not match the token that was supplied")
	}
}

// tokenFileFixture is a minimal on-disk Scion installation plus a mock runtime,
// wired so Start() reaches the token-write block at run.go:761 with a
// broker-supplied SCION_AUTH_TOKEN.
type tokenFileFixture struct {
	t         *testing.T
	agentDir  string
	scionDir  string
	env       map[string]string
	mgr       Manager
	opts      api.StartOptions
	captured  runtime.RunConfig
	runCalled bool
}

func newTokenFileFixture(t *testing.T) *tokenFileFixture {
	t.Helper()

	tmpDir := t.TempDir()
	oldWd, _ := os.Getwd()
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWd) })

	t.Setenv("HOME", tmpDir)
	// Keep host state out of the fixture: a real dev token or hub endpoint in
	// the environment would change which branch supplies SCION_AUTH_TOKEN.
	for _, k := range []string{"SCION_DEV_TOKEN", "SCION_AUTH_TOKEN", "SCION_DEV_TOKEN_FILE",
		"SCION_HUB_ENDPOINT", "SCION_HUB_URL"} {
		t.Setenv(k, "")
		if err := os.Unsetenv(k); err != nil {
			t.Fatal(err)
		}
	}

	globalScionDir := filepath.Join(tmpDir, ".scion")
	hcDir := filepath.Join(globalScionDir, "harness-configs", "test-harness")
	mkdirAll(t, hcDir)
	writeFile(t, filepath.Join(hcDir, "config.yaml"),
		"harness: gemini\nuser: scion\nimage: test-image:latest\n")

	tplDir := filepath.Join(globalScionDir, "templates", "default")
	mkdirAll(t, tplDir)
	writeFile(t, filepath.Join(tplDir, "scion-agent.json"),
		`{"default_harness_config": "test-harness"}`)

	writeFile(t, filepath.Join(globalScionDir, "settings.yaml"),
		"schema_version: \"1\"\nactive_profile: local\nprofiles:\n  local:\n    runtime: docker\n")

	projectScionDir := filepath.Join(tmpDir, "project", ".scion")
	mkdirAll(t, projectScionDir)
	writeFile(t, filepath.Join(projectScionDir, "settings.yaml"),
		"hub:\n  enabled: true\n  endpoint: \"http://localhost:9810\"\n")

	fx := &tokenFileFixture{t: t}
	fx.agentDir = filepath.Join(projectScionDir, "agents", "test-agent")
	fx.scionDir = filepath.Join(fx.agentDir, "home", ".scion")

	// Supplied the way the broker supplies it (start_context.go:240), rather
	// than resolved from a dev-token file, so the test owns the map and can
	// inspect it after Start returns.
	fx.env = map[string]string{
		"SCION_HUB_ENDPOINT": "http://localhost:9810",
		"SCION_AUTH_TOKEN":   tokenFileSentinel,
	}

	fx.mgr = NewManager(&runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, cfg runtime.RunConfig) (string, error) {
			fx.captured = cfg
			fx.runCalled = true
			return "mock-id", nil
		},
	})
	fx.opts = api.StartOptions{
		Name:        "test-agent",
		ProjectPath: projectScionDir,
		Env:         fx.env,
		NoAuth:      true,
	}
	return fx
}

func (fx *tokenFileFixture) start() (*api.AgentInfo, error) {
	fx.t.Helper()
	return fx.mgr.Start(context.Background(), fx.opts)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}
