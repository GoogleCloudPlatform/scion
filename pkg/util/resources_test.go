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

package util

import (
	"testing"
)

func TestParseMemory(t *testing.T) {
	tests := []struct {
		input    string
		expected int64
		wantErr  bool
	}{
		// Binary suffixes
		{"1Ki", 1024, false},
		{"1Mi", 1048576, false},
		{"1Gi", 1073741824, false},
		{"2Gi", 2147483648, false},
		{"512Mi", 536870912, false},
		{"1Ti", 1099511627776, false},

		// Decimal suffixes
		{"1K", 1000, false},
		{"1M", 1000000, false},
		{"1G", 1000000000, false},
		{"2G", 2000000000, false},
		{"512M", 512000000, false},
		{"1T", 1000000000000, false},

		// Lowercase/Docker-style
		{"2g", 2000000000, false},
		{"512m", 512000000, false},
		{"1k", 1000, false},

		// Multi-char suffixes
		{"2GB", 2000000000, false},
		{"512MB", 512000000, false},

		// Plain bytes
		{"1073741824", 1073741824, false},
		{"9223372036854775807", 1<<63 - 1, false},
		{"0", 0, false},
		{"0Gi", 0, false},

		// Representable boundaries for suffixed quantities.
		{"8191Pi", 8191 << 50, false},
		{"-8192Pi", -1 << 63, false},

		// Fractional
		{"1.5Gi", 1610612736, false},
		{"0.5G", 500000000, false},

		// Errors
		{"", 0, true},
		{"abc", 0, true},
		{"Gi", 0, true},
		{"8192Pi", 0, true},
		{"9223372036854776K", 0, true},
		{"-8193Pi", 0, true},
		{"1e308G", 0, true},
		{"NaNGi", 0, true},
		{"InfM", 0, true},
		{"-InfMi", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := ParseMemory(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseMemory(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
				return
			}
			if !tt.wantErr && got != tt.expected {
				t.Errorf("ParseMemory(%q) = %d, want %d", tt.input, got, tt.expected)
			}
		})
	}
}

func TestFormatMemoryForDocker(t *testing.T) {
	tests := []struct {
		input    int64
		expected string
	}{
		{0, "0"},
		{1073741824, "1g"},
		{2147483648, "2g"},
		{536870912, "512m"},
		{1048576, "1m"},
		{1024, "1k"},
		{1500, "1500"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			got := FormatMemoryForDocker(tt.input)
			if got != tt.expected {
				t.Errorf("FormatMemoryForDocker(%d) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func TestFormatMemoryForApple(t *testing.T) {
	tests := []struct {
		input    int64
		expected string
	}{
		{0, "0"},
		{1073741824, "1G"},
		{2147483648, "2G"},
		{536870912, "512M"},
		{1048576, "1M"},
		{1500, "1M"}, // rounds up to 1 MiB
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			got := FormatMemoryForApple(tt.input)
			if got != tt.expected {
				t.Errorf("FormatMemoryForApple(%d) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func TestParseCPU(t *testing.T) {
	tests := []struct {
		input    string
		expected float64
		wantErr  bool
	}{
		{"1", 1.0, false},
		{"4", 4.0, false},
		{"0.5", 0.5, false},
		{"2.5", 2.5, false},
		{"500m", 0.5, false},
		{"1000m", 1.0, false},
		{"250m", 0.25, false},
		{"0", 0, false},
		{"0m", 0, false},
		{" 500m ", 0.5, false},
		{"1e3m", 1, false},
		{"-1", -1, false},
		{"-500m", -0.5, false},
		{"", 0, true},
		{"abc", 0, true},
		{"m", 0, true},
		{"NaN", 0, true},
		{"NaNm", 0, true},
		{"Inf", 0, true},
		{"+Inf", 0, true},
		{"-Inf", 0, true},
		{"Infm", 0, true},
		{"-Infm", 0, true},
		{"Infinity", 0, true},
		{"+Infinitym", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := ParseCPU(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseCPU(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
				return
			}
			if !tt.wantErr && got != tt.expected {
				t.Errorf("ParseCPU(%q) = %f, want %f", tt.input, got, tt.expected)
			}
		})
	}
}

func TestFormatCPU(t *testing.T) {
	tests := []struct {
		input    float64
		expected string
	}{
		{1.0, "1"},
		{4.0, "4"},
		{0.5, "0.5"},
		{2.5, "2.5"},
		{0.25, "0.25"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			got := FormatCPU(tt.input)
			if got != tt.expected {
				t.Errorf("FormatCPU(%f) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}
