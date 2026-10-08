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
	"errors"
	"fmt"
	"sort"
)

// startupRouteMetadata is the route table New validates. It is a variable
// only so a test can prove New refuses a misconfigured table.
var startupRouteMetadata = func() map[string]RouteMetadata { return routeMetadataTable }

// validateSessionOnlyRoutes checks every route that sets SessionOnly. The
// route guard applies SessionOnly only on a RouteHubAdmin route with a
// Permission (and, with it, a Resource and an Action), so a SessionOnly
// route must have all of those and a catalog reason. It returns every
// violation, sorted by pattern; New refuses to start on any.
func validateSessionOnlyRoutes(table map[string]RouteMetadata) error {
	patterns := make([]string, 0, len(table))
	for p, meta := range table {
		if meta.SessionOnly != "" {
			patterns = append(patterns, p)
		}
	}
	sort.Strings(patterns)
	var errs []error
	for _, p := range patterns {
		meta := table[p]
		var problems []string
		if !meta.SessionOnly.Valid() {
			problems = append(problems, fmt.Sprintf("unknown session-only reason %q", meta.SessionOnly))
		}
		if meta.Classification != RouteHubAdmin {
			problems = append(problems, fmt.Sprintf("classification %q (want %q)", meta.Classification, RouteHubAdmin))
		}
		if meta.Permission == "" {
			problems = append(problems, "no Permission")
		}
		if meta.Resource == "" || meta.Action == "" {
			problems = append(problems, "no Resource or Action")
		}
		for _, problem := range problems {
			errs = append(errs, fmt.Errorf("route %q (%s): session-only: %s", p, meta.RouteID, problem))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("invalid session-only route metadata: %w", errors.Join(errs...))
	}
	return nil
}
