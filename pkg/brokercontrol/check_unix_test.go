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

//go:build !windows

package brokercontrol

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCheck_PermissionErrorFailsClosed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	f := newFixture(t)
	locked := filepath.Join(f.outside, "locked")
	mkdir(t, filepath.Join(locked, "inner"))
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	_, err := Check(filepath.Join(locked, "inner"), VolumeAllowed)
	wantRefused(t, err, RuleUnresolved)
}

func TestCheck_HardLinkToControlFile(t *testing.T) {
	f := newFixture(t)
	link := filepath.Join(f.outside, "copy.json")
	if err := os.Link(filepath.Join(f.global, "broker-credentials.json"), link); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	_, err := Check(link, VolumeAllowed)
	wantRefused(t, err, RuleIdentity)
	// Documented residual: a directory that merely contains such a hard link
	// is not walked.
	if _, err := Check(f.outside, VolumeAllowed); err != nil {
		t.Fatalf("directory containing a hard link: %v", err)
	}
}

func TestTokenFilesHardLinkIdentity(t *testing.T) {
	for _, name := range []string{"transport-token", "scion-token"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			write(t, filepath.Join(f.global, name))
			link := filepath.Join(f.outside, name+".copy")
			if err := os.Link(filepath.Join(f.global, name), link); err != nil {
				t.Skipf("hard links unavailable: %v", err)
			}
			_, err := Check(link, VolumeAllowed)
			wantRefused(t, err, RuleIdentity)
		})
	}
}

func TestVolumeAllowed_PermissionErrorIsRefused(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	f := newFixture(t)
	locked := filepath.Join(f.global, "workspace", "locked")
	mkdir(t, filepath.Join(locked, "inner"))
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	if VolumeAllowed(filepath.Join(locked, "inner"), f.global) {
		t.Fatal("a path that cannot be resolved (permission error) was allowed")
	}
}
