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

package credentials

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/internal/testutil/wintest"
	"github.com/GoogleCloudPlatform/scion/pkg/util/privatefs"
)

// assertPrivateFile checks the credentials file has the owner-only ACL.
func assertPrivateFile(t *testing.T, path string) {
	t.Helper()
	wintest.AssertPrivateSD(t, path)
}

var testToken = &TokenResponse{AccessToken: "tok", ExpiresIn: time.Hour}

// A credentials file other users can read is refused
// on load, with the delete-and-log-in-again hint, and is left as it is.
// Control: the same file loads while it is private.
func TestLoad_RefusesUsersReadableCredentials(t *testing.T) {
	path := useTempCredentials(t)
	captureNotices(t)
	if err := Store("https://hub.example.com", testToken); err != nil {
		t.Fatal(err)
	}
	if c, err := Load("https://hub.example.com"); err != nil || c.AccessToken != "tok" {
		t.Fatalf("control: Load of the private file = %v, %v", c, err)
	}
	wintest.MakeUsersReadable(t, path)
	_, err := Load("https://hub.example.com")
	if !errors.Is(err, privatefs.ErrNotPrivate) {
		t.Fatalf("Load of a Users-readable credentials file = %v, want ErrNotPrivate", err)
	}
	for _, want := range []string{path, wintest.SIDUsers, "Delete the file", "scion hub auth login", "owner-only ACL", "other hubs"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not mention %q", err, want)
		}
	}
	if !wintest.ReadSD(t, path).HasAllowACE(wintest.SIDUsers) {
		t.Error("the refused file was modified; a read-side refusal must not repair")
	}
	if IsAuthenticated("https://hub.example.com") {
		t.Error("IsAuthenticated is true for a refused credentials file")
	}
}

// A symlinked credentials file is refused, never followed.
// Control: the target itself loads when it is the credentials file.
func TestLoad_RefusesSymlinkedCredentials(t *testing.T) {
	path := useTempCredentials(t)
	target := filepath.Join(t.TempDir(), CredentialsFile)
	writeRawCredentials(t, target, map[string]string{"https://hub.example.com": "tok"})
	wintest.MustSymlink(t, target, path)
	if _, err := Load("https://hub.example.com"); !errors.Is(err, privatefs.ErrReparsePoint) {
		t.Fatalf("Load through a symlink = %v, want ErrReparsePoint", err)
	}
	credentialsPath = func() string { return target }
	if c, err := Load("https://hub.example.com"); err != nil || c.AccessToken != "tok" {
		t.Fatalf("control: Load of the target = %v, %v", c, err)
	}
}

// Saving creates the file with the owner-only ACL, in a directory
// created private, and replaces an existing too-open file with a private
// one (atomic replace: an implicit repair).
func TestStore_WritesPrivateAndReplacesTooOpenFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "new", "scion")
	path := filepath.Join(dir, CredentialsFile)
	orig := credentialsPath
	credentialsPath = func() string { return path }
	t.Cleanup(func() { credentialsPath = orig })
	captureNotices(t)

	if err := Store("https://hub.example.com", testToken); err != nil {
		t.Fatal(err)
	}
	wintest.AssertPrivateSD(t, dir)
	wintest.AssertPrivateSD(t, path)

	wintest.MakeUsersReadable(t, path)
	if err := Store("https://other.example.com", testToken); err != nil {
		t.Fatalf("Store over a too-open file: %v", err)
	}
	wintest.AssertPrivateSD(t, path)
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

// The read paths (GetAccessToken, IsAuthenticated,
// Load) report "not logged in" for a refused credentials file, so the
// refusal with its fix hint is printed on stderr, once per process.
// Control: a private file prints nothing.
func TestReadPaths_PrintRefusalOnceForUsersReadableCredentials(t *testing.T) {
	path := useTempCredentials(t)
	buf := captureNotices(t)
	if err := Store("https://hub.example.com", testToken); err != nil {
		t.Fatal(err)
	}
	if GetAccessToken("https://hub.example.com") != "tok" || !IsAuthenticated("https://hub.example.com") {
		t.Fatal("control: the private file did not load")
	}
	if buf.Len() != 0 {
		t.Fatalf("control: a private file printed %q", buf.String())
	}

	wintest.MakeUsersReadable(t, path)
	if tok := GetAccessToken("https://hub.example.com"); tok != "" {
		t.Fatalf("GetAccessToken of a refused file = %q, want empty", tok)
	}
	if IsAuthenticated("https://hub.example.com") {
		t.Fatal("IsAuthenticated is true for a refused file")
	}
	if _, err := Load("https://hub.example.com"); !errors.Is(err, privatefs.ErrNotPrivate) {
		t.Fatalf("Load = %v, want ErrNotPrivate", err)
	}
	out := buf.String()
	if n := strings.Count(out, "refusing to read credentials"); n != 1 {
		t.Fatalf("refusal printed %d times, want once: %q", n, out)
	}
	for _, want := range []string{path, "Delete the file", "scion hub auth login", "other hubs"} {
		if !strings.Contains(out, want) {
			t.Errorf("printed refusal %q does not mention %q", out, want)
		}
	}
	if !wintest.ReadSD(t, path).HasAllowACE(wintest.SIDUsers) {
		t.Error("the refused file was modified by a read path")
	}
}

// A login over a refused credentials file warns that
// the file is replaced and other hubs' logins must be repeated.
// Control: a login over a private file warns nothing.
func TestStore_WarnsWhenReplacingRefusedCredentials(t *testing.T) {
	path := useTempCredentials(t)
	buf := captureNotices(t)
	if err := Store("https://hub.example.com", testToken); err != nil {
		t.Fatal(err)
	}
	if err := Store("https://other.example.com", testToken); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 0 {
		t.Fatalf("control: a login over a private file printed %q", buf.String())
	}

	wintest.MakeUsersReadable(t, path)
	if err := Store("https://third.example.com", testToken); err != nil {
		t.Fatalf("Store over a refused file: %v", err)
	}
	out := buf.String()
	for _, want := range []string{path, "not private", "replaced", "other hubs", "scion hub auth login"} {
		if !strings.Contains(out, want) {
			t.Errorf("login warning %q does not mention %q", out, want)
		}
	}
	wintest.AssertPrivateSD(t, path)
	if IsAuthenticated("https://hub.example.com") {
		t.Error("a login from the replaced file survived; the refused file must not be read")
	}
	if !IsAuthenticated("https://third.example.com") {
		t.Error("the new login is missing")
	}
}
