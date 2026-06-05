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

// Command fixturegen generates the canonical hub test fixture database
// (testdata/hub-v46-fixture.db) from a Go-defined spec, verifies that every
// domain table is covered, and caches the resulting blob to the shared
// scratchpad mount for reuse by other agents and CI.
//
// Usage:
//
//	go run ./internal/fixturegen
//
// The run fails (non-zero exit) if any domain table ends up with zero rows.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// defaultOutputPath is the repository-relative path of the generated fixture.
const defaultOutputPath = "testdata/hub-v46-fixture.db"

// defaultCacheDir is the shared-mount location where the fixture blob is cached
// for reuse. Overridable via SCION_FIXTURE_CACHE_DIR.
const defaultCacheDir = "/scion-volumes/scratchpad/postgres-integration/fixtures"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fixturegen: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	ctx := context.Background()

	outPath := defaultOutputPath
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return fmt.Errorf("creating output dir: %w", err)
	}

	report, err := Generate(ctx, outPath)
	if err != nil {
		return err
	}

	printReport(report)

	if len(report.Missing) > 0 {
		return fmt.Errorf("coverage check failed: %d table(s) with zero rows: %v",
			len(report.Missing), report.Missing)
	}

	// Cache the blob to the shared mount. A missing/unwritable mount is a
	// warning, not a hard failure, so the fixture can still be generated
	// locally without the scratchpad.
	cacheDir := defaultCacheDir
	if v := os.Getenv("SCION_FIXTURE_CACHE_DIR"); v != "" {
		cacheDir = v
	}
	if cached, err := cacheBlob(outPath, cacheDir); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not cache fixture to %s: %v\n", cacheDir, err)
	} else {
		fmt.Printf("Cached fixture blob -> %s\n", cached)
	}

	return nil
}

// printReport prints the per-table coverage report.
func printReport(r *Report) {
	fmt.Printf("Generated fixture: %s\n", r.Path)
	fmt.Printf("Coverage: %d domain tables\n", r.TotalTables())
	for _, c := range r.Counts {
		fmt.Printf("  %-32s %d row(s)\n", c.Table, c.Count)
	}
}

// cacheBlob copies the generated fixture into cacheDir and returns the
// destination path.
func cacheBlob(srcPath, cacheDir string) (string, error) {
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", err
	}
	dst := filepath.Join(cacheDir, filepath.Base(srcPath))
	if err := copyFile(srcPath, dst); err != nil {
		return "", err
	}
	return dst, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}
