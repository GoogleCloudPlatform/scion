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

package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Regression tests for ptone/scion#1355: ExecWithStdin must deliver its
// payload over the child process's stdin, never as a substring of the argv
// passed to the runtime's CLI. A fake docker/podman binary records both its
// argv and everything it reads from stdin, so the two channels are
// distinguishable in the assertion the way they are not distinguishable in
// /proc/<pid>/cmdline (argv) versus a pipe (stdin).

const stdinLeakSecret = "S3CR3T-1355-STDIN-NOT-ARGV"

// writeArgvStdinRecorder writes a fake CLI binary at path that appends its
// argv to argvFile and copies its stdin verbatim to stdinFile.
func writeArgvStdinRecorder(t *testing.T, path, argvFile, stdinFile string) {
	t.Helper()
	script := `#!/bin/sh
printf '%s\n' "$@" > "` + argvFile + `"
cat > "` + stdinFile + `"
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("failed to write fake binary: %v", err)
	}
}

func TestDockerRuntime_ExecWithStdin_SecretNotInArgv(t *testing.T) {
	tmpDir := t.TempDir()
	bin := filepath.Join(tmpDir, "mock-docker")
	argvFile := filepath.Join(tmpDir, "argv")
	stdinFile := filepath.Join(tmpDir, "stdin")
	writeArgvStdinRecorder(t, bin, argvFile, stdinFile)

	rt := &DockerRuntime{Command: bin}
	if _, err := rt.ExecWithStdin(context.Background(), "test-container", []string{"cat"}, strings.NewReader(stdinLeakSecret)); err != nil {
		t.Fatalf("ExecWithStdin failed: %v", err)
	}

	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("failed to read recorded argv: %v", err)
	}
	if strings.Contains(string(argv), stdinLeakSecret) {
		t.Errorf("secret leaked into docker exec argv: %q", string(argv))
	}
	if !strings.Contains(string(argv), "-i") {
		t.Errorf("expected -i (required for docker exec to attach stdin) in argv, got %q", string(argv))
	}

	stdin, err := os.ReadFile(stdinFile)
	if err != nil {
		t.Fatalf("failed to read recorded stdin: %v", err)
	}
	if string(stdin) != stdinLeakSecret {
		t.Errorf("secret not delivered via stdin verbatim: got %q, want %q", string(stdin), stdinLeakSecret)
	}
}

func TestPodmanRuntime_ExecWithStdin_SecretNotInArgv(t *testing.T) {
	tmpDir := t.TempDir()
	bin := filepath.Join(tmpDir, "mock-podman")
	argvFile := filepath.Join(tmpDir, "argv")
	stdinFile := filepath.Join(tmpDir, "stdin")
	writeArgvStdinRecorder(t, bin, argvFile, stdinFile)

	rt := &PodmanRuntime{Command: bin}
	if _, err := rt.ExecWithStdin(context.Background(), "test-container", []string{"cat"}, strings.NewReader(stdinLeakSecret)); err != nil {
		t.Fatalf("ExecWithStdin failed: %v", err)
	}

	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("failed to read recorded argv: %v", err)
	}
	if strings.Contains(string(argv), stdinLeakSecret) {
		t.Errorf("secret leaked into podman exec argv: %q", string(argv))
	}
	if !strings.Contains(string(argv), "-i") {
		t.Errorf("expected -i (required for podman exec to attach stdin) in argv, got %q", string(argv))
	}

	stdin, err := os.ReadFile(stdinFile)
	if err != nil {
		t.Fatalf("failed to read recorded stdin: %v", err)
	}
	if string(stdin) != stdinLeakSecret {
		t.Errorf("secret not delivered via stdin verbatim: got %q, want %q", string(stdin), stdinLeakSecret)
	}
}

func TestCloudRunSandboxRuntime_ExecWithStdin_SecretNotInArgv(t *testing.T) {
	tmpDir := t.TempDir()
	bin := filepath.Join(tmpDir, "mock-sandbox")
	argvFile := filepath.Join(tmpDir, "argv")
	stdinFile := filepath.Join(tmpDir, "stdin")
	writeArgvStdinRecorder(t, bin, argvFile, stdinFile)

	rt := &CloudRunSandboxRuntime{bin: bin}
	if _, err := rt.ExecWithStdin(context.Background(), "test-instance", []string{"cat"}, strings.NewReader(stdinLeakSecret)); err != nil {
		t.Fatalf("ExecWithStdin failed: %v", err)
	}

	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("failed to read recorded argv: %v", err)
	}
	if strings.Contains(string(argv), stdinLeakSecret) {
		t.Errorf("secret leaked into sandbox exec argv: %q", string(argv))
	}

	stdin, err := os.ReadFile(stdinFile)
	if err != nil {
		t.Fatalf("failed to read recorded stdin: %v", err)
	}
	if string(stdin) != stdinLeakSecret {
		t.Errorf("secret not delivered via stdin verbatim: got %q, want %q", string(stdin), stdinLeakSecret)
	}
}
