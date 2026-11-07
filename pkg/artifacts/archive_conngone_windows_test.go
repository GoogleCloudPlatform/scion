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

//go:build windows

package artifacts

import (
	"errors"
	"fmt"
	"net"
	"syscall"
	"testing"
)

func TestWindowsConnGone(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		want bool
	}{
		{"WSAECONNRESET", fmt.Errorf("wsasend: %w", syscall.WSAECONNRESET), true},
		{"WSAECONNABORTED", &net.OpError{Op: "write", Err: syscall.WSAECONNABORTED}, true},
		{"other", errors.New("disk full"), false},
	} {
		if got := connGone(c.err); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}
