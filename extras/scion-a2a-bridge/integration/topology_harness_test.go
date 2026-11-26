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

package integration_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"testing"
	"time"
)

const (
	helperModeEnv       = "SCION_INTEGRATION_HELPER_MODE"
	helperAddressEnv    = "SCION_INTEGRATION_HELPER_ADDRESS"
	helperReplicaIDEnv  = "SCION_INTEGRATION_HELPER_REPLICA_ID"
	helperListenerFDEnv = "SCION_INTEGRATION_HELPER_LISTENER_FD"
	helperReadyFDEnv    = "SCION_INTEGRATION_HELPER_READY_FD"
	helperReadySignal   = "ready\n"
	helperReadyTimeout  = 15 * time.Second
)

type processSpec struct {
	Name      string
	Mode      string
	ReplicaID string
	Env       map[string]string
}

type testProcess struct {
	Name  string
	PID   int
	Port  int
	Ready bool
	cmd   *exec.Cmd
}

func (p *testProcess) URL() string {
	return fmt.Sprintf("http://127.0.0.1:%d", p.Port)
}

type processTopology struct {
	ctx          context.Context
	cancel       context.CancelFunc
	allocator    *loopbackPortAllocator
	logs         *sanitizedWriter
	observations *observationRecorder
	processes    []*testProcess
	stopOnce     sync.Once
}

func newProcessTopology(t *testing.T, redactor *credentialRedactor) *processTopology {
	t.Helper()
	if redactor == nil {
		redactor = newCredentialRedactor()
	}
	ctx, cancel := context.WithCancel(context.Background())
	topology := &processTopology{
		ctx:          ctx,
		cancel:       cancel,
		allocator:    newLoopbackPortAllocator(),
		logs:         newSanitizedWriter(redactor),
		observations: newObservationRecorder(redactor),
	}
	t.Cleanup(func() { topology.stop(t) })
	return topology
}

func (t *processTopology) start(tb testing.TB, spec processSpec) *testProcess {
	tb.Helper()
	port, err := t.allocator.reserve()
	if err != nil {
		tb.Fatalf("reserve port for %s: %v", spec.Name, err)
	}
	listenerFile, err := t.allocator.listenerFile(port)
	if err != nil {
		tb.Fatalf("inherit listener for %s: %v", spec.Name, err)
	}

	readyRead, readyWrite, err := os.Pipe()
	if err != nil {
		_ = listenerFile.Close()
		tb.Fatalf("create readiness pipe for %s: %v", spec.Name, err)
	}
	defer func() { _ = readyRead.Close() }()

	cmd := exec.CommandContext(t.ctx, os.Args[0], "-test.run=^TestHarnessHelperProcess$", "-test.v")
	cmd.ExtraFiles = []*os.File{listenerFile, readyWrite}
	cmd.Env = append(os.Environ(),
		helperModeEnv+"="+spec.Mode,
		helperAddressEnv+"=127.0.0.1:"+strconv.Itoa(port),
		helperReplicaIDEnv+"="+spec.ReplicaID,
		helperListenerFDEnv+"=3",
		helperReadyFDEnv+"=4",
	)
	for key, value := range spec.Env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	cmd.Stdout = t.logs
	cmd.Stderr = t.logs
	if err := cmd.Start(); err != nil {
		_ = listenerFile.Close()
		_ = readyWrite.Close()
		tb.Fatalf("start %s: %v", spec.Name, err)
	}
	if err := listenerFile.Close(); err != nil {
		_ = readyWrite.Close()
		tb.Fatalf("close inherited-listener copy for %s: %v", spec.Name, err)
	}
	if err := readyWrite.Close(); err != nil {
		tb.Fatalf("close parent readiness pipe write end for %s: %v", spec.Name, err)
	}
	if err := t.allocator.release(port); err != nil {
		tb.Fatalf("release parent listener for %s: %v", spec.Name, err)
	}
	process := &testProcess{Name: spec.Name, PID: cmd.Process.Pid, Port: port, cmd: cmd}
	t.processes = append(t.processes, process)
	if err := waitForHelperReady(t.ctx, readyRead, helperReadyTimeout); err != nil {
		tb.Fatalf("wait for %s readiness (PID %d): %v\nlogs:\n%s", spec.Name, process.PID, err, t.logs.String())
	}
	process.Ready = true
	tb.Logf("helper ready name=%s pid=%d port=%d", process.Name, process.PID, process.Port)
	t.observations.record(observation{ReplicaID: spec.ReplicaID, Outcome: "ready"})
	return process
}

