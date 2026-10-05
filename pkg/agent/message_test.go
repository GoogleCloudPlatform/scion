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

package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

func TestMessage(t *testing.T) {
	// Interrupt messages bypass the buffer and are delivered immediately.
	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{
				{
					ContainerID:     "agent-1",
					Name:            "test-agent",
					ContainerStatus: "Up 2 minutes",
					Labels:          map[string]string{"scion.name": "test-agent"},
				},
			}, nil
		},
	}

	var capturedCmd []string
	mockRT.ExecFunc = func(ctx context.Context, id string, cmd []string) (string, error) {
		capturedCmd = append(capturedCmd, strings.Join(cmd, " "))
		return "", nil
	}

	mgr := &AgentManager{
		Runtime: mockRT,
	}
	// Initialize buffer (not used for interrupt messages, but needed to avoid nil).
	mgr.msgBuffer = NewMessageBuffer(100*time.Millisecond, func(agentID, projectID, message string, interrupt bool) error {
		return mgr.deliverImmediate(context.Background(), agentID, projectID, message, interrupt)
	})
	defer mgr.msgBuffer.Close()

	ctx := context.Background()
	err := mgr.Message(ctx, "test-agent", "", "hello world", true)
	if err != nil {
		t.Fatalf("Message failed: %v", err)
	}

	if len(capturedCmd) != 6 {
		t.Fatalf("Expected 6 commands, got %d: %v", len(capturedCmd), capturedCmd)
	}

	bufName := bufNameFromLoadCmd(t, capturedCmd[1])
	expectedCmds := []string{
		"tmux send-keys -t scion:0 C-c",
		"tmux load-buffer -b " + bufName + " -",
		"tmux paste-buffer -t scion:0 -p -d -b " + bufName,
		"tmux send-keys -t scion:0 Enter",
		"tmux send-keys -t scion:0 Enter",
		"tmux send-keys -t scion:0 Enter",
	}

	for i, cmd := range capturedCmd {
		if cmd != expectedCmds[i] {
			t.Errorf("Expected cmd %d to be '%s', got '%s'", i, expectedCmds[i], cmd)
		}
	}
}

// parseBufNameFromLoadCmd extracts the buffer name from a captured
// "tmux load-buffer -b <name> -" command string. It reports failure via its
// return value rather than *testing.T so it's safe to call from a goroutine
// other than the test's own, e.g. inside a mock invoked concurrently by
// deliverImmediate's delivery goroutines.
func parseBufNameFromLoadCmd(loadCmd string) (string, error) {
	parts := strings.Fields(loadCmd)
	for i, p := range parts {
		if p == "-b" && i+1 < len(parts) {
			return parts[i+1], nil
		}
	}
	return "", fmt.Errorf("could not find buffer name in load-buffer command: %q", loadCmd)
}

// bufNameFromLoadCmd is parseBufNameFromLoadCmd for callers on the test
// goroutine itself, where failing the test directly is safe.
func bufNameFromLoadCmd(t *testing.T, loadCmd string) string {
	t.Helper()
	name, err := parseBufNameFromLoadCmd(loadCmd)
	if err != nil {
		t.Fatal(err)
	}
	return name
}

