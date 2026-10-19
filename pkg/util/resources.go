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
	"fmt"
	"math"
	"strconv"
	"strings"
)

// ParseMemory parses a Kubernetes-style memory quantity and returns bytes.
// Accepts binary suffixes (Ki, Mi, Gi, Ti, Pi) and decimal suffixes (K, M, G, T, P),
// as well as plain byte counts.
func ParseMemory(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty memory value")
	}

	// Binary suffixes (powers of 1024)
	binarySuffixes := []struct {
		suffix     string
		multiplier int64
	}{
		{"Pi", 1 << 50},
		{"Ti", 1 << 40},
		{"Gi", 1 << 30},
		{"Mi", 1 << 20},
		{"Ki", 1 << 10},
	}

	for _, bs := range binarySuffixes {
		if strings.HasSuffix(s, bs.suffix) {
			val, err := strconv.ParseFloat(s[:len(s)-len(bs.suffix)], 64)
			if err != nil {
				return 0, fmt.Errorf("invalid memory value %q: %w", s, err)
			}
			return checkedMemoryBytes(s, val*float64(bs.multiplier))
		}
	}

	// Decimal suffixes (powers of 1000) — also accept lowercase and
	// common Docker-style abbreviations like "2g", "512m", "1024MB"
	decimalSuffixes := []struct {
		suffixes   []string
		multiplier int64
	}{
		{[]string{"PB", "Pb", "P", "p"}, 1e15},
		{[]string{"TB", "Tb", "T", "t"}, 1e12},
		{[]string{"GB", "Gb", "G", "g"}, 1e9},
		{[]string{"MB", "Mb", "M", "m"}, 1e6},
		{[]string{"KB", "Kb", "K", "k"}, 1e3},
	}

	for _, ds := range decimalSuffixes {
		for _, suffix := range ds.suffixes {
			if strings.HasSuffix(s, suffix) {
				val, err := strconv.ParseFloat(s[:len(s)-len(suffix)], 64)
				if err != nil {
					return 0, fmt.Errorf("invalid memory value %q: %w", s, err)
				}
				return checkedMemoryBytes(s, val*float64(ds.multiplier))
			}
		}
	}

	// Plain bytes
	val, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid memory value %q: %w", s, err)
	}
	return val, nil
}

func checkedMemoryBytes(input string, bytes float64) (int64, error) {
	// The upper bound is exclusive: 1<<63 cannot be represented by int64.
	if math.IsNaN(bytes) || bytes >= 1<<63 || bytes < -(1<<63) {
		return 0, fmt.Errorf("memory value %q is outside the supported byte range", input)
	}
	return int64(bytes), nil
}

// FormatMemoryForDocker formats bytes as a Docker-compatible memory string.
// Docker accepts suffixes: b, k, m, g.
func FormatMemoryForDocker(bytes int64) string {
	if bytes <= 0 {
		return "0"
	}
	if bytes%int64(1<<30) == 0 {
		return fmt.Sprintf("%dg", bytes/(1<<30))
	}
	if bytes%int64(1<<20) == 0 {
		return fmt.Sprintf("%dm", bytes/(1<<20))
	}
	if bytes%int64(1<<10) == 0 {
		return fmt.Sprintf("%dk", bytes/(1<<10))
	}
	return strconv.FormatInt(bytes, 10)
}

// FormatMemoryForApple formats bytes as an Apple Container-compatible memory string.
// Apple container accepts suffixes: K, M, G, T, P (decimal-style but actually used as binary).
func FormatMemoryForApple(bytes int64) string {
	if bytes <= 0 {
		return "0"
	}
	// Apple container docs say 1 MiByte granularity minimum.
	// Use the largest clean unit.
	if bytes%int64(1<<30) == 0 {
		return fmt.Sprintf("%dG", bytes/(1<<30))
	}
	if bytes%int64(1<<20) == 0 {
		return fmt.Sprintf("%dM", bytes/(1<<20))
	}
	// Round up to nearest MiB
	mib := int64(math.Ceil(float64(bytes) / float64(1<<20)))
	return fmt.Sprintf("%dM", mib)
}

// ParseCPU parses a CPU quantity. Accepts whole numbers, decimals, and
// Kubernetes millicore notation (e.g., "500m" = 0.5 cores).
// Returns the value as a finite float64 number of cores.
func ParseCPU(s string) (float64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty cpu value")
	}

	if strings.HasSuffix(s, "m") {
		millis, err := strconv.ParseFloat(s[:len(s)-1], 64)
		if err != nil {
			return 0, fmt.Errorf("invalid cpu value %q: %w", s, err)
		}
		if math.IsNaN(millis) || math.IsInf(millis, 0) {
			return 0, fmt.Errorf("invalid cpu value %q: must be finite", s)
		}
		return millis / 1000.0, nil
	}

	val, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid cpu value %q: %w", s, err)
	}
	if math.IsNaN(val) || math.IsInf(val, 0) {
		return 0, fmt.Errorf("invalid cpu value %q: must be finite", s)
	}
	return val, nil
}

// FormatCPU formats a CPU core count as a string suitable for Docker/Apple container --cpus flag.
func FormatCPU(cores float64) string {
	if cores == float64(int64(cores)) {
		return strconv.FormatInt(int64(cores), 10)
	}
	return strconv.FormatFloat(cores, 'f', -1, 64)
}
