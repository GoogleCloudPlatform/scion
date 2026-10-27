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
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/util/privatefs"
)

// captureNotices sends the package's stderr notices to a buffer and resets
// the once-per-process state for the test.
func captureNotices(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	mu.Lock()
	origOut, origReported := noticeOut, refusalReported.Load()
	noticeOut = &buf
	refusalReported.Store(false)
	mu.Unlock()
	t.Cleanup(func() {
		mu.Lock()
		noticeOut = origOut
		refusalReported.Store(origReported)
		mu.Unlock()
	})
	return &buf
}

// A privacy refusal from a read path is printed once per process, with the
// refusal's own text (which carries the fix hint); other load errors are
// not printed.
func TestReportRefusal_OncePerProcessAndOnlyNotPrivate(t *testing.T) {
	buf := captureNotices(t)
	refusal := fmt.Errorf("failed to read credentials file: refusing to read credentials from /x: %w. Delete the file and log in again", privatefs.ErrNotPrivate)

	mu.Lock()
	reportRefusal(ErrNotAuthenticated)
	reportRefusal(fmt.Errorf("failed to parse credentials file: %w", errors.New("bad json")))
	reportRefusal(fmt.Errorf("x: %w", fs.ErrPermission))
	mu.Unlock()
	if buf.Len() != 0 {
		t.Fatalf("non-refusal load errors printed %q, want nothing", buf.String())
	}

	mu.Lock()
	reportRefusal(refusal)
	reportRefusal(refusal)
	mu.Unlock()
	if got, want := buf.String(), "Warning: "+refusal.Error()+"\n"; got != want {
		t.Fatalf("notices = %q, want exactly one %q", got, want)
	}
}

// Concurrent reports of the refusal still print it exactly once: the
// once-only check is atomic and does not rely on mu.
func TestReportRefusal_OnceUnderConcurrency(t *testing.T) {
	buf := captureNotices(t)
	refusal := fmt.Errorf("refusing to read credentials from /x: %w", privatefs.ErrNotPrivate)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reportRefusal(refusal)
		}()
	}
	wg.Wait()
	if got, want := buf.String(), "Warning: "+refusal.Error()+"\n"; got != want {
		t.Fatalf("notices = %q, want exactly one %q", got, want)
	}
}

// The login warning is printed only when the file it replaces was refused
// as not private.
func TestWarnReplacingRefused_OnlyNotPrivate(t *testing.T) {
	buf := captureNotices(t)
	mu.Lock()
	warnReplacingRefused("/x/credentials.json", nil)
	warnReplacingRefused("/x/credentials.json", ErrNotAuthenticated)
	warnReplacingRefused("/x/credentials.json", errors.New("failed to parse credentials file"))
	mu.Unlock()
	if buf.Len() != 0 {
		t.Fatalf("warning printed for a non-refused file: %q", buf.String())
	}

	mu.Lock()
	warnReplacingRefused("/x/credentials.json", fmt.Errorf("refusing: %w", privatefs.ErrNotPrivate))
	mu.Unlock()
	for _, want := range []string{"/x/credentials.json", "not private", "replaced", "other hubs", "scion hub auth login"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("warning %q does not mention %q", buf.String(), want)
		}
	}
}
