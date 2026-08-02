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

// Package autoexpose detects TCP listening ports inside agent containers by
// parsing /proc/net/tcp{,6} and automatically exposes them through the Hub.
package autoexpose

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// tcpListen is the hex state code for TCP_LISTEN in /proc/net/tcp.
const tcpListen = "0A"

// ListenSocket represents a TCP socket in the LISTEN state.
type ListenSocket struct {
	Port     int
	BindAddr string // e.g. "0.0.0.0", "127.0.0.1", "::", "::1"
}

// ScanListeners reads /proc/net/tcp and /proc/net/tcp6, returning all
// sockets in the TCP_LISTEN state. The function is pure Go, no subprocess.
func ScanListeners() ([]ListenSocket, error) {
	return scanListenersFrom("/proc/net/tcp", "/proc/net/tcp6")
}

// scanListenersFrom is the testable core — reads the given paths instead of
// the real procfs files.
func scanListenersFrom(tcpPath, tcp6Path string) ([]ListenSocket, error) {
	var result []ListenSocket

	sockets, err := parseProcNetTCP(tcpPath, false)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", tcpPath, err)
	}
	result = append(result, sockets...)

	// tcp6 may not exist; ignore read errors.
	if sockets, err := parseProcNetTCP(tcp6Path, true); err == nil {
		result = append(result, sockets...)
	}

	return result, nil
}

// parseProcNetTCP parses a /proc/net/tcp or /proc/net/tcp6 file, returning
// only LISTEN sockets.
func parseProcNetTCP(path string, ipv6 bool) ([]ListenSocket, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseProcNetTCPData(string(data), ipv6)
}

// parseProcNetTCPData parses the content of a /proc/net/tcp{,6} file.
// Exported for testing.
func parseProcNetTCPData(data string, ipv6 bool) ([]ListenSocket, error) {
	var result []ListenSocket

	lines := strings.Split(strings.TrimSpace(data), "\n")
	if len(lines) < 2 {
		return nil, nil // header only or empty
	}

	// Skip the header line (line 0).
	for _, line := range lines[1:] {
		fields := strings.Fields(line)
		// Need at least 4 fields: sl, local_address, rem_address, st
		if len(fields) < 4 {
			continue
		}

		state := fields[3]
		if state != tcpListen {
			continue
		}

		// local_address is "hex_ip:hex_port"
		localAddr := fields[1]
		port, bindAddr, err := parseLocalAddress(localAddr, ipv6)
		if err != nil {
			continue // skip malformed lines
		}

		result = append(result, ListenSocket{
			Port:     port,
			BindAddr: bindAddr,
		})
	}

	return result, nil
}

// parseLocalAddress parses "HEXIP:HEXPORT" from /proc/net/tcp{,6}.
func parseLocalAddress(s string, ipv6 bool) (port int, bindAddr string, err error) {
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return 0, "", fmt.Errorf("invalid local_address: %q", s)
	}

	hexIP := parts[0]
	hexPort := parts[1]

	// Parse port (always 4 hex digits).
	portVal, err := strconv.ParseUint(hexPort, 16, 16)
	if err != nil {
		return 0, "", fmt.Errorf("invalid port hex %q: %w", hexPort, err)
	}
	port = int(portVal)

	// Parse IP address.
	addr, err := parseHexIP(hexIP, ipv6)
	if err != nil {
		return 0, "", err
	}

	return port, addr, nil
}

// parseHexIP converts a hex-encoded IP from /proc/net/tcp to a human-readable string.
// IPv4: 8 hex chars, little-endian 32-bit integer.
// IPv6: 32 hex chars, four little-endian 32-bit groups.
func parseHexIP(hex string, ipv6 bool) (string, error) {
	if ipv6 {
		return parseHexIPv6(hex)
	}
	return parseHexIPv4(hex)
}

func parseHexIPv4(hex string) (string, error) {
	if len(hex) != 8 {
		return "", fmt.Errorf("invalid IPv4 hex length: %d", len(hex))
	}
	var ip [4]byte
	for i := 0; i < 4; i++ {
		b, err := strconv.ParseUint(hex[i*2:i*2+2], 16, 8)
		if err != nil {
			return "", err
		}
		// /proc/net/tcp stores IPv4 in little-endian order: byte 0 is rightmost.
		ip[i] = byte(b)
	}
	// Reverse byte order (little-endian to host).
	return fmt.Sprintf("%d.%d.%d.%d", ip[3], ip[2], ip[1], ip[0]), nil
}

func parseHexIPv6(hex string) (string, error) {
	if len(hex) != 32 {
		return "", fmt.Errorf("invalid IPv6 hex length: %d", len(hex))
	}

	// /proc/net/tcp6 stores each 32-bit group in little-endian.
	// There are 4 groups of 8 hex chars.
	var groups [4]uint32
	for g := 0; g < 4; g++ {
		chunk := hex[g*8 : g*8+8]
		// Read the 4 bytes, then reverse for little-endian.
		var bytes [4]byte
		for i := 0; i < 4; i++ {
			b, err := strconv.ParseUint(chunk[i*2:i*2+2], 16, 8)
			if err != nil {
				return "", err
			}
			bytes[i] = byte(b)
		}
		// Reverse (little-endian to big-endian).
		groups[g] = uint32(bytes[3])<<24 | uint32(bytes[2])<<16 | uint32(bytes[1])<<8 | uint32(bytes[0])
	}

	// Build standard IPv6 notation.
	addr := fmt.Sprintf("%x:%x:%x:%x:%x:%x:%x:%x",
		groups[0]>>16, groups[0]&0xFFFF,
		groups[1]>>16, groups[1]&0xFFFF,
		groups[2]>>16, groups[2]&0xFFFF,
		groups[3]>>16, groups[3]&0xFFFF,
	)

	// Simplify well-known addresses.
	switch addr {
	case "0:0:0:0:0:0:0:0":
		return "::", nil
	case "0:0:0:0:0:0:0:1":
		return "::1", nil
	case "0:0:0:0:0:ffff:7f00:1":
		return "127.0.0.1", nil // IPv4-mapped loopback
	}

	return addr, nil
}
