/*
Copyright 2026 The Scion Authors.
*/

package log

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// tokyo is a fixed +09:00 zone, so these tests do not depend on the host
// having tzdata installed.
var tokyo = time.FixedZone("JST", 9*60*60)

// withLocal sets time.Local for the duration of a test, standing in for an
// agent container started with TZ=Asia/Tokyo.
func withLocal(t *testing.T, loc *time.Location) {
	t.Helper()
	orig := time.Local
	time.Local = loc
	t.Cleanup(func() { time.Local = orig })
}

func TestTimestamp_IsUTCRFC3339Nano(t *testing.T) {
	in := time.Date(2026, 10, 2, 9, 30, 15, 123456789, tokyo)
	got := Timestamp(in)
	want := "2026-10-02T00:30:15.123456789Z"
	if got != want {
		t.Errorf("Timestamp(%v) = %q, want %q", in, got, want)
	}
}

// TestWrite_AgentLogLineIsUTC writes a line through the public logger with
// the process zone set to Tokyo and checks that agent.log carries a UTC
// RFC 3339 timestamp for the same instant.
func TestWrite_AgentLogLineIsUTC(t *testing.T) {
	withLocal(t, tokyo)
	path := filepath.Join(t.TempDir(), "agent.log")
	defer setLogPathForTest(t, path)()
	origQuiet := quiet.Load()
	SetQuiet(true)
	defer SetQuiet(origQuiet)

	before := time.Now()
	Info("hello %s", "world")
	after := time.Now()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read agent.log: %v", err)
	}
	line := strings.TrimSuffix(string(data), "\n")
	ts, rest, ok := strings.Cut(line, " ")
	if !ok {
		t.Fatalf("agent.log line has no timestamp field: %q", line)
	}
	if rest != "[sciontool] [INFO] hello world" {
		t.Errorf("agent.log line body = %q, want %q", rest, "[sciontool] [INFO] hello world")
	}
	if !strings.HasSuffix(ts, "Z") {
		t.Errorf("agent.log timestamp %q is not UTC (want a trailing Z)", ts)
	}
	parsed, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		t.Fatalf("agent.log timestamp %q is not RFC 3339: %v", ts, err)
	}
	if parsed.Before(before) || parsed.After(after) {
		t.Errorf("agent.log timestamp %v is outside the write window [%v, %v]", parsed, before.UTC(), after.UTC())
	}
}
