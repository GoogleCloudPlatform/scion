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

package relay

import (
	"errors"
	"fmt"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
)

// TestSpliceCloseMapping (r2-F6): a splice leg that ended with a normal
// close (1000) ends the other leg with 1000, never 4504; other close codes
// pass through and non-close errors are 4504 upstream_unreachable.
func TestSpliceCloseMapping(t *testing.T) {
	for _, tc := range []struct {
		name       string
		err        error
		wantCode   uint32
		wantReason string
	}{
		{name: "normal close", err: &conduit.CloseError{Code: conduit.CloseNormal}, wantCode: conduit.CloseNormal},
		{name: "wrapped normal close", err: fmt.Errorf("write: %w", &conduit.CloseError{Code: conduit.CloseNormal, Reason: "bye"}), wantCode: conduit.CloseNormal},
		{name: "other close code", err: &conduit.CloseError{Code: conduit.CloseForbidden, Reason: "forbidden: x"}, wantCode: conduit.CloseForbidden, wantReason: "forbidden: x"},
		{name: "transport error", err: errors.New("broken pipe"), wantCode: conduit.CloseRelayTimeout, wantReason: reason(ReasonUpstreamUnreachable, "peer leg closed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, why := spliceClose(tc.err, "peer leg closed")
			if code != tc.wantCode || why != tc.wantReason {
				t.Fatalf("spliceClose = %d %q, want %d %q", code, why, tc.wantCode, tc.wantReason)
			}
		})
	}
}
