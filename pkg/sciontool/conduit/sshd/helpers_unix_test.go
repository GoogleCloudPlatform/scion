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

//go:build unix

package sshd

import (
	"context"
	"net"
	"os"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"
)

// TestMain lets the test binary act as the sciontool helper: the server
// under test runs os.Args[0] with HelperCommand, as sciontool does.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == HelperCommand {
		os.Exit(RunHelper(os.Args[2:], os.Stdin, os.Stdout))
	}
	os.Exit(m.Run())
}

const waitTimeout = 10 * time.Second

// connPair returns two connected sockets (no listener involved).
func connPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	conns := make([]net.Conn, 2)
	for i, fd := range fds {
		f := os.NewFile(uintptr(fd), "pair")
		c, err := net.FileConn(f)
		_ = f.Close()
		if err != nil {
			t.Fatal(err)
		}
		conns[i] = c
	}
	return conns[0], conns[1]
}

// testConfig is a server configuration for tests: a fresh host key,
// /bin/sh, the test binary as the helper.
func testConfig(t *testing.T) *Config {
	t.Helper()
	key, err := NewHostKey()
	if err != nil {
		t.Fatal(err)
	}
	return &Config{HostKey: key, Shell: "/bin/sh", HelperPath: os.Args[0]}
}

// serve runs cfg over one end of a socket pair and returns an SSH client
// on the other, logged in as user. The test's cleanup closes the client
// and waits for Serve to return.
func serve(t *testing.T, cfg *Config, user string) *ssh.Client {
	t.Helper()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	srvConn, cliConn := connPair(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- cfg.Serve(ctx, srvConn) }()
	cc, chans, reqs, err := ssh.NewClientConn(cliConn, "agent", &ssh.ClientConfig{
		User:            user,
		HostKeyCallback: ssh.FixedHostKey(cfg.HostKey.PublicKey()),
		Timeout:         waitTimeout,
	})
	if err != nil {
		cancel()
		t.Fatalf("ssh handshake: %v", err)
	}
	client := ssh.NewClient(cc, chans, reqs)
	t.Cleanup(func() {
		_ = client.Close()
		cancel()
		select {
		case <-done:
		case <-time.After(waitTimeout):
			t.Error("Serve did not return")
		}
	})
	return client
}

// waitFor polls cond until it holds or the timeout passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
