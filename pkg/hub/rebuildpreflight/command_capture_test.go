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

package rebuildpreflight

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

func TestPreflightCommand(t *testing.T) {
	dsn := "postgres://hub:supersecret@db.internal/hub"
	environ := []string{"PATH=/usr/bin", "SCION_PREFLIGHT_DB=old-inherited", "HOME=/home/hub"}
	argv, env := PreflightCommand("/tmp/scion", "broker-1", "/etc/scion/server.yaml", environ, dsn)

	// S34: DSN must never appear in argv.
	for _, a := range argv {
		if strings.Contains(a, dsn) || strings.Contains(a, "supersecret") {
			t.Fatalf("DSN leaked into argv: %v", argv)
		}
	}
	wantArgv := []string{"/tmp/scion", "server", "preflight", "--format", "json", "--colocated-broker-id", "broker-1", "--config", "/etc/scion/server.yaml"}
	if strings.Join(argv, "\x00") != strings.Join(wantArgv, "\x00") {
		t.Errorf("argv = %v, want %v", argv, wantArgv)
	}

	// S66: inherited SCION_PREFLIGHT_DB must be replaced, not kept.
	var found int
	for _, kv := range env {
		if strings.HasPrefix(kv, PreflightDBEnv+"=") {
			found++
			if kv != PreflightDBEnv+"="+dsn {
				t.Errorf("env entry = %q, want %q", kv, PreflightDBEnv+"="+dsn)
			}
		}
	}
	if found != 1 {
		t.Errorf("found %d SCION_PREFLIGHT_DB entries in env, want 1", found)
	}
}

func TestAbsSQLiteDSN_And_AbsCheck(t *testing.T) {
	cwd := "/var/lib/scion"
	cases := []struct {
		in, want string
	}{
		{"hub.db", filepath.Join(cwd, "hub.db")},
		{"file:rel.db?cache=shared", "file:" + filepath.Join(cwd, "rel.db") + "?cache=shared"},
		{":memory:", ":memory:"},
		{"file::memory:?cache=shared", "file::memory:?cache=shared"},
		{"/abs/hub.db", "/abs/hub.db"},
		{"file:/abs/hub.db?mode=ro", "file:/abs/hub.db?mode=ro"},
		{"", ""},
	}
	for _, tc := range cases {
		got, err := AbsSQLiteDSN(tc.in, cwd)
		if err != nil || got != tc.want {
			t.Errorf("AbsSQLiteDSN(%q, %q) = (%q, %v), want %q", tc.in, cwd, got, err, tc.want)
		}
	}

	// S90: AbsCheck refuses relative paths, accepts empty or absolute.
	if err := AbsCheck(""); err != nil {
		t.Errorf("AbsCheck('') = %v", err)
	}
	if err := AbsCheck("/etc/scion/server.yaml"); err != nil {
		t.Errorf("AbsCheck(abs) = %v", err)
	}
	if err := AbsCheck("relative/server.yaml"); err == nil {
		t.Error("S90: AbsCheck accepted relative path")
	}
}

func TestCappedWriter_DrainsPastLimit(t *testing.T) {
	w := NewCappedWriter(1024)
	payload := bytes.Repeat([]byte("a"), 8*1024)
	n, err := w.Write(payload)
	if err != nil || n != len(payload) {
		t.Fatalf("S84: Write = (%d, %v), want (%d, nil)", n, err, len(payload))
	}
	if !w.Oversize() {
		t.Error("expected Oversize() == true")
	}
	if len(w.Bytes()) != 1025 {
		t.Errorf("retained len = %d, want 1025", len(w.Bytes()))
	}
}

func TestCapture_R3_OrderingAndStraddling(t *testing.T) {
	dsn := "postgres://hub:straddle-s3cr3t-99@db.internal/hub"

	t.Run("secret straddling head cut is dropped (kills S58)", func(t *testing.T) {
		c := NewCaptureWithLimits(32, 256, 4096)
		// First line fits in head; second line straddles byte 32 with secret.
		_, _ = c.Write([]byte("line-one\n"))
		_, _ = c.Write([]byte("pad-pad-pad-straddle-s3cr3t-99-tail\n"))
		_, _ = c.Write([]byte("after-line\n"))
		got := c.Redacted("postgres", dsn)
		if strings.Contains(got, "straddle") || strings.Contains(got, "s3cr3t") {
			t.Errorf("S58: secret fragment survived head cut:\n%s", got)
		}
		if !strings.Contains(got, "line-one") || !strings.Contains(got, "after-line") {
			t.Errorf("valid lines lost:\n%s", got)
		}
	})

	t.Run("secret straddling tail ring cut is dropped (kills S58)", func(t *testing.T) {
		c := NewCaptureWithLimits(16, 40, 4096)
		_, _ = c.Write([]byte("head-ok\n"))
		_, _ = c.Write([]byte("middle-padding-line-1234567890\n"))
		_, _ = c.Write([]byte("ring-straddle-s3cr3t-99\n"))
		_, _ = c.Write([]byte("final-ok-line-1234567890\n"))
		got := c.Redacted("postgres", dsn)
		if strings.Contains(got, "s3cr3t") {
			t.Errorf("S58: secret fragment survived ring cut:\n%s", got)
		}
	})

	t.Run("redact happens before log truncation (kills S57)", func(t *testing.T) {
		c := NewCaptureWithLimits(256, 256, 80)
		// Write enough lines to trigger logCap=80 truncation, with secret on head and tail.
		_, _ = c.Write([]byte("Error: " + dsn + "\n"))
		for i := 0; i < 10; i++ {
			_, _ = c.Write([]byte("filler-line-0123456789\n"))
		}
		_, _ = c.Write([]byte("tail: straddle-s3cr3t-99\n"))
		got := c.Redacted("postgres", dsn)
		if strings.Contains(got, "straddle-s3cr3t-99") {
			t.Errorf("S57/S35: secret leaked:\n%s", got)
		}
		if !strings.Contains(got, "bytes omitted") {
			t.Errorf("expected truncation marker in:\n%s", got)
		}
	})

	t.Run("env dump and multi-line password redacted (S3, S70)", func(t *testing.T) {
		mlDSN := "host=db password='secretLineOne\nsecretLineTwo' dbname=hub"
		c := NewCapture()
		_, _ = c.Write([]byte("SCION_PREFLIGHT_DB=" + dsn + "\n"))
		_, _ = c.Write([]byte("pw1=secretLineOne\npw2=secretLineTwo\n"))
		got1 := c.Redacted("postgres", dsn)
		if strings.Contains(got1, "straddle-s3cr3t-99") {
			t.Errorf("env dump secret leaked:\n%s", got1)
		}
		got2 := c.Redacted("postgres", mlDSN)
		if strings.Contains(got2, "secretLineOne") || strings.Contains(got2, "secretLineTwo") {
			t.Errorf("S70: multi-line credential piece leaked:\n%s", got2)
		}
	})
}
