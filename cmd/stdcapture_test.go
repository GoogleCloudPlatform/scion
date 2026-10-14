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

package cmd

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

// captureStdout runs fn with os.Stdout redirected to a pipe and returns
// everything written to it. See captureFile.
func captureStdout(t testing.TB, fn func()) string {
	t.Helper()
	return captureFile(t, &os.Stdout, fn)
}

// captureStderr runs fn with os.Stderr redirected to a pipe and returns
// everything written to it. See captureFile.
func captureStderr(t testing.TB, fn func()) string {
	t.Helper()
	return captureFile(t, &os.Stderr, fn)
}

// captureFile runs fn with *target (os.Stdout or os.Stderr) redirected to a
// pipe and returns everything written to it.
func captureFile(t testing.TB, target **os.File, fn func()) string {
	t.Helper()
	out, err := redirectFile(target, fn)
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	return out
}

// redirectFile runs fn with *target redirected to a pipe and returns what fn
// wrote. The original file is restored and the pipe's write end closed in a
// defer, so a panic or t.FailNow inside fn can't leave later tests in the
// binary writing to a closed pipe. The pipe is drained concurrently, so fn
// can write more than the pipe buffer holds without blocking.
func redirectFile(target **os.File, fn func()) (string, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return "", err
	}
	// Buffered, so the reader can always hand off its result and exit,
	// even if fn panics or calls t.FailNow and the receive is never reached.
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		_ = r.Close()
		done <- buf.String()
	}()
	orig := *target
	*target = w
	func() {
		defer func() { _ = w.Close() }()
		defer func() { *target = orig }()
		fn()
	}()
	return <-done, nil
}

// goexitTB is a testing.TB whose FailNow ends the calling goroutine the way
// testing.T.FailNow does, without failing the real test.
type goexitTB struct {
	testing.TB
}

func (goexitTB) Helper()                 {}
func (goexitTB) FailNow()                { runtime.Goexit() }
func (f goexitTB) Fatalf(string, ...any) { f.FailNow() }
func (goexitTB) Errorf(string, ...any) {}
func (goexitTB) Cleanup(func())          {}

// runCapture runs fn under captureFile on a fresh goroutine, so a FailNow
// (runtime.Goexit) or panic inside fn ends only that goroutine. It returns
// the file fn saw as *target.
func runCapture(t *testing.T, target **os.File, fn func(tb testing.TB)) (inner *os.File) {
	t.Helper()
	tb := goexitTB{}
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		defer func() { _ = recover() }()
		captureFile(tb, target, func() {
			inner = *target
			fn(tb)
		})
	}()
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("capture did not return")
	}
	return inner
}

func TestCaptureFileRestoresOnFailure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target **os.File
	}{
		{"stdout", &os.Stdout},
		{"stderr", &os.Stderr},
	} {
		for _, failure := range []struct {
			name string
			fn   func(tb testing.TB)
		}{
			{"FailNow", func(tb testing.TB) { tb.FailNow() }},
			{"panic", func(testing.TB) { panic("boom") }},
		} {
			t.Run(tc.name+"/"+failure.name, func(t *testing.T) {
				orig := *tc.target
				inner := runCapture(t, tc.target, failure.fn)
				if *tc.target != orig {
					*tc.target = orig
					t.Fatalf("%s was not restored after %s inside the capture", tc.name, failure.name)
				}
				if inner == nil || inner == orig {
					t.Fatalf("%s was not redirected during the capture", tc.name)
				}
				if _, err := inner.Write([]byte("x")); err == nil {
					t.Fatalf("pipe write end still open after %s inside the capture", failure.name)
				}
			})
		}
	}
}

func TestCaptureFileLargeOutput(t *testing.T) {
	// Well past a pipe buffer, so a helper that reads only after fn returns
	// would block forever.
	want := strings.Repeat("0123456789abcdef", 1<<16)
	got := make(chan string, 1)
	go func() {
		got <- captureStdout(t, func() { fmt.Print(want) })
	}()
	select {
	case out := <-got:
		if out != want {
			t.Fatalf("captured %d bytes, want %d", len(out), len(want))
		}
	case <-time.After(30 * time.Second):
		t.Fatal("captureStdout blocked on output larger than the pipe buffer")
	}
}
