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

package maintenance

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/unitdrift"
	"github.com/GoogleCloudPlatform/scion/pkg/preflight"
)

// unsetTrustedProxiesFinding is the finding a hub with an https public URL
// and server.trusted_proxies unset gets.
func unsetTrustedProxiesFinding() []preflight.Finding {
	return []preflight.Finding{{
		Check: preflight.TransportProxyCheckName, Code: preflight.TransportProxyCheckName + ".trusted_proxies_not_set",
		Severity: preflight.SeverityWarn, Message: "server.trusted_proxies is not set", Remediation: "list the proxy",
	}}
}

// update-binary writes the settings check after its preflight, and an
// unset required setting does not stop the update.
func TestBinaryUpdate_SettingsCheckWarnsOnly(t *testing.T) {
	requireLinux(t)
	gh := newFakeGitHub(t)
	gh.addRelease("nightly-20261023", "2026-10-23T04:00:00Z", "nightly-20261023")
	inst := &fakeInstaller{}
	e := newTestExecutor(gh, inst, "nightly-20261022")
	calls := 0
	e.settings = func() []preflight.Finding { calls++; return unsetTrustedProxiesFinding() }

	var log bytes.Buffer
	if err := e.Run(context.Background(), &log, map[string]string{"target_version": "nightly-20261023"}); err != nil {
		t.Fatalf("Run: %v\n%s", err, log.String())
	}
	out := log.String()
	if calls != 1 {
		t.Errorf("settings check ran %d times, want 1", calls)
	}
	section := strings.Index(out, "==> Settings check")
	if section < 0 || !strings.Contains(out, "WARNING ["+preflight.TransportProxyCheckName+".trusted_proxies_not_set]") {
		t.Fatalf("log lacks the settings check warning:\n%s", out)
	}
	if pre := strings.Index(out, "Preflight passed."); pre < 0 || pre > section {
		t.Errorf("the settings check is not after the installer preflight:\n%s", out)
	}
	if !strings.Contains(strings.Join(inst.calls, ","), "restart") {
		t.Errorf("the update did not go on to the restart: %v", inst.calls)
	}

	// Without a settings check there is no section.
	var plain bytes.Buffer
	e2 := newTestExecutor(gh, &fakeInstaller{}, "nightly-20261022")
	if err := e2.Run(context.Background(), &plain, map[string]string{"target_version": "nightly-20261023"}); err != nil {
		t.Fatalf("Run: %v\n%s", err, plain.String())
	}
	if strings.Contains(plain.String(), "Settings check") {
		t.Errorf("a section without a settings check:\n%s", plain.String())
	}
}

// rebuild-server writes the settings check before its first step, and an
// unset required setting does not stop the rebuild.
func TestRebuildServer_SettingsCheckWarnsOnly(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("rebuild-server only runs on linux")
	}
	repoDir, binDir, stubDir := t.TempDir(), t.TempDir(), t.TempDir()
	for _, cmd := range []string{"git", "make", "go", "sudo"} {
		script := "#!/bin/sh\nexit 0\n"
		if err := os.WriteFile(filepath.Join(stubDir, cmd), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", stubDir+":"+os.Getenv("PATH"))

	e := NewRebuildServerExecutor(RebuildServerOptions{
		RepoPath:    repoDir,
		RepoBranch:  "main",
		BinaryDest:  filepath.Join(binDir, "scion"),
		ServiceName: "test-scion",
		UnitLocator: func(context.Context, string) (unitdrift.UnitInfo, error) {
			return unitdrift.UnitInfo{}, fmt.Errorf("no systemd in tests")
		},
		UnitRevision:  func(context.Context, string) (string, error) { return "", nil },
		SettingsCheck: unsetTrustedProxiesFinding,
	})
	var log bytes.Buffer
	if err := e.Run(context.Background(), &log, nil); err != nil {
		t.Fatalf("Run: %v\n%s", err, log.String())
	}
	out := log.String()
	section := strings.Index(out, "==> Settings check")
	first := strings.Index(out, "==> Fetching latest code")
	if section < 0 || first < 0 || section > first {
		t.Fatalf("the settings check is not before the first step:\n%s", out)
	}
	if !strings.Contains(out, "WARNING ["+preflight.TransportProxyCheckName+".trusted_proxies_not_set]") {
		t.Errorf("log lacks the warning:\n%s", out)
	}
	if !strings.Contains(out, "==> Installing server binary") {
		t.Errorf("the rebuild did not go on:\n%s", out)
	}
}
