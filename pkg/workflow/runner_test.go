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

package workflow_test

import (
	"context"
	"errors"
	"os/exec"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/workflow"
)

// TestValidateInputFlag exercises the happy and error paths for ValidateInputFlag.
func TestValidateInputFlag(t *testing.T) {
	t.Helper()

	valid := []string{
		"foo=bar",
		"foo=",
		`foo={"a":1}`,
		"foo=value with spaces",
		"FOO_BAR=x",
		"_private=1",
	}
	for _, s := range valid {
		if err := workflow.ValidateInputFlag(s); err != nil {
			t.Errorf("ValidateInputFlag(%q) = %v, want nil", s, err)
		}
	}

	invalid := []struct {
		input string
		desc  string
	}{
		{"foo", "missing '='"},
		{"=bar", "empty key"},
		{"", "empty string"},
		{"1foo=bar", "key starts with digit"},
		{"foo-bar=baz", "key contains hyphen"},
		{"foo bar=baz", "key contains space"},
	}
	for _, tc := range invalid {
		if err := workflow.ValidateInputFlag(tc.input); err == nil {
			t.Errorf("ValidateInputFlag(%q) = nil, want error (%s)", tc.input, tc.desc)
		}
	}
}

// TestRunLocal_QuackNotFound verifies that RunLocal returns an error wrapping
// ErrQuackNotFound when quack is not on PATH.
func TestRunLocal_QuackNotFound(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	res, err := workflow.RunLocal(context.Background(), workflow.RunLocalRequest{
		File: "testdata/hello.duck.yaml",
	})

	if err == nil {
		t.Fatalf("RunLocal() = (%v, nil), want error", res)
	}
	if !errors.Is(err, workflow.ErrQuackNotFound) {
		t.Errorf("RunLocal() error = %v, want to wrap ErrQuackNotFound", err)
	}
}

// TestRunLocal_Success runs the hello fixture through quack when it is available.
// The test is skipped if quack is not installed on the host.
func TestRunLocal_Success(t *testing.T) {
	if _, err := exec.LookPath("quack"); err != nil {
		t.Skip("quack not found in PATH; skipping integration test")
	}

	res, err := workflow.RunLocal(context.Background(), workflow.RunLocalRequest{
		File: "testdata/hello.duck.yaml",
	})
	if err != nil {
		t.Fatalf("RunLocal() error = %v, want nil", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("RunLocal() ExitCode = %d, want 0", res.ExitCode)
	}
}
