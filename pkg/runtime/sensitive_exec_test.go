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
	"bytes"
	"context"
	"errors"
	"log"
	"log/slog"
	"strings"
	"testing"
)

func TestIsSensitiveExec(t *testing.T) {
	if IsSensitiveExec(context.Background()) {
		t.Error("a plain context must not be reported as sensitive")
	}
	if !IsSensitiveExec(WithSensitiveExec(context.Background())) {
		t.Error("a context wrapped with WithSensitiveExec must be reported as sensitive")
	}
}

// TestWrapExecStreamError_SensitiveOmitsStderr covers the Kubernetes-backend
// source-level leak the agent-keys contract identifies (§5): a failed exec's
// stderr, which can carry caller-supplied terminal input for SendKeys, must
// never be embedded in the returned error when the call is marked sensitive
// — but non-sensitive callers keep the diagnostic stderr they rely on.
func TestWrapExecStreamError_SensitiveOmitsStderr(t *testing.T) {
	const secret = "K8S-EXEC-SENTINEL-do-not-leak"
	baseErr := errors.New("command terminated with non-zero exit code")

	plain := wrapExecStreamError(context.Background(), baseErr, secret)
	if !strings.Contains(plain.Error(), secret) {
		t.Errorf("non-sensitive call must still include stderr for diagnostics; got %q", plain.Error())
	}

	sensitive := wrapExecStreamError(WithSensitiveExec(context.Background()), baseErr, secret)
	if strings.Contains(sensitive.Error(), secret) {
		t.Errorf("SECURITY: sensitive call leaked stderr into the returned error: %q", sensitive.Error())
	}
	if !errors.Is(sensitive, baseErr) {
		t.Errorf("sensitive error must still wrap the underlying error, got %q", sensitive.Error())
	}
}

// TestRunSimpleCommand_SensitiveExec_SuppressesOutputInDebugLog is the
// log-capture test for pkg/runtime/common.go's identified leak: a failing
// command's CombinedOutput (which can carry SendKeys's injected content, or
// its stderr) must not reach the debug log when the call is marked via
// WithSensitiveExec, even though a non-sensitive failing command's output is
// still logged for ordinary diagnostics.
func TestRunSimpleCommand_SensitiveExec_SuppressesOutputInDebugLog(t *testing.T) {
	const secret = "RUNTIME-EXEC-SENTINEL-do-not-leak"

	var buf bytes.Buffer
	origWriter := log.Writer()
	origFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(origWriter)
		log.SetFlags(origFlags)
	})
	origLevel := slog.SetLogLoggerLevel(slog.LevelDebug)
	t.Cleanup(func() { slog.SetLogLoggerLevel(origLevel) })

	// A failing shell command that writes the secret to stdout before
	// exiting non-zero, standing in for a tmux failure whose combined
	// output would otherwise carry the injected keys.
	script := "echo " + secret + "; exit 1"

	// --- Non-sensitive: output is logged (existing behavior). ---
	buf.Reset()
	_, err := runSimpleCommand(context.Background(), "sh", "-c", script)
	if err == nil {
		t.Fatal("expected the script to fail")
	}
	if !strings.Contains(buf.String(), secret) {
		t.Errorf("non-sensitive failing command should log its output for diagnostics; log: %s", buf.String())
	}

	// --- Sensitive: output must be suppressed. ---
	buf.Reset()
	_, err = runSimpleCommand(WithSensitiveExec(context.Background()), "sh", "-c", script)
	if err == nil {
		t.Fatal("expected the script to fail")
	}
	logOutput := buf.String()
	if strings.Contains(logOutput, secret) {
		t.Errorf("SECURITY: sensitive-exec debug log contains the secret\nLog: %s", logOutput)
	}
	// Command name/duration must still be logged — only the output value
	// itself is suppressed.
	if !strings.Contains(logOutput, "sh") {
		t.Errorf("debug log should still contain the command name 'sh'\nLog: %s", logOutput)
	}
	if !strings.Contains(err.Error(), "sh failed") {
		t.Errorf("returned error should be unaffected by sensitive-exec suppression, got %q", err.Error())
	}
}

// TestRunSimpleCommandWithStdin_SensitiveExec_SuppressesOutputInDebugLog is
// runSimpleCommand's sibling test for the stdin-delivery path a keys call
// uses (SendKeys pipes its generated command on stdin rather than passing it
// as a process argument): a failing command's CombinedOutput must not reach
// the debug log when the call is marked sensitive, whether or not the
// content that produced the failure arrived via stdin.
func TestRunSimpleCommandWithStdin_SensitiveExec_SuppressesOutputInDebugLog(t *testing.T) {
	const secret = "RUNTIME-EXEC-STDIN-SENTINEL-do-not-leak"

	var buf bytes.Buffer
	origWriter := log.Writer()
	origFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(origWriter)
		log.SetFlags(origFlags)
	})
	origLevel := slog.SetLogLoggerLevel(slog.LevelDebug)
	t.Cleanup(func() { slog.SetLogLoggerLevel(origLevel) })

	// A command that echoes back whatever it reads on stdin, then exits
	// non-zero — standing in for a failed "tmux source-file -" call whose
	// combined output would otherwise carry the piped-in command (and the
	// keys payload octal-escaped inside it).
	stdin := strings.NewReader(secret)
	script := "cat; exit 1"

	// --- Non-sensitive: output is logged (existing behavior). ---
	buf.Reset()
	_, err := runSimpleCommandWithStdin(context.Background(), stdin, "sh", "-c", script)
	if err == nil {
		t.Fatal("expected the script to fail")
	}
	if !strings.Contains(buf.String(), secret) {
		t.Errorf("non-sensitive failing command should log its output for diagnostics; log: %s", buf.String())
	}

	// --- Sensitive: output must be suppressed. ---
	buf.Reset()
	stdin = strings.NewReader(secret)
	_, err = runSimpleCommandWithStdin(WithSensitiveExec(context.Background()), stdin, "sh", "-c", script)
	if err == nil {
		t.Fatal("expected the script to fail")
	}
	logOutput := buf.String()
	if strings.Contains(logOutput, secret) {
		t.Errorf("SECURITY: sensitive-exec debug log contains the secret piped via stdin\nLog: %s", logOutput)
	}
	if !strings.Contains(logOutput, "sh") {
		t.Errorf("debug log should still contain the command name 'sh'\nLog: %s", logOutput)
	}
	if !strings.Contains(err.Error(), "sh failed") {
		t.Errorf("returned error should be unaffected by sensitive-exec suppression, got %q", err.Error())
	}
}
