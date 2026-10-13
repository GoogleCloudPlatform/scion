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

// TestStreamlocalPolicy: covers paths below entries and links in either side.
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
	for _, d := range []string{"priv", "real"} {
		if err := os.Mkdir(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	ok := filepath.Join(dir, "ok.sock")
	denied := filepath.Join(dir, "d.sock")
	inDir := filepath.Join(dir, "priv", "p.sock")
	aliased := filepath.Join(dir, "real", "a.sock")
	for _, p := range []string{ok, denied, inDir, aliased} {
		listen(p)
	}
	links := map[string]string{
		filepath.Join(dir, "l.sock"):  denied,
		filepath.Join(dir, "dirlink"): filepath.Join(dir, "priv"),
		filepath.Join(dir, "alias"):   filepath.Join(dir, "real"),
	}
	for link, target := range links {
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	cfg := policyConfig(t, `{"deny_sockets":["`+denied+`","`+filepath.Join(dir, "priv")+`","`+filepath.Join(dir, "alias", "a.sock")+`"]}`)
	client := serve(t, cfg, "scion")

	c, err := client.Dial("unix", ok)
	if err != nil {
		t.Fatalf("direct-streamlocal to an allowed socket refused: %v", err)
	}
	roundTrip(t, c, "allowed")
	_ = c.Close()
	for _, p := range []string{denied, inDir, filepath.Join(dir, "l.sock"), filepath.Join(dir, "dirlink", "p.sock"), aliased} {
		_, err := client.Dial("unix", p)
		wantOpenRefused(t, err, ssh.Prohibited)
	}
}
