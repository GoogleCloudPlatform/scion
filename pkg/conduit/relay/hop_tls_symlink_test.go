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

//go:build !no_sqlite && !windows

package relay_test

import (
	"bytes"
	"crypto/tls"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay/relaytest"
)

// The Kubernetes Secret-volume rotation needs symlinks, so this test does not
// build on Windows; TestHopServerTLSReloadsRewrittenCertificate covers reload
// on every platform.

// TestHopServerTLSReloadsRotatedCertificate: the internal listener picks
// up a certificate rotated through a Kubernetes-style symlink swap
// (tls.crt -> ..data/tls.crt, ..data -> a new directory) at the next
// check after HopCertReloadInterval, and keeps serving the current
// certificate when the new pair does not load.
func TestHopServerTLSReloadsRotatedCertificate(t *testing.T) {
	dir := t.TempDir()
	gen := func(name string) string {
		f := relaytest.NewHopTLSFiles(t, hopServerName)
		d := filepath.Join(dir, name)
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
		for src, dst := range map[string]string{f.CertFile: "tls.crt", f.KeyFile: "tls.key"} {
			b, err := os.ReadFile(src)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(d, dst), b, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return name
	}
	swap := func(target string) {
		tmp := filepath.Join(dir, "..data_tmp")
		if err := os.Symlink(target, tmp); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, filepath.Join(dir, "..data")); err != nil {
			t.Fatal(err)
		}
	}
	swap(gen("gen1"))
	for _, f := range []string{"tls.crt", "tls.key"} {
		if err := os.Symlink(filepath.Join("..data", f), filepath.Join(dir, f)); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	clock := func() time.Time { return now }
	cfg, err := relay.NewHopServerTLSWithClockForTest(filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"), clock)
	if err != nil {
		t.Fatal(err)
	}
	leaf := func() []byte {
		c, err := cfg.GetCertificate(&tls.ClientHelloInfo{})
		if err != nil {
			t.Fatal(err)
		}
		return c.Certificate[0]
	}
	first := leaf()

	swap(gen("gen2"))
	if !bytes.Equal(leaf(), first) {
		t.Fatal("the certificate changed before the reload interval")
	}
	now = now.Add(relay.HopCertReloadInterval)
	second := leaf()
	if bytes.Equal(second, first) {
		t.Fatal("the rotated certificate was not picked up after the reload interval")
	}

	// A broken rotation keeps the current certificate.
	broken := filepath.Join(dir, "gen3")
	if err := os.Mkdir(broken, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"tls.crt", "tls.key"} {
		if err := os.WriteFile(filepath.Join(broken, f), []byte("not pem"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	swap("gen3")
	now = now.Add(relay.HopCertReloadInterval)
	if !bytes.Equal(leaf(), second) {
		t.Fatal("a pair that does not load replaced the current certificate")
	}
}