func TestBroadcast(t *testing.T) {
	// Non-interrupt messages go through the debounce buffer. When sent to
	// different agents, each agent's buffer flushes independently.
	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{
				{
					ContainerID:     "agent-1",
					Name:            "test-agent-1",
					ContainerStatus: "Up 2 minutes",
					Labels:          map[string]string{"scion.name": "test-agent-1"},
				},
				{
					ContainerID:     "agent-2",
					Name:            "test-agent-2",
					ContainerStatus: "Up 1 minute",
					Labels:          map[string]string{"scion.name": "test-agent-2"},
				},
			}, nil
		},
	}

	var mu sync.Mutex
	var capturedCalls []string
	done := make(chan struct{}, 6)
	mockRT.ExecFunc = func(ctx context.Context, id string, cmd []string) (string, error) {
		mu.Lock()
		capturedCalls = append(capturedCalls, fmt.Sprintf("%s: %s", id, strings.Join(cmd, " ")))
		// Signal done for each bare Enter keypress (two trailing Enters per agent delivery).
		// Bare Enter: ["tmux", "send-keys", "-t", "scion:0", "Enter"] → len 5.
		if len(cmd) == 5 && cmd[0] == "tmux" && cmd[1] == "send-keys" && cmd[4] == "Enter" {
			done <- struct{}{}
		}
		mu.Unlock()
		return "", nil
	}

	mgr := &AgentManager{
		Runtime: mockRT,
	}
	// Use a short buffer delay for testing.
	mgr.msgBuffer = NewMessageBuffer(100*time.Millisecond, func(agentID, projectID, message string, interrupt bool) error {
		return mgr.deliverImmediate(context.Background(), agentID, projectID, message, interrupt)
	})
	defer mgr.msgBuffer.Close()

	ctx := context.Background()
	// Broadcast is handled by CLI loop usually, but let's test mgr.Message on both.
	// Non-interrupt messages are buffered and delivered after the debounce window.
	err := mgr.Message(ctx, "test-agent-1", "", "hello", false)
	if err != nil {
		t.Fatalf("Message 1 failed: %v", err)
	}
	err = mgr.Message(ctx, "test-agent-2", "", "hello", false)
	if err != nil {
		t.Fatalf("Message 2 failed: %v", err)
	}

	// Wait for both buffered deliveries to complete (3 Enters per agent × 2 agents).
	for i := 0; i < 6; i++ {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for buffered delivery")
		}
	}

	mu.Lock()
	defer mu.Unlock()

	if len(capturedCalls) != 10 {
		t.Fatalf("Expected 10 calls, got %d: %v", len(capturedCalls), capturedCalls)
	}

	// Since buffer delivery is async, agents may flush in either order.
	// Verify each agent's commands appear together and in the right sequence.
	agent1Calls := filterByPrefix(capturedCalls, "agent-1:")
	agent2Calls := filterByPrefix(capturedCalls, "agent-2:")

	if len(agent1Calls) != 5 || len(agent2Calls) != 5 {
		t.Fatalf("Expected 5 calls per agent, got agent-1=%d agent-2=%d", len(agent1Calls), len(agent2Calls))
	}

	// Buffer names are not required to differ across agents: each agent has
	// its own tmux server (a different container), so a name reused there
	// isn't a collision. TestDeliverImmediate_ConcurrentDeliveriesUseDistinctBufferNames
	// covers uniqueness for two overlapping deliveries to one agent, which is
	// the property that actually matters.
	buf1 := bufNameFromLoadCmd(t, strings.TrimPrefix(agent1Calls[0], "agent-1: "))
	buf2 := bufNameFromLoadCmd(t, strings.TrimPrefix(agent2Calls[0], "agent-2: "))

	expectedAgent1 := []string{
		"agent-1: tmux load-buffer -b " + buf1 + " -",
		"agent-1: tmux paste-buffer -t scion:0 -p -d -b " + buf1,
		"agent-1: tmux send-keys -t scion:0 Enter",
		"agent-1: tmux send-keys -t scion:0 Enter",
		"agent-1: tmux send-keys -t scion:0 Enter",
	}
	for i, want := range expectedAgent1 {
		if agent1Calls[i] != want {
			t.Errorf("Unexpected agent-1 call[%d]: got %q, want %q", i, agent1Calls[i], want)
		}
	}

	expectedAgent2 := []string{
		"agent-2: tmux load-buffer -b " + buf2 + " -",
		"agent-2: tmux paste-buffer -t scion:0 -p -d -b " + buf2,
		"agent-2: tmux send-keys -t scion:0 Enter",
		"agent-2: tmux send-keys -t scion:0 Enter",
		"agent-2: tmux send-keys -t scion:0 Enter",
	}
	for i, want := range expectedAgent2 {
		if agent2Calls[i] != want {
			t.Errorf("Unexpected agent-2 call[%d]: got %q, want %q", i, agent2Calls[i], want)
		}
	}
}

