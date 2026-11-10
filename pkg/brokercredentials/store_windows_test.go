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

package brokercredentials

import (
	"encoding/base64"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/internal/testutil/wintest"
	"github.com/GoogleCloudPlatform/scion/pkg/util/privatefs"
)

func windowsFixture(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), DefaultFileName)
	store := NewStore(path)
	writeStoreFixture(t, store, &BrokerCredentials{
		BrokerID:  "b-1",
		SecretKey: base64.StdEncoding.EncodeToString([]byte("k")),
	})
	return store, path
}

// The read that allowed hub commands reach (getCurrentHostBrokerID) refuses
// a broker credentials file other users can read, and leaves it as it is,
// by the same rule as the CLI credentials. Control: the same
// file loads while it is private.
func TestWindowsBrokerCredentials_RefusesUsersReadableFile(t *testing.T) {
	store, path := windowsFixture(t)
	if c, err := store.Load(); err != nil || c.BrokerID != "b-1" {
		t.Fatalf("control: Load of the private file = %v, %v", c, err)
	}
	wintest.MakeUsersReadable(t, path)
	_, err := store.Load()
	if !errors.Is(err, privatefs.ErrNotPrivate) {
		t.Fatalf("Load of a Users-readable file = %v, want ErrNotPrivate", err)
	}
	for _, want := range []string{path, wintest.SIDUsers, "Delete the file"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not mention %q", err, want)
		}
	}
	if !wintest.ReadSD(t, path).HasAllowACE(wintest.SIDUsers) {
		t.Error("the refused file was modified; a read-side refusal must not repair")
	}
}

// A symlinked broker credentials file is refused, never followed. Control:
// its target loads as the credentials file.
func TestWindowsBrokerCredentials_RefusesSymlink(t *testing.T) {
	target, targetPath := windowsFixture(t)
	link := filepath.Join(t.TempDir(), DefaultFileName)
	wintest.MustSymlink(t, targetPath, link)
	if _, err := NewStore(link).Load(); !errors.Is(err, privatefs.ErrReparsePoint) {
		t.Fatalf("Load through a symlink = %v, want ErrReparsePoint", err)
	}
	if c, err := target.Load(); err != nil || c.BrokerID != "b-1" {
		t.Fatalf("control: Load of the target = %v, %v", c, err)
	}
}

// A missing file is still ErrNotFound through privatefs (missing file and
// missing directory).
func TestWindowsBrokerCredentials_MissingIsNotFound(t *testing.T) {
	for _, p := range []string{
		filepath.Join(t.TempDir(), DefaultFileName),
		filepath.Join(t.TempDir(), "absent", DefaultFileName),
	} {
		if _, err := NewStore(p).Load(); !errors.Is(err, ErrNotFound) {
			t.Errorf("Load(%s) = %v, want ErrNotFound", p, err)
		}
	}
}
