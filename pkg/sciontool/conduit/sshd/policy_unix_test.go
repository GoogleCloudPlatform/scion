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
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/conduit/localpolicy"
)

func policyConfig(t *testing.T, raw string) *Config {
	t.Helper()
	p, err := localpolicy.ParsePolicy(raw)
	if err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t)
	cfg.Policy = p
	return cfg
}

// TestDirectTCPIPPolicy: a port on the operator deny-list is refused as
// prohibited without a dial; other loopback ports are still forwarded.
func TestDirectTCPIPPolicy(t *testing.T) {
	allowed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	echoServer(t, allowed)
	deniedL, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	echoServer(t, deniedL)
	denied := strconv.Itoa(deniedL.Addr().(*net.TCPAddr).Port)
	d := &recordingDialer{}
	cfg := policyConfig(t, `{"deny_ports":[`+denied+`]}`)
	cfg.DialTCP = d.dial
	client := serve(t, cfg, "scion")

	c, err := client.Dial("tcp", allowed.Addr().String())
	if err != nil {
		t.Fatalf("direct-tcpip to an allowed port refused: %v", err)
	}
	roundTrip(t, c, "allowed")
	_ = c.Close()
	for _, host := range []string{"127.0.0.1", "localhost", "::1"} {
		_, err := client.Dial("tcp", net.JoinHostPort(host, denied))
		wantOpenRefused(t, err, ssh.Prohibited)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.addrs) != 1 {
		t.Fatalf("dialed %v, want only the allowed port", d.addrs)
	}
}

// TestStreamlocalPolicy: a denied socket, a socket below a denied
// directory and a symbolic link to a denied socket are refused as
// prohibited; a socket elsewhere is forwarded.
func TestStreamlocalPolicy(t *testing.T) {
	dir := shortTempDir(t)
	listen := func(path string) {
		t.Helper()
		l, err := net.Listen("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		echoServer(t, l)
	}
	if err := os.Mkdir(filepath.Join(dir, "priv"), 0o755); err != nil {
		t.Fatal(err)
	}
	ok := filepath.Join(dir, "ok.sock")
	denied := filepath.Join(dir, "d.sock")
	inDir := filepath.Join(dir, "priv", "p.sock")
	link := filepath.Join(dir, "l.sock")
	for _, p := range []string{ok, denied, inDir} {
		listen(p)
	}
	if err := os.Symlink(denied, link); err != nil {
		t.Fatal(err)
	}
	// The temp dir may itself sit behind a symbolic link (macOS /var).
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := policyConfig(t, `{"deny_sockets":["`+filepath.Join(real, "d.sock")+`","`+filepath.Join(real, "priv")+`","`+denied+`","`+filepath.Join(dir, "priv")+`"]}`)
	client := serve(t, cfg, "scion")

	c, err := client.Dial("unix", ok)
	if err != nil {
		t.Fatalf("direct-streamlocal to an allowed socket refused: %v", err)
	}
	roundTrip(t, c, "allowed")
	_ = c.Close()
	for _, p := range []string{denied, inDir, link} {
		_, err := client.Dial("unix", p)
		wantOpenRefused(t, err, ssh.Prohibited)
	}
}
