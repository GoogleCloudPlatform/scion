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

package hub

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ExternalEffectCallSiteClassification is the durable call-site guard for
// messaging and agent-dispatch external effects (C3 containment).
//
// Every non-test call to dispatchWithBrokerRetry, DispatchAgentMessage,
// DispatchAgentCreate, or DispatchAgentStart in pkg/hub (including
// subpackages) must be classified as "guarded" or "exempt" with a truthful
// reason. The test fails on:
//   - an unclassified new site (prevents silent bypass introduction),
//   - a stale classification (prevents drift), and
//   - zero matches (scanner self-check).
//
// This is the mechanism that would have caught the B1 scheduled-message
// bypass and will catch the next one.

// targetSymbols is the set of function/method names that constitute
// external-effect emit primitives.
var targetSymbols = map[string]bool{
	"dispatchWithBrokerRetry":       true,
	"DispatchAgentMessage":          true,
	"DispatchAgentCreate":           true,
	"DispatchAgentStart":            true,
	"DispatchAgentCreateWithGather": true,
}

func TestExternalEffectCallSiteClassification(t *testing.T) {
	// Build the classified set for lookup.
	type classKey struct{ file, function, symbol string }
	classified := make(map[classKey]bool)
	for _, e := range effectCallSiteClassifications {
		k := classKey{e.file, e.function, e.symbol}
		if classified[k] {
			t.Errorf("duplicate classification: %s:%s (%s)", e.file, e.function, e.symbol)
		}
		classified[k] = true
	}

	// Discover all call sites in production code.
	discovered := discoverEffectCallSites(t)

	// Scanner self-check: zero matches means the scanner is broken.
	if len(discovered) == 0 {
		t.Fatal("found zero external-effect call sites — the scanner is broken or the target symbols have been renamed")
	}

	// Check every discovered site is classified.
	discoveredKeys := make(map[classKey]bool)
	for _, site := range discovered {
		k := classKey{site.file, site.function, site.symbol}
		discoveredKeys[k] = true
		if !classified[k] {
			t.Errorf("UNCLASSIFIED external-effect call site: %s in %s:%s (line %d)\n"+
				"  Every call to %s must be in effectCallSiteClassifications as 'guarded' or 'exempt'.",
				site.symbol, site.file, site.function, site.line, site.symbol)
		}
	}

	// Check no stale classifications exist.
	for _, e := range effectCallSiteClassifications {
		k := classKey{e.file, e.function, e.symbol}
		if !discoveredKeys[k] {
			t.Errorf("STALE classification: %s in %s:%s — call site no longer exists in production code.\n"+
				"  Remove or update this entry in effectCallSiteClassifications.",
				e.symbol, e.file, e.function)
		}
	}

	// Report summary.
	guardedCount := 0
	exemptCount := 0
	for _, e := range effectCallSiteClassifications {
		switch e.class {
		case "guarded":
			guardedCount++
		case "exempt":
			exemptCount++
		}
	}
	t.Logf("Verified %d external-effect call sites: %d guarded, %d exempt",
		len(discovered), guardedCount, exemptCount)
}

// discoverEffectCallSites finds all production call sites of the target
// symbols in non-test .go files under pkg/hub, including subpackages.
// This fixes the directory-walk blind spot in create_message_enumeration_test.go
// which skips subdirectories.
func discoverEffectCallSites(t *testing.T) []effectCallSite {
	t.Helper()
	hubDir := findHubDir(t)
	var sites []effectCallSite

	// Walk pkg/hub and all subdirectories to avoid the subpackage blind spot.
	err := filepath.Walk(hubDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			// Skip test fixtures and vendored directories.
			base := filepath.Base(path)
			if base == "testdata" || base == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(info.Name(), ".go") || strings.HasSuffix(info.Name(), "_test.go") {
			return nil
		}

		relPath, _ := filepath.Rel(hubDir, path)
		// Use just the base filename for top-level files, or subdir/file for subpackages.
		// For top-level pkg/hub files, relPath is just the filename.
		fset := token.NewFileSet()
		f, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			t.Logf("warning: failed to parse %s: %v", relPath, parseErr)
			return nil
		}

		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sym := extractCallSymbol(call)
			if !targetSymbols[sym] {
				return true
			}
			pos := fset.Position(call.Pos())
			funcName := enclosingFuncName(fset, f, pos.Offset)
			// Skip function declarations that ARE the interface/primitive itself.
			// We want call sites, not definitions.
			sites = append(sites, effectCallSite{
				file:     relPath,
				function: funcName,
				symbol:   sym,
				line:     pos.Line,
			})
			return true
		})

		return nil
	})
	if err != nil {
		t.Fatalf("failed to walk hub directory: %v", err)
	}

	return sites
}
