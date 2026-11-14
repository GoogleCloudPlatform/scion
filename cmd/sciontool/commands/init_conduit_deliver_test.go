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
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/tmuxdeliver"
	"github.com/GoogleCloudPlatform/scion/pkg/agentcontrol"
	core "github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/control"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

func TestChildRunsScionTmux(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{[]string{"sh", "-c", "tmux new-session -d -s scion -n agent claude \\; attach-session -t scion"}, true},
		{[]string{"tmux", "new-session", "-A", "-s", "scion"}, true},
		{[]string{"/usr/bin/tmux", "new", "-s", "scion"}, true},
		{[]string{"tmux", "new-session", "-A", "-s", "main"}, false},
		{[]string{"tmux", "attach-session", "-s", "scion"}, false},
		{[]string{"claude", "-s", "scion"}, false},
		{[]string{"sh", "-c", "claude --dangerously-skip-permissions"}, false},
		{nil, false},
	} {
		if got := childRunsScionTmux(tc.args); got != tc.want {
			t.Errorf("childRunsScionTmux(%q) = %t, want %t", tc.args, got, tc.want)
		}
	}
}

// TestConduitDeliverOptions: the route is registered only with the launch
// flag, tmux on PATH and the harness in the scion tmux session.
func TestConduitDeliverOptions(t *testing.T) {
	scionTmux := []string{"sh", "-c", "tmux new-session -d -s scion -n agent claude"}
	withFlag := func(k string) string {
		if k == control.EnvHubExperiments {
			return "hub.conduit,hub.conduit_deliver"
		}
		return ""
	}
	noFlag := func(k string) string {
		if k == control.EnvHubExperiments {
			return "hub.conduit"
		}
		return ""
	}
	hasTmux := func(string) (string, error) { return "/usr/bin/tmux", nil }
	noTmux := func(string) (string, error) { return "", errors.New("not found") }
	u := conduit.PTYUser{UID: 1000, GID: 1000, Username: "scion"}
	if conduitDeliverOptions(withFlag, hasTmux, scionTmux, u) == nil {
		t.Fatal("not registered with flag, tmux and the scion session")
	}
	if conduitDeliverOptions(noFlag, hasTmux, scionTmux, u) != nil {
		t.Fatal("registered without the launch flag")
	}
	if conduitDeliverOptions(withFlag, noTmux, scionTmux, u) != nil {
		t.Fatal("registered without tmux")
	}
	if conduitDeliverOptions(withFlag, hasTmux, []string{"claude"}, u) != nil {
		t.Fatal("registered without the scion tmux session")
	}
}

// recordingRunner records argv; every command succeeds.
type recordingRunner struct {
	mu    sync.Mutex
	calls [][]string
}

func (r *recordingRunner) Run(_ context.Context, argv []string, _ io.Reader) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, slices.Clone(argv))
	return "", nil
}

// TestPortForwardingConduitDeliver: with the route registered, the Hello
// advertises deliver-message next to rotate-token, and a hub RpcRequest on
// the agent's own session reaches it (JSON body, Content-Type passed on)
// and runs the flush.
func TestPortForwardingConduitDeliver(t *testing.T) {
	t.Cleanup(hub.SetTokenHome(t.TempDir()))
	h := newPFHub(t, true)
	client := hub.NewClientWithConfig(h.srv.URL, "token-1", "agent-1")
	env := map[string]string{
		conduit.EnvHubExperiments: conduit.ExperimentConduit + "," + control.ExperimentConduitDeliver,
		projectkeys.EnvProjectID:  "project-1",
		conduit.EnvLaunchID:       "launch-1",
	}
	rr := &recordingRunner{}
	ctx, cancel := context.WithCancel(context.Background())
	p := newPortForwarding(client, false, conduit.PTYUser{}, nil, &control.DeliverOptions{Runner: tmuxdeliver.Runner(rr)}, nil, func(k string) string { return env[k] })
	done := make(chan struct{})
	go func() { p.run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	var hello *conduitv1.Hello
	select {
	case hello = <-h.hellos:
	case <-time.After(pfWait):
		t.Fatal("no conduit Hello")
	}
	if got := hello.GetCapabilities().GetRpc(); !slices.Equal(got, []string{control.RouteRotateToken, control.RouteDeliverMessage}) {
		t.Fatalf("Hello rpc = %v", got)
	}
	var s core.Session
	select {
	case s = <-h.sessions:
	case <-time.After(pfWait):
		t.Fatal("no conduit session")
	}
	text := "hi"
	body, _ := json.Marshal(agentcontrol.DeliverRequest{
		V: 1, DeliveryID: "d1", BrokerID: "b1", MessageIDs: []string{"m1"}, Op: agentcontrol.OpPaste,
		Text: &text, DeadlineMS: 92000, NotAfterUnixMS: time.Now().Add(92 * time.Second).UnixMilli(),
		SubmitKeys: []string{"Enter"},
	})
	resp, err := s.Call(ctx, &conduitv1.RpcRequest{
		Method: "POST", Path: "/v1/control/deliver-message", Body: body,
		Headers: map[string]string{"Content-Type": "application/json", "Authorization": "Bearer x"},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := agentcontrol.DecodeDeliverResponse(resp.GetBody())
	if resp.GetStatus() != 200 || err != nil || out.Outcome != agentcontrol.OutcomeDelivered {
		t.Fatalf("status %d body %q", resp.GetStatus(), resp.GetBody())
	}
	rr.mu.Lock()
	defer rr.mu.Unlock()
	if len(rr.calls) != 4 || rr.calls[2][1] != "paste-buffer" {
		t.Fatalf("calls = %v", rr.calls)
	}
}

// TestSciontoolDoesNotImportAgentManager: sciontool uses the delivery
// steps through pkg/agent/tmuxdeliver (and pkg/agent/state) only, never the
// agent manager in pkg/agent (design v2.22 §3.4).
func TestSciontoolDoesNotImportAgentManager(t *testing.T) {
	cmd := exec.Command("go", "list", "-deps", "./cmd/sciontool/...")
	cmd.Dir = findRepoRoot(t)
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("go list failed (may not have full module context): %v", err)
	}
	deps := strings.Fields(string(out))
	if slices.Contains(deps, "github.com/GoogleCloudPlatform/scion/pkg/agent") {
		t.Fatal("sciontool imports pkg/agent (the agent manager); use pkg/agent/tmuxdeliver")
	}
	if !slices.Contains(deps, "github.com/GoogleCloudPlatform/scion/pkg/agent/tmuxdeliver") {
		t.Fatal("sciontool no longer imports pkg/agent/tmuxdeliver; this test checks nothing")
	}
}
