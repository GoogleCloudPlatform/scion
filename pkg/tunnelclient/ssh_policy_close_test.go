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

package tunnelclient

import (
	"io"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// TestSSHStdioClosedByPolicyExits7: an ssh tunnel on stdio (the
// ProxyCommand form) that the hub closes with 4401 authz_expired exits 7
// with the policy line, after the bytes sent before the close reach
// stdout.
func TestSSHStdioClosedByPolicyExits7(t *testing.T) {
	const banner = "SSH-2.0-agent\r\n"
	h := newFakeHub(t)
	h.set(func() {
		h.handle = func(c rpcCall) *conduitv1.RpcResponse {
			return openTunnelResp(c, func(st conduit.Stream) {
				_, _ = io.WriteString(st, banner)
				_ = st.CloseWithCode(conduit.CloseUnauthenticated, "authz_expired")
			})
		}
	})
	// stdin stays open: only the stream's close ends the run.
	pr, pw := io.Pipe()
	defer func() { _ = pw.Close() }()
	var out syncBuffer
	req := Request{Agent: "a1", Project: "proj", SSH: true, Stdio: true, Stdin: pr, Stdout: &out}
	err := Run(runCtx(t), testConfig(h, newFakeREST(), credSeq(oauth("tok1"))), req)
	if got := CodeOf(err); got != ExitPolicyClosed {
		t.Fatalf("exit %d (%v), want %d", got, err, ExitPolicyClosed)
	}
	if err == nil || !strings.Contains(err.Error(), "tunnel closed by policy (authz_expired)") {
		t.Fatalf("error %v does not name the policy close", err)
	}
	if out.String() != banner {
		t.Fatalf("stdout = %q, want %q", out.String(), banner)
	}
	h.set(func() {
		if len(h.calls) != 1 || h.calls[0].body.Kind != "ssh" {
			t.Fatalf("calls = %+v, want one ssh tunnel request", h.calls)
		}
	})
}