// TestDeliverImmediate_PartialDeliveryAfterPaste covers #1866: once
// "tmux paste-buffer" has succeeded, the message text is already sitting in
// the agent's terminal input. A later failure (the closing Enter, or one of
// the confirmation Enters) must be reported as a PartialDeliveryError so the
// message buffer's bounded retry does not re-run the whole delivery — doing
// so would re-paste the text and the agent would see it twice.
func TestDeliverImmediate_PartialDeliveryAfterPaste(t *testing.T) {
	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{
				{ContainerID: "agent-1", Name: "test-agent", Labels: map[string]string{"scion.name": "test-agent"}},
			}, nil
		},
	}
	mockRT.ExecFunc = func(ctx context.Context, id string, cmd []string) (string, error) {
		if len(cmd) >= 2 && cmd[1] == "send-keys" && cmd[len(cmd)-1] == "Enter" {
			return "", fmt.Errorf("exec failed")
		}
		return "", nil
	}

	mgr := &AgentManager{Runtime: mockRT}
	err := mgr.deliverImmediate(context.Background(), "test-agent", "", "hello", false)
	if err == nil {
		t.Fatal("expected an error")
	}
	var partial *PartialDeliveryError
	if !errors.As(err, &partial) {
		t.Fatalf("expected a PartialDeliveryError once paste-buffer succeeded, got %T: %v", err, err)
	}
}

// TestDeliverImmediate_RetryableBeforePaste covers #1866: a failure before
// any content reaches the terminal (e.g. the initial "tmux load-buffer") is
// safe to retry and must not be wrapped as a PartialDeliveryError.
func TestDeliverImmediate_RetryableBeforePaste(t *testing.T) {
	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{
				{ContainerID: "agent-1", Name: "test-agent", Labels: map[string]string{"scion.name": "test-agent"}},
			}, nil
		},
	}
	mockRT.ExecFunc = func(ctx context.Context, id string, cmd []string) (string, error) {
		if len(cmd) >= 2 && cmd[1] == "load-buffer" {
			return "", fmt.Errorf("exec failed")
		}
		return "", nil
	}

	mgr := &AgentManager{Runtime: mockRT}
	err := mgr.deliverImmediate(context.Background(), "test-agent", "", "hello", false)
	if err == nil {
		t.Fatal("expected an error")
	}
	var partial *PartialDeliveryError
	if errors.As(err, &partial) {
		t.Fatalf("failure before paste-buffer must not be wrapped as PartialDeliveryError: %v", err)
	}
}

// TestDeliverImmediate_ContextCanceledDuringEnterWait covers the gemini
// review follow-up on ptone/scion#1866 (GoogleCloudPlatform/scion#1893): the
// 300ms wait before each trailing confirmation Enter must respect context
// cancellation via select instead of an unconditional time.Sleep. Once
// paste-buffer has already delivered the message text, a cancellation during
// that wait is reported as a PartialDeliveryError — consistent with any other
// failure once delivery is no longer safe to retry — rather than sleeping out
// the full window regardless of ctx.
func TestDeliverImmediate_ContextCanceledDuringEnterWait(t *testing.T) {
	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{
				{ContainerID: "agent-1", Name: "test-agent", Labels: map[string]string{"scion.name": "test-agent"}},
			}, nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	var enterCalls int
	mockRT.ExecFunc = func(ctx context.Context, id string, cmd []string) (string, error) {
		if len(cmd) >= 2 && cmd[1] == "paste-buffer" {
			// Cancel right after the message text has been pasted, before the
			// post-delivery confirmation Enters begin waiting.
			cancel()
		}
		if len(cmd) >= 2 && cmd[1] == "send-keys" && cmd[len(cmd)-1] == "Enter" {
			enterCalls++
		}
		return "", nil
	}

	mgr := &AgentManager{Runtime: mockRT}
	start := time.Now()
	err := mgr.deliverImmediate(ctx, "test-agent", "", "hello", false)
	elapsed := time.Since(start)

	var partial *PartialDeliveryError
	if !errors.As(err, &partial) {
		t.Fatalf("expected a PartialDeliveryError on cancellation after paste-buffer, got %T: %v", err, err)
	}
	if elapsed >= 300*time.Millisecond {
		t.Fatalf("expected cancellation to short-circuit the 300ms wait, took %v", elapsed)
	}
	// One Enter closes the paste sequence itself; cancellation must prevent
	// the two confirmation Enters that follow.
	if enterCalls != 1 {
		t.Fatalf("expected exactly 1 Enter (the paste's closing keypress) before cancellation stopped further Enters, got %d", enterCalls)
	}
}

