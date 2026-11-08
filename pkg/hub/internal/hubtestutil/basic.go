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

package hubtestutil

import (
	"context"
	"testing"
	"time"
)

// DevToken is the development token used for testing.
const DevToken = "scion_dev_test_token_for_unit_tests_1234567890"

// StrPtr returns a pointer to a copy of s.
func StrPtr(s string) *string { return &s }

// IntPtr returns a pointer to a copy of i.
func IntPtr(i int) *int { return &i }

// Itoa is a minimal int-to-string without importing strconv.
func Itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// AlwaysAuthorized is a permissive pkg/hub authChecker for tests that don't exercise
// the authorization policy path.
func AlwaysAuthorized(_ context.Context, _ string) bool { return true }

// RequireNoError is a test helper that fails immediately on error.
func RequireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// WaitClosed fails the test if ch is not closed within the given duration;
// what names the awaited event in the failure message.
func WaitClosed(t *testing.T, ch <-chan struct{}, within time.Duration, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(within):
		t.Fatalf("timed out waiting for %s", what)
	}
}
