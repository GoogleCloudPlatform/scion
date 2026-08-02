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

package autoexpose

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// Sample /proc/net/tcp content with various socket states.
const sampleProcNetTCP = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 12345 1 0000000000000000 100 0 0 10 0
   1: 00000000:0BB8 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 12346 1 0000000000000000 100 0 0 10 0
   2: 0100007F:47CC 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 12347 1 0000000000000000 100 0 0 10 0
   3: 0100007F:C350 01020304:0050 01 00000000:00000000 02:00000000 00000000  1000        0 12348 1 0000000000000000 100 0 0 10 0
   4: 00000000:1388 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 12349 1 0000000000000000 100 0 0 10 0
`

// Sample /proc/net/tcp6 content.
const sampleProcNetTCP6 = `  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000000000000000000000000000:1F90 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 23456 1 0000000000000000 100 0 0 10 0
   1: 00000000000000000000000001000000:1F91 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 23457 1 0000000000000000 100 0 0 10 0
   2: 00000000000000000000000000000000:0BB8 00000000000000000000000001000000:1F90 01 00000000:00000000 02:00000000 00000000  1000        0 23458 1 0000000000000000 100 0 0 10 0
`

func TestParseProcNetTCPData_IPv4(t *testing.T) {
	sockets, err := parseProcNetTCPData(sampleProcNetTCP, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should find exactly 4 LISTEN sockets (state 0A), not the ESTABLISHED one (state 01).
	if len(sockets) != 4 {
		t.Fatalf("expected 4 LISTEN sockets, got %d", len(sockets))
	}

	// Sort by port for deterministic comparison.
	sort.Slice(sockets, func(i, j int) bool { return sockets[i].Port < sockets[j].Port })

	tests := []struct {
		port     int
		bindAddr string
	}{
		{3000, "0.0.0.0"},    // 0x0BB8 = 3000, 00000000 = 0.0.0.0
		{5000, "0.0.0.0"},    // 0x1388 = 5000, 00000000 = 0.0.0.0
		{8080, "127.0.0.1"},  // 0x1F90 = 8080, 0100007F = 127.0.0.1
		{18380, "127.0.0.1"}, // 0x47CC = 18380, 0100007F = 127.0.0.1
	}

	for i, tt := range tests {
		if sockets[i].Port != tt.port {
			t.Errorf("socket[%d].Port = %d, want %d", i, sockets[i].Port, tt.port)
		}
		if sockets[i].BindAddr != tt.bindAddr {
			t.Errorf("socket[%d].BindAddr = %q, want %q", i, sockets[i].BindAddr, tt.bindAddr)
		}
	}
}

func TestParseProcNetTCPData_IPv6(t *testing.T) {
	sockets, err := parseProcNetTCPData(sampleProcNetTCP6, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should find 2 LISTEN sockets (not the ESTABLISHED one).
	if len(sockets) != 2 {
		t.Fatalf("expected 2 LISTEN sockets, got %d", len(sockets))
	}

	sort.Slice(sockets, func(i, j int) bool { return sockets[i].Port < sockets[j].Port })

	// Port 8080 (0x1F90) on :: (all zeros)
	if sockets[0].Port != 8080 {
		t.Errorf("socket[0].Port = %d, want 8080", sockets[0].Port)
	}
	if sockets[0].BindAddr != "::" {
		t.Errorf("socket[0].BindAddr = %q, want %q", sockets[0].BindAddr, "::")
	}

	// Port 8081 (0x1F91) on ::1
	if sockets[1].Port != 8081 {
		t.Errorf("socket[1].Port = %d, want 8081", sockets[1].Port)
	}
	if sockets[1].BindAddr != "::1" {
		t.Errorf("socket[1].BindAddr = %q, want %q", sockets[1].BindAddr, "::1")
	}
}

func TestParseProcNetTCPData_EmptyFile(t *testing.T) {
	sockets, err := parseProcNetTCPData("", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(sockets) != 0 {
		t.Fatalf("expected 0 sockets from empty data, got %d", len(sockets))
	}
}

func TestParseProcNetTCPData_HeaderOnly(t *testing.T) {
	data := "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"
	sockets, err := parseProcNetTCPData(data, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(sockets) != 0 {
		t.Fatalf("expected 0 sockets from header-only data, got %d", len(sockets))
	}
}

func TestParseProcNetTCPData_MalformedLines(t *testing.T) {
	data := `  sl  local_address rem_address   st
   0: BADDATA 0A
   1: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 12345
   2: short