// TestDeliverImmediate_LargeMessageOverStdin covers ptone/scion#2256: tmux's
// client-server protocol caps a single command's argv around 16 KB, so
// "tmux set-buffer -- <message>" silently dropped larger (often coalesced)
// messages. The message body must instead be streamed via ExecWithStdin into
// a named buffer, never carried in any argv element. This sends a payload
// well over 16 KB containing newlines, quotes and angle brackets, and
// verifies it reaches ExecWithStdin byte-for-byte and that the named buffer
// used by load-buffer is the same one paste-buffer deletes on use (-d).
func TestDeliverImmediate_LargeMessageOverStdin(t *testing.T) {
	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{
				{ContainerID: "agent-1", Name: "test-agent", Labels: map[string]string{"scion.name": "test-agent"}},
			}, nil
		},
	}

	var b strings.Builder
	const line = "line with \"quotes\", <angle> brackets & an ampersand\n"
	for b.Len() < 20000 {
		b.WriteString(line)
	}
	payload := b.String()
	if len(payload) <= 16*1024 {
		t.Fatalf("test payload must exceed 16 KiB, got %d bytes", len(payload))
	}

	var argvCmds [][]string
	var stdinBody []byte
	mockRT.ExecFunc = func(ctx context.Context, id string, cmd []string) (string, error) {
		argvCmds = append(argvCmds, cmd)
		return "", nil
	}
	mockRT.ExecWithStdinFunc = func(ctx context.Context, id string, cmd []string, stdin io.Reader) (string, error) {
		argvCmds = append(argvCmds, cmd)
		data, err := io.ReadAll(stdin)
		if err != nil {
			t.Fatalf("reading stdin: %v", err)
		}
		stdinBody = data
		return "", nil
	}

	mgr := &AgentManager{Runtime: mockRT}
	if err := mgr.deliverImmediate(context.Background(), "test-agent", "", payload, false); err != nil {
		t.Fatalf("deliverImmediate failed: %v", err)
	}

	if string(stdinBody) != payload {
		t.Fatalf("stdin payload mismatch: got %d bytes, want %d bytes", len(stdinBody), len(payload))
	}

	// No argv element of any executed command may carry the message body.
	for _, cmd := range argvCmds {
		for _, arg := range cmd {
			if strings.Contains(arg, "quotes") {
				t.Fatalf("message body found in argv: %q (full cmd %v)", arg, cmd)
			}
		}
	}

	if len(argvCmds) < 2 {
		t.Fatalf("expected at least a load-buffer and a paste-buffer command, got %v", argvCmds)
	}
	loadCmd := argvCmds[0]
	if len(loadCmd) != 5 || loadCmd[0] != "tmux" || loadCmd[1] != "load-buffer" || loadCmd[2] != "-b" || loadCmd[4] != "-" {
		t.Fatalf("unexpected load-buffer command: %v", loadCmd)
	}
	bufName := loadCmd[3]
	if bufName == "" {
		t.Fatal("expected a named buffer, got an empty name")
	}

	pasteCmd := argvCmds[1]
	if pasteCmd[0] != "tmux" || pasteCmd[1] != "paste-buffer" {
		t.Fatalf("expected the second command to be paste-buffer, got %v", pasteCmd)
	}
	var hasP, hasD, pastesNamedBuf bool
	for i, arg := range pasteCmd {
		switch arg {
		case "-p":
			hasP = true
		case "-d":
			hasD = true
		case "-b":
			if i+1 < len(pasteCmd) && pasteCmd[i+1] == bufName {
				pastesNamedBuf = true
			}
		}
	}
	if !hasP {
		t.Errorf("expected paste-buffer to keep -p (bracketed paste): %v", pasteCmd)
	}
	if !hasD {
		t.Errorf("expected paste-buffer to use -d (delete buffer after paste): %v", pasteCmd)
	}
	if !pastesNamedBuf {
		t.Errorf("expected paste-buffer to reference the same named buffer %q loaded above: %v", bufName, pasteCmd)
	}
}