func (t *processTopology) stopProcess(tb testing.TB, process *testProcess) {
	tb.Helper()
	if process.cmd.ProcessState != nil {
		return
	}
	if err := process.cmd.Process.Kill(); err != nil {
		tb.Fatalf("kill %s (PID %d): %v", process.Name, process.PID, err)
	}
	if err := process.cmd.Wait(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			tb.Fatalf("wait for %s (PID %d): %v", process.Name, process.PID, err)
		}
	}
}

func (t *processTopology) stop(tb testing.TB) {
	tb.Helper()
	t.stopOnce.Do(func() {
		t.cancel()
		for _, process := range t.processes {
			if err := process.cmd.Wait(); err != nil && process.cmd.ProcessState == nil {
				tb.Errorf("wait for %s (PID %d): %v", process.Name, process.PID, err)
			}
			tb.Logf("helper stopped name=%s pid=%d port=%d reaped=%t", process.Name, process.PID, process.Port, process.cmd.ProcessState != nil)
			t.observations.record(observation{ReplicaID: process.Name, Outcome: "stopped"})
		}
		if err := t.allocator.close(); err != nil {
			tb.Errorf("close port allocator: %v", err)
		}
	})
}

func waitForHelperReady(ctx context.Context, readyPipe *os.File, timeout time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	if err := readyPipe.SetReadDeadline(deadline); err != nil {
		return fmt.Errorf("set readiness deadline: %w", err)
	}
	stopCancel := context.AfterFunc(ctx, func() {
		_ = readyPipe.SetReadDeadline(time.Now())
	})
	defer stopCancel()

	var buf [len(helperReadySignal)]byte
	if _, err := io.ReadFull(readyPipe, buf[:]); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, os.ErrDeadlineExceeded) {
			return fmt.Errorf("timed out after %s waiting for helper readiness", timeout)
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return fmt.Errorf("helper exited before signaling readiness: %w", err)
		}
		return fmt.Errorf("read helper readiness signal: %w", err)
	}
	if string(buf[:]) != helperReadySignal {
		return fmt.Errorf("unexpected helper readiness signal %q", string(buf[:]))
	}
	return nil
}

type loopbackPortAllocator struct {
	mu           sync.Mutex
	reservations map[int]net.Listener
}

func newLoopbackPortAllocator() *loopbackPortAllocator {
	return &loopbackPortAllocator{reservations: make(map[int]net.Listener)}
}

func (a *loopbackPortAllocator) reserve() (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("reserve ephemeral loopback port: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	a.reservations[port] = listener
	return port, nil
}

func (a *loopbackPortAllocator) listenerFile(port int) (*os.File, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	listener, ok := a.reservations[port]
	if !ok {
		return nil, fmt.Errorf("port %d is not reserved", port)
	}
	tcpListener, ok := listener.(*net.TCPListener)
	if !ok {
		return nil, fmt.Errorf("port %d listener has type %T, want TCP", port, listener)
	}
	file, err := tcpListener.File()
	if err != nil {
		return nil, fmt.Errorf("duplicate listener for port %d: %w", port, err)
	}
	return file, nil
}

func (a *loopbackPortAllocator) release(port int) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	listener, ok := a.reservations[port]
	if !ok {
		return fmt.Errorf("port %d is not reserved", port)
	}
	delete(a.reservations, port)
	return listener.Close()
}

func (a *loopbackPortAllocator) close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	var firstErr error
	for port, listener := range a.reservations {
		if err := listener.Close(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("close reserved port %d: %w", port, err)
		}
		delete(a.reservations, port)
	}
	return firstErr
}
