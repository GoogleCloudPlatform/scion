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

//go:build unix

package shipper

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/util/privatefs"
)

func TestOpenArtifactNoFollowRefusesLeafAndIntermediateSwaps(t *testing.T) {
	home := t.TempDir()
	outside := t.TempDir()

	secret := filepath.Join(outside, "secret.jsonl")
	if err := os.WriteFile(secret, []byte("{\"secret\":true}\n"), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}

	ruleRoot := filepath.Join(home, ".claude", "projects")
	workDir := filepath.Join(ruleRoot, "-workspace")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("mkdir workDir: %v", err)
	}

	// Normal file opens cleanly.
	okFile := filepath.Join(workDir, "ok.jsonl")
	if err := os.WriteFile(okFile, []byte("{\"ok\":1}\n"), 0o644); err != nil {
		t.Fatalf("write okFile: %v", err)
	}
	f, fi, err := openArtifactNoFollow(ruleRoot, "-workspace/ok.jsonl")
	if err != nil {
		t.Fatalf("openArtifactNoFollow(ok): %v", err)
	}
	_ = f.Close()
	if !fi.Mode().IsRegular() {
		t.Fatalf("expected regular file")
	}

	// Leaf swap: replace ok.jsonl with a symlink to outside secret.
	if err := os.Remove(okFile); err != nil {
		t.Fatalf("remove okFile: %v", err)
	}
	if err := os.Symlink(secret, okFile); err != nil {
		t.Fatalf("symlink leaf: %v", err)
	}
	if f, _, err := openArtifactNoFollow(ruleRoot, "-workspace/ok.jsonl"); err == nil {
		_ = f.Close()
		t.Fatalf("expected leaf symlink swap to be refused")
	} else if !errors.Is(err, privatefs.ErrReparsePoint) {
		t.Fatalf("expected ErrReparsePoint on leaf swap, got %v", err)
	}

	// Intermediate-directory swap: replace -workspace with a symlink to outside.
	if err := os.RemoveAll(workDir); err != nil {
		t.Fatalf("remove workDir: %v", err)
	}
	if err := os.Symlink(outside, workDir); err != nil {
		t.Fatalf("symlink intermediate dir: %v", err)
	}
	if f, _, err := openArtifactNoFollow(ruleRoot, "-workspace/secret.jsonl"); err == nil {
		_ = f.Close()
		t.Fatalf("expected intermediate dir symlink swap to be refused")
	} else if !errors.Is(err, privatefs.ErrReparsePoint) {
		t.Fatalf("expected ErrReparsePoint on intermediate dir swap, got %v", err)
	}
}