// TestMessageBuffer_CoalescedLargeMessagesDeliveredViaStdin covers
// ptone/scion#2256: several messages coalesced by the debounce buffer into
// one flush must still be delivered as a single ExecWithStdin call carrying
// the full joined payload, even when that combined payload is well over the
// 16 KB tmux argv cap that broke "tmux set-buffer -- <message>".
func TestMessageBuffer_CoalescedLargeMessagesDeliveredViaStdin(t *testing.T) {
	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{
				{ContainerID: "agent-1", Name: "test-agent", Labels: map[string]string{"scion.name": "test-agent"}},
			}, nil
		},
	}

	var mu sync.Mutex
	var stdinPayloads [][]byte
	mockRT.ExecFunc = func(ctx context.Context, id string, cmd []string) (string, error) {
		return "", nil
	}
	mockRT.ExecWithStdinFunc = func(ctx context.Context, id string, cmd []string, stdin io.Reader) (string, error) {
		data, err := io.ReadAll(stdin)
		if err != nil {
			return "", err
		}
		mu.Lock()
		stdinPayloads = append(stdinPayloads, data)
		mu.Unlock()
		return "", nil
	}

	mgr := &AgentManager{Runtime: mockRT}
	mgr.msgBuffer = NewMessageBuffer(100*time.Millisecond, func(agentID, projectID, message string, interrupt bool) error {
		return mgr.deliverImmediate(context.Background(), agentID, projectID, message, interrupt)
	})
	defer mgr.msgBuffer.Close()

	const chunkSize = 6 * 1024
	const numChunks = 4
	var want []string
	for i := 0; i < numChunks; i++ {
		chunk := strings.Repeat(fmt.Sprintf("chunk-%d-", i), chunkSize/8)
		want = append(want, chunk)
		if err := mgr.Message(context.Background(), "test-agent", "", chunk, false); err != nil {
			t.Fatalf("Message %d failed: %v", i, err)
		}
	}
	expected := strings.Join(want, "\n\n")
	if len(expected) <= 16*1024 {
		t.Fatalf("test setup error: combined payload must exceed 16 KiB, got %d bytes", len(expected))
	}

	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		n := len(stdinPayloads)
		mu.Unlock()
		if n >= 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for the coalesced delivery")
		case <-time.After(10 * time.Millisecond):
		}
	}

	// Give any unexpected extra deliveries a moment to arrive before asserting.
	time.Sleep(150 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(stdinPayloads) != 1 {
		t.Fatalf("expected exactly one delivery for the coalesced batch, got %d", len(stdinPayloads))
	}
	if string(stdinPayloads[0]) != expected {
		t.Fatalf("stdin payload mismatch: got %d bytes, want %d bytes", len(stdinPayloads[0]), len(expected))
	}
}

// msgBufferNameRE matches nextMsgBufferName's format: the fixed prefix, a
// 64-bit per-process nonce as 16 lowercase hex digits, and a decimal
// per-delivery sequence number.
var msgBufferNameRE = regexp.MustCompile(`^scion-msg-[0-9a-f]{16}-[0-9]+$`)

