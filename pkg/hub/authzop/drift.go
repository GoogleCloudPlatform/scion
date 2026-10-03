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

package authzop

// DriftKind classifies the kind of mismatch CheckDrift found between the
// declared catalog and the actual, live entry points for an operation.
type DriftKind string

const (
	// DriftMissingFromCatalog: a live entry point exists with no
	// corresponding catalog declaration for that operation.
	DriftMissingFromCatalog DriftKind = "missing_from_catalog"
	// DriftMissingRoute: the catalog declares an entry point for which no
	// live route/service/scheduled entry point was found.
	DriftMissingRoute DriftKind = "missing_route"
	// DriftPatternMismatch: both a catalog entry and a live entry point
	// exist for the operation and kind, but their pattern or method
	// disagree (e.g. catalog "/attach" vs. live "/pty"; catalog PUT vs.
	// live PATCH on the same pattern).
	DriftPatternMismatch DriftKind = "pattern_mismatch"
)

// DriftFinding is one mismatch between catalog metadata and a live entry
// point.
type DriftFinding struct {
	OperationID OperationID
	Kind        DriftKind
	// Catalog is the catalog-declared entry point involved, or nil for
	// DriftMissingFromCatalog.
	Catalog *EntryPoint
	// Actual is the discovered live entry point involved, or nil for
	// DriftMissingRoute.
	Actual *EntryPoint
	Detail string
}

// DiscoveredEntryPoint is what a route inventory reports for one live entry
// point. Discovered inputs must come from actual routes and service/
// scheduler registrations (e.g. hand-verified against server.go,
// pty_handlers.go, and equivalent registration call sites) — never from
// another copy of catalog.go's own metadata, which is exactly what
// CheckDrift verifies against.
type DiscoveredEntryPoint struct {
	Kind    EntryPointKind
	Pattern string
	Method  string
}

func (d DiscoveredEntryPoint) matches(e EntryPoint) bool {
	return d.Kind == e.Kind && d.Pattern == e.Pattern && d.Method == e.Method
}

// CheckDrift compares the catalog's declared EntryPoints for each operation
// against a supplied discovered-entry-point map and returns every mismatch.
// An operation absent from discovered is not itself a finding — CheckDrift
// only reports mismatches for operations present in discovered (callers
// build the inventory only for the entry points they have verified against
// live registrations); a discovered operation ID with no matching
// OperationSpec at all is reported as DriftMissingFromCatalog for each of
// its entries.
func CheckDrift(discovered map[OperationID][]DiscoveredEntryPoint) []DriftFinding {
	var findings []DriftFinding

	specByID := make(map[OperationID]OperationSpec, len(Catalog))
	for _, spec := range Catalog {
		specByID[spec.ID] = spec
	}

	for opID, discoveredEntries := range discovered {
		spec, hasSpec := specByID[opID]
		if !hasSpec {
			for i := range discoveredEntries {
				ep := discoveredEntries[i].asEntryPoint()
				findings = append(findings, DriftFinding{
					OperationID: opID,
					Kind:        DriftMissingFromCatalog,
					Actual:      &ep,
					Detail:      "no OperationSpec registered for this operation ID",
				})
			}
			continue
		}

		matchedCatalog := make([]bool, len(spec.EntryPoints))
		matchedDiscovered := make([]bool, len(discoveredEntries))

		for ci := range spec.EntryPoints {
			for di := range discoveredEntries {
				if matchedDiscovered[di] {
					continue
				}
				if discoveredEntries[di].matches(spec.EntryPoints[ci]) {
					matchedCatalog[ci] = true
					matchedDiscovered[di] = true
					break
				}
			}
		}

		for ci, ok := range matchedCatalog {
			if ok {
				continue
			}
			catalogEntry := spec.EntryPoints[ci]
			// Look for a same-kind discovered entry with a different
			// pattern/method, to distinguish "route moved" from "route
			// gone entirely".
			var mismatch *EntryPoint
			for di, dmatched := range matchedDiscovered {
				if dmatched {
					continue
				}
				d := discoveredEntries[di]
				if d.Kind == catalogEntry.Kind {
					ep := d.asEntryPoint()
					mismatch = &ep
					matchedDiscovered[di] = true
					break
				}
			}
			ce := catalogEntry
			if mismatch != nil {
				findings = append(findings, DriftFinding{
					OperationID: opID,
					Kind:        DriftPatternMismatch,
					Catalog:     &ce,
					Actual:      mismatch,
					Detail:      "catalog entry point does not match the live route of the same kind",
				})
			} else {
				findings = append(findings, DriftFinding{
					OperationID: opID,
					Kind:        DriftMissingRoute,
					Catalog:     &ce,
					Detail:      "no live entry point found for this catalog declaration",
				})
			}
		}

		for di, ok := range matchedDiscovered {
			if ok {
				continue
			}
			ep := discoveredEntries[di].asEntryPoint()
			findings = append(findings, DriftFinding{
				OperationID: opID,
				Kind:        DriftMissingFromCatalog,
				Actual:      &ep,
				Detail:      "live entry point has no matching catalog declaration",
			})
		}
	}

	return findings
}

func (d DiscoveredEntryPoint) asEntryPoint() EntryPoint {
	return EntryPoint(d)
}