`
	sockets, err := parseProcNetTCPData(data, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Only the well-formed LISTEN line should parse.
	if len(sockets) != 1 {
		t.Fatalf("expected 1 socket from mixed data, got %d", len(sockets))
	}
	if sockets[0].Port != 8080 {
		t.Errorf("port = %d, want 8080", sockets[0].Port)
	}
}

func TestParseProcNetTCPData_NoListenSockets(t *testing.T) {
	// All sockets are ESTABLISHED (state 01).
	data := `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:1F90 01020304:0050 01 00000000:00000000 02:00000000 00000000  1000        0 12345 1 0000000000000000 100 0 0 10 0
   1: 0100007F:C350 01020304:0051 01 00000000:00000000 02:00000000 00000000  1000        0 12346 1 0000000000000000 100 0 0 10 0
`
	sockets, err := parseProcNetTCPData(data, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(sockets) != 0 {
		t.Fatalf("expected 0 LISTEN sockets, got %d", len(sockets))
	}
}

func TestScanListenersFrom_BothFiles(t *testing.T) {
	dir := t.TempDir()

	tcpPath := filepath.Join(dir, "tcp")
	tcp6Path := filepath.Join(dir, "tcp6")

	if err := os.WriteFile(tcpPath, []byte(sampleProcNetTCP), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tcp6Path, []byte(sampleProcNetTCP6), 0644); err != nil {
		t.Fatal(err)
	}

	sockets, err := scanListenersFrom(tcpPath, tcp6Path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// 4 from tcp + 2 from tcp6 = 6 total.
	if len(sockets) != 6 {
		t.Errorf("expected 6 total sockets, got %d", len(sockets))
	}
}

func TestScanListenersFrom_MissingTCP6(t *testing.T) {
	dir := t.TempDir()

	tcpPath := filepath.Join(dir, "tcp")
	tcp6Path := filepath.Join(dir, "tcp6_missing")

	if err := os.WriteFile(tcpPath, []byte(sampleProcNetTCP), 0644); err != nil {
		t.Fatal(err)
	}

	// tcp6 doesn't exist — should succeed with only tcp results.
	sockets, err := scanListenersFrom(tcpPath, tcp6Path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(sockets) != 4 {
		t.Errorf("expected 4 sockets from tcp only, got %d", len(sockets))
	}
}

func TestScanListenersFrom_TCPReadError(t *testing.T) {
	dir := t.TempDir()

	// tcp file does not exist — should return an error.
	tcpPath := filepath.Join(dir, "tcp_missing")
	tcp6Path := filepath.Join(dir, "tcp6")

	if err := os.WriteFile(tcp6Path, []byte(sampleProcNetTCP6), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := scanListenersFrom(tcpPath, tcp6Path)
	if err == nil {
		t.Fatal("expected error when /proc/net/tcp is unreadable, got nil")
	}
}

func TestParseHexIPv4(t *testing.T) {
	tests := []struct {
		hex  string
		want string
	}{
		{"00000000", "0.0.0.0"},
		{"0100007F", "127.0.0.1"},
		{"0101A8C0", "192.168.1.1"},
	}

	for _, tt := range tests {
		got, err := parseHexIPv4(tt.hex)
		if err != nil {
			t.Errorf("parseHexIPv4(%q) error: %v", tt.hex, err)
			continue
		}
		if got != tt.want {
			t.Errorf("parseHexIPv4(%q) = %q, want %q", tt.hex, got, tt.want)
		}
	}
}

func TestParseHexIPv6_WellKnown(t *testing.T) {
	tests := []struct {
		hex  string
		want string
	}{
		{"00000000000000000000000000000000", "::"},
		{"00000000000000000000000001000000", "::1"},
		{"0000000000000000FFFF00000100007F", "127.0.0.1"}, // IPv4-mapped ::ffff:127.0.0.1
	}

	for _, tt := range tests {
		got, err := parseHexIPv6(tt.hex)
		if err != nil {
			t.Errorf("parseHexIPv6(%q) error: %v", tt.hex, err)
			continue
		}
		if got != tt.want {
			t.Errorf("parseHexIPv6(%q) = %q, want %q", tt.hex, got, tt.want)
		}
	}
}