// TestNextMsgBufferName_CarriesProcessNonce covers ptone/scion#2265's
// cross-process gap: a process-local counter alone restarts at 1 in every
// new process, so two short-lived CLI invocations (or a CLI call racing the
// broker) against the same agent would otherwise both name their first
// delivery "scion-msg-1". Every name nextMsgBufferName produces must carry
// this process's random nonce, and that nonce must stay the same across
// calls within the process (it's generated once, not per call).
func TestNextMsgBufferName_CarriesProcessNonce(t *testing.T) {
	first := nextMsgBufferName()
	second := nextMsgBufferName()

	for _, name := range []string{first, second} {
		if !msgBufferNameRE.MatchString(name) {
			t.Errorf("buffer name %q does not match the expected %s-<16 hex>-<seq> format", name, msgBufferPrefix)
		}
	}

	nonceOf := func(name string) string {
		parts := strings.Split(name, "-")
		if len(parts) < 3 {
			t.Fatalf("buffer name %q has too few components to extract a nonce", name)
		}
		return parts[len(parts)-2]
	}
	firstNonce, secondNonce := nonceOf(first), nonceOf(second)
	if firstNonce != secondNonce {
		t.Errorf("expected the same process nonce on every call, got %q then %q", firstNonce, secondNonce)
	}
	if firstNonce != msgBufferNonce() {
		t.Errorf("buffer name nonce %q did not match msgBufferNonce() %q", firstNonce, msgBufferNonce())
	}

	if first == second {
		t.Errorf("expected distinct names for two calls, both got %q", first)
	}
}

