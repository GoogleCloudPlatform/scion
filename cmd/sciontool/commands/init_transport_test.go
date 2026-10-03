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

package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
)

// TestStageTransportToken_MovesEnvToFile verifies the bootstrap transport
// credential is written to a 0600 file and removed from the environment
// children inherit (and from the shell-sourceable scion-env file), with
// SCION_TRANSPORT_TOKEN_FILE pointing at the file instead.
func TestStageTransportToken_MovesEnvToFile(t *testing.T) {
	home := t.TempDir()
	t.Cleanup(hub.SetTokenHome(home))

	token := makeDoctorTestJWT(time.Now().Add(time.Hour))
	t.Setenv(transportauth.EnvTransportToken, token)
	t.Setenv(transportauth.EnvTransportTokenFile, "")

	if !stageTransportToken(0, 0) {
		t.Fatal("stageTransportToken returned false")
	}

	if _, ok := os.LookupEnv(transportauth.EnvTransportToken); ok {
		t.Errorf("%s still set after staging", transportauth.EnvTransportToken)
	}
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, transportauth.EnvTransportToken+"=") {
			t.Errorf("child environment still carries %s", transportauth.EnvTransportToken)
		}
	}

	wantPath := filepath.Join(home, ".scion", transportauth.TransportTokenFileName)
	if got := os.Getenv(transportauth.EnvTransportTokenFile); got != wantPath {
		t.Errorf("%s = %q, want %q", transportauth.EnvTransportTokenFile, got, wantPath)
	}
	fi, err := os.Stat(wantPath)
	if err != nil {
		t.Fatalf("transport token file not written: %v", err)
	}
	if fi.Mode().Perm() != 0600 {
		t.Errorf("transport token file mode = %o, want 0600", fi.Mode().Perm())
	}
	data, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != token {
		t.Error("transport token file does not hold the bootstrap value")
	}

	writeEnvFile(home, 0, 0)
	envFile, err := os.ReadFile(filepath.Join(home, ".scion", "scion-env"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(envFile), transportauth.EnvTransportToken+"=") {
		t.Error("scion-env still exports the bootstrap transport credential")
	}
	if !strings.Contains(string(envFile), transportauth.EnvTransportTokenFile+"=") {
		t.Error("scion-env does not export the transport token file path")
	}

	// A fresh in-agent client now gets the credential from the file.
	src, err := transportauth.FromEnv()
	if err != nil || src == nil {
		t.Fatalf("FromEnv() = %v, %v", src, err)
	}
	got, err := src.Token()
	if err != nil || got != token {
		t.Error("FromEnv source does not return the file value")
	}
}

// TestStageTransportToken_KeepsNewerFile verifies an in-place restart with
// an older bootstrap value does not clobber a newer refreshed value.
func TestStageTransportToken_KeepsNewerFile(t *testing.T) {
	home := t.TempDir()
	t.Cleanup(hub.SetTokenHome(home))

	newer := makeDoctorTestJWT(time.Now().Add(50 * time.Minute))
	if err := hub.WriteTransportTokenFile(newer, 0, 0); err != nil {
		t.Fatal(err)
	}
	older := makeDoctorTestJWT(time.Now().Add(-90 * time.Minute))
	t.Setenv(transportauth.EnvTransportToken, older)
	// stageTransportToken sets the FILE var; t.Setenv restores it afterwards.
	t.Setenv(transportauth.EnvTransportTokenFile, "")

	if !stageTransportToken(0, 0) {
		t.Fatal("stageTransportToken returned false")
	}
	data, err := os.ReadFile(hub.TransportTokenFilePath())
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != newer {
		t.Error("older bootstrap value overwrote the newer refreshed value")
	}
}

func TestStageTransportToken_NoEnv(t *testing.T) {
	t.Cleanup(hub.SetTokenHome(t.TempDir()))
	t.Setenv(transportauth.EnvTransportToken, "")
	t.Setenv(transportauth.EnvTransportTokenFile, "")
	if stageTransportToken(0, 0) {
		t.Error("stageTransportToken should be a no-op without the env var")
	}
}

// TestStageTransportToken_UnsetsBootstrapExpiry verifies the bootstrap-only
// expiry is removed from the environment along with the bootstrap value.
func TestStageTransportToken_UnsetsBootstrapExpiry(t *testing.T) {
	t.Cleanup(hub.SetTokenHome(t.TempDir()))
	t.Setenv(transportauth.EnvTransportToken, makeDoctorTestJWT(time.Now().Add(time.Hour)))
	t.Setenv(transportauth.EnvTransportTokenFile, "")
	t.Setenv(transportauth.EnvTransportTokenExpiry, time.Now().Add(time.Hour).Format(time.RFC3339))

	if !stageTransportToken(0, 0) {
		t.Fatal("stageTransportToken returned false")
	}
	if _, ok := os.LookupEnv(transportauth.EnvTransportTokenExpiry); ok {
		t.Errorf("%s still set after staging", transportauth.EnvTransportTokenExpiry)
	}
}

// TestStageTransportToken_RemovesStaleFile verifies a transport token file
// left in a persisted home is removed when this start provides no transport
// token, so it cannot be used.
func TestStageTransportToken_RemovesStaleFile(t *testing.T) {
	t.Cleanup(hub.SetTokenHome(t.TempDir()))
	if err := hub.WriteTransportTokenFile(makeDoctorTestJWT(time.Now().Add(time.Hour)), 0, 0); err != nil {
		t.Fatal(err)
	}
	t.Setenv(transportauth.EnvTransportToken, "")
	t.Setenv(transportauth.EnvTransportTokenFile, "")

	if stageTransportToken(0, 0) {
		t.Error("stageTransportToken should return false without the env var")
	}
	if _, err := os.Lstat(hub.TransportTokenFilePath()); !os.IsNotExist(err) {
		t.Errorf("stale transport token file still present (err=%v)", err)
	}
}

// TestStageTransportToken_KeepsFileAfterReExec verifies the file staged
// before the re-exec (SCION_TRANSPORT_TOKEN_FILE set, bootstrap value gone)
// is not removed.
func TestStageTransportToken_KeepsFileAfterReExec(t *testing.T) {
	t.Cleanup(hub.SetTokenHome(t.TempDir()))
	if err := hub.WriteTransportTokenFile(makeDoctorTestJWT(time.Now().Add(time.Hour)), 0, 0); err != nil {
		t.Fatal(err)
	}
	t.Setenv(transportauth.EnvTransportToken, "")
	t.Setenv(transportauth.EnvTransportTokenFile, hub.TransportTokenFilePath())

	stageTransportToken(0, 0)
	if _, err := os.Lstat(hub.TransportTokenFilePath()); err != nil {
		t.Errorf("staged transport token file removed: %v", err)
	}
}

// TestStageTransportToken_StaleSymlinkNotFollowed verifies removal unlinks a
// symlink planted at the file path rather than its target.
func TestStageTransportToken_StaleSymlinkNotFollowed(t *testing.T) {
	home := t.TempDir()
	t.Cleanup(hub.SetTokenHome(home))
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".scion"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, hub.TransportTokenFilePath()); err != nil {
		t.Fatal(err)
	}
	t.Setenv(transportauth.EnvTransportToken, "")
	t.Setenv(transportauth.EnvTransportTokenFile, "")

	stageTransportToken(0, 0)
	if _, err := os.Stat(target); err != nil {
		t.Errorf("symlink target affected: %v", err)
	}
}
