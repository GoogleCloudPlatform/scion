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

//go:build !no_sqlite && unix

package hub

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestGCSLink_RealSource_NoADCDetection proves gcsObjectSourceFor never
// runs ADC (Application Default Credentials) detection. A merely-missing
// or malformed GOOGLE_APPLICATION_CREDENTIALS path does not distinguish
// this: the storage client's own ADC detection swallows a read or parse
// error there rather than surfacing it, so client construction would
// succeed either way. Instead, GOOGLE_APPLICATION_CREDENTIALS points at a
// FIFO with no writer: a real attempt to read it — the open() call alone,
// before any read — blocks until a writer appears, so the only way this
// test can pass is if nothing ever tries to open that path. The call runs
// in a goroutine racing a bound: if ADC detection is skipped, it returns
// almost immediately; if it ran, it is still blocked when the bound
// expires, and the test fails there without waiting for it to return —
// detection's own retries after the FIFO unblocks are not bounded by
// anything this test controls. The FIFO is still opened read-write
// (which never blocks a FIFO open, regardless of other openers) once the
// bound expires and again at cleanup, so a goroutine stuck in the open()
// call can exit on its own in the background rather than hanging forever.
//
// This test lives in its own unix-tagged file (syscall.Mkfifo has no
// Windows equivalent): only this test and its exclusively-used imports
// move here, so the rest of this package's test suite — including every
// other RealClient test in gcs_link_realclient_test.go that shares
// fakeGCSJSONServer — is unaffected and keeps compiling on every platform.
func TestGCSLink_RealSource_NoADCDetection(t *testing.T) {
	before := goroutineBaseline()

	fifoPath := filepath.Join(t.TempDir(), "gcs-link-test-credentials")
	require.NoError(t, syscall.Mkfifo(fifoPath, 0o600))
	var unblockOnce sync.Once
	unblockFIFO := func() {
		unblockOnce.Do(func() {
			if f, err := os.OpenFile(fifoPath, os.O_RDWR, 0); err == nil {
				_ = f.Close()
			}
		})
	}
	t.Cleanup(unblockFIFO)
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", fifoPath)

	fake := &fakeGCSJSONServer{name: "o.txt", size: 5, generation: 9, body: []byte("hello")}
	ts := httptest.NewServer(fake)

	gen := &fakeGCSTokenGenerator{}
	srv := &Server{
		gcpTokenGenerator:       gen,
		gcsLinkBaseTransport:    newGCSLinkBaseTransport(),
		gcsLinkEndpointOverride: ts.URL,
	}

	type sourceResult struct {
		source  gcsObjectSource
		release func()
		err     error
	}
	done := make(chan sourceResult, 1)
	go func() {
		source, release, err := srv.gcsObjectSourceFor(context.Background(), "sa-noadc@test.iam.gserviceaccount.com")
		done <- sourceResult{source, release, err}
	}()

	var res sourceResult
	select {
	case res = <-done:
	case <-time.After(2 * time.Second):
		// Do not wait for the blocked call to actually return here: whatever
		// client the open call eventually produces is out of scope for this
		// test, and nothing bounds how long detection's own retries might
		// take after the FIFO unblocks. unblockFIFO (also registered via
		// t.Cleanup) is still called so the goroutine can exit on its own in
		// the background instead of blocking forever.
		unblockFIFO()
		ts.Close()
		t.Fatal("gcsObjectSourceFor did not return within 2s: ADC detection must never attempt to open the credentials path")
	}

	require.NoError(t, res.err)

	_, err := res.source.Attrs(context.Background(), "bkt", "o.txt")
	require.NoError(t, err)
	require.Equal(t, "Bearer fake-token-for-sa-noadc@test.iam.gserviceaccount.com", fake.lastAuthHeader(),
		"the request must be authenticated solely by the minted bearer, never a detected ADC credential")

	// Closed explicitly here, not deferred: a deferred close only runs after
	// this function returns, which is too late for the leak check below —
	// see the matching comment on the RealClient context-deadline tests.
	res.release()
	ts.Close()
	requireNoGoroutineLeak(t, before)
}