// TestDeliverImmediate_ConcurrentDeliveriesUseDistinctBufferNames covers
// ptone/scion#2265: deliveries to the same agent are not serialised (an
// interrupt can race a buffered flush), so two deliveries overlapping in time
// must never share a tmux buffer name. A shared fixed name would let one
// delivery's paste consume the buffer loaded for the other.
func TestDeliverImmediate_ConcurrentDeliveriesUseDistinctBufferNames(t *testing.T) {
	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{
				{ContainerID: "agent-1", Name: "test-agent", Labels: map[string]string{"scion.name": "test-agent"}},
			}, nil
		},
	}

	var mu sync.Mutex
	var loadBufNames []string
	var pasteBufNames []string
	var parseErrs []error

	start := make(chan struct{})
	mockRT.ExecWithStdinFunc = func(ctx context.Context, id string, cmd []string, stdin io.Reader) (string, error) {
		<-start // release both goroutines' load-buffer calls together
		// This runs on the delivery goroutines below, not the test goroutine,
		// so a parse failure is recorded rather than reported directly:
		// t.Fatalf (via bufNameFromLoadCmd) is only safe to call from the
		// test's own goroutine.
		bufName, err := parseBufNameFromLoadCmd(strings.Join(cmd, " "))
		mu.Lock()
		if err != nil {
			parseErrs = append(parseErrs, err)
		} else {
			loadBufNames = append(loadBufNames, bufName)
		}
		mu.Unlock()
		// Give the other goroutine a chance to be mid-delivery too, mirroring
		// an interleaved A.load, B.load, A.paste, B.paste.
		time.Sleep(5 * time.Millisecond)
		return "", nil
	}
	mockRT.ExecFunc = func(ctx context.Context, id string, cmd []string) (string, error) {
		if len(cmd) >= 2 && cmd[1] == "paste-buffer" {
			for i, arg := range cmd {
				if arg == "-b" && i+1 < len(cmd) {
					mu.Lock()
					pasteBufNames = append(pasteBufNames, cmd[i+1])
					mu.Unlock()
				}
			}
		}
		return "", nil
	}

	mgr := &AgentManager{Runtime: mockRT}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		errs[0] = mgr.deliverImmediate(context.Background(), "test-agent", "", "message A", true)
	}()
	go func() {
		defer wg.Done()
		errs[1] = mgr.deliverImmediate(context.Background(), "test-agent", "", "message B", false)
	}()
	close(start)
	wg.Wait()

	for _, err := range parseErrs {
		t.Error(err)
	}
	if t.Failed() {
		t.FailNow()
	}

	for i, err := range errs {
		if err != nil {
			t.Fatalf("delivery %d failed: %v", i, err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(loadBufNames) != 2 {
		t.Fatalf("expected 2 load-buffer calls, got %d: %v", len(loadBufNames), loadBufNames)
	}
	if loadBufNames[0] == loadBufNames[1] {
		t.Fatalf("expected distinct buffer names for two interleaved deliveries, both got %q", loadBufNames[0])
	}
	if len(pasteBufNames) != 2 {
		t.Fatalf("expected 2 paste-buffer calls, got %d: %v", len(pasteBufNames), pasteBufNames)
	}
	if pasteBufNames[0] == pasteBufNames[1] {
		t.Fatalf("expected the two pastes to use distinct buffer names, both got %q", pasteBufNames[0])
	}
	wantSet := map[string]bool{loadBufNames[0]: true, loadBufNames[1]: true}
	for _, b := range pasteBufNames {
		if !wantSet[b] {
			t.Errorf("paste-buffer referenced a buffer name %q that was never loaded", b)
		}
	}
}

// TestDeliverImmediate_PasteFailureDeletesBuffer covers ptone/scion#2265:
// paste-buffer's own "-d" only deletes the buffer when the paste succeeds, so
// a failed paste must trigger an explicit, best-effort
// "tmux delete-buffer -b <name>" with the same name that was loaded — and its
// own failure must not change the error reported for the delivery. The
// caller's ctx is already cancelled when this happens (a plausible reason
// paste-buffer itself failed), so the cleanup call must still run: it needs
// its own ctx, detached from the caller's.
func TestDeliverImmediate_PasteFailureDeletesBuffer(t *testing.T) {
	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{
				{ContainerID: "agent-1", Name: "test-agent", Labels: map[string]string{"scion.name": "test-agent"}},
			}, nil
		},
	}

	var loadedBufName string
	var deleteCalls []string
	var deleteCtxErrs []error
	mockRT.ExecWithStdinFunc = func(ctx context.Context, id string, cmd []string, stdin io.Reader) (string, error) {
		loadedBufName = bufNameFromLoadCmd(t, strings.Join(cmd, " "))
		return "", nil
	}
	mockRT.ExecFunc = func(ctx context.Context, id string, cmd []string) (string, error) {
		switch {
		case len(cmd) >= 2 && cmd[1] == "paste-buffer":
			return "", fmt.Errorf("no buffer %s", loadedBufName)
		case len(cmd) >= 2 && cmd[1] == "delete-buffer":
			deleteCalls = append(deleteCalls, strings.Join(cmd, " "))
			deleteCtxErrs = append(deleteCtxErrs, ctx.Err())
			// delete-buffer's own failure must be tolerated (best-effort).
			return "", fmt.Errorf("delete-buffer also failed")
		default:
			return "", nil
		}
	}

	mgr := &AgentManager{Runtime: mockRT}
	// The caller's ctx is already cancelled before delivery starts, standing
	// in for the caller cancelling mid-delivery: the cleanup call below must
	// not inherit that cancellation.
	callerCtx, cancel := context.WithCancel(context.Background())
	cancel()
	err := mgr.deliverImmediate(callerCtx, "test-agent", "", "hello", false)
	if err == nil {
		t.Fatal("expected an error from the failed paste-buffer")
	}
	if strings.Contains(err.Error(), "delete-buffer") {
		t.Fatalf("delete-buffer's own failure must be ignored, got: %v", err)
	}

	if len(deleteCalls) != 1 {
		t.Fatalf("expected exactly one delete-buffer call, got %d: %v", len(deleteCalls), deleteCalls)
	}
	want := "tmux delete-buffer -b " + loadedBufName
	if deleteCalls[0] != want {
		t.Fatalf("expected delete-buffer to reuse the loaded buffer's name: got %q, want %q", deleteCalls[0], want)
	}
	if deleteCtxErrs[0] != nil {
		t.Fatalf("expected the delete-buffer cleanup to run with a ctx detached from the already-cancelled caller ctx, got: %v", deleteCtxErrs[0])
	}
}

// filterByPrefix returns entries from calls that start with the given prefix.
func filterByPrefix(calls []string, prefix string) []string {
	var result []string
	for _, c := range calls {
		if strings.HasPrefix(c, prefix) {
			result = append(result, c)
		}
	}
	return result
}
