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

// Command gen copies the canonical harnesses/scion_harness.py into each
// harnesses/<name>/scion_harness.py, prepending a GENERATED header.
//
// Usage:
//
//	go run ./harnesses/gen
package main

import (
	"fmt"
	"os"
	"path/filepath"
)

const header = "# GENERATED FILE — DO NOT EDIT. Source: harnesses/scion_harness.py\n"

func main() {
	harnessesDir := filepath.Join("harnesses")
	canonical := filepath.Join(harnessesDir, "scion_harness.py")

	src, err := os.ReadFile(canonical)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: read canonical %s: %v\n", canonical, err)
		os.Exit(1)
	}

	entries, err := os.ReadDir(harnessesDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: read harnesses dir: %v\n", err)
		os.Exit(1)
	}

	generated := header + string(src)

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if name == "gen" {
			continue
		}
		// Only process directories containing a provision.py (i.e. real bundles).
		provisionPath := filepath.Join(harnessesDir, name, "provision.py")
		if _, err := os.Stat(provisionPath); err != nil {
			continue
		}

		dst := filepath.Join(harnessesDir, name, "scion_harness.py")
		if err := os.WriteFile(dst, []byte(generated), 0644); err != nil {
			fmt.Fprintf(os.Stderr, "error: write %s: %v\n", dst, err)
			os.Exit(1)
		}
		fmt.Printf("generated %s\n", dst)
	}
}
