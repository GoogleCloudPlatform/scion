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
	"net/http"
	"sort"
	"strings"
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

// visitorListableMethods are the methods VisitorMethods may name.
var visitorListableMethods = map[string]bool{
	http.MethodGet: true, http.MethodHead: true, http.MethodPost: true,
	http.MethodPut: true, http.MethodPatch: true, http.MethodDelete: true,
}

// patternServesMethod reports whether a method-qualified pattern whose
// method is patternMethod serves method: its own method, and HEAD for a
// GET pattern (http.ServeMux serves HEAD on GET patterns).
func patternServesMethod(patternMethod, method string) bool {
	return method == patternMethod || (patternMethod == http.MethodGet && method == http.MethodHead)
}

// validateVisitorRoutes checks every route that lists VisitorMethods. A
// visitor admitted by routeGuard still passes the route's classification
// guard, so a list is accepted only on an authenticated or policy route,
// where that guard admits a user identity and the handler authorizes it.
// (Session-only routes are hub-admin routes, so they are excluded too.)
// Each method must be a known method, listed once, and served by the
// pattern: a method-qualified pattern ("GET /x") serves only its method
// (and HEAD for GET). It returns every violation, sorted by pattern; New
// refuses to start on any.
func validateVisitorRoutes(table map[string]RouteMetadata) error {
	patterns := make([]string, 0, len(table))
	for p, meta := range table {
		if len(meta.VisitorMethods) > 0 {
			patterns = append(patterns, p)
		}
	}
	sort.Strings(patterns)
	var errs []error
	for _, p := range patterns {
		meta := table[p]
		if meta.Classification != RouteAuthenticated && meta.Classification != RoutePolicy {
			errs = append(errs, fmt.Errorf("route %q (%s): visitor methods: classification %q (want %q or %q)",
				p, meta.RouteID, meta.Classification, RouteAuthenticated, RoutePolicy))
		}
		patternMethod, _, qualified := strings.Cut(p, " ")
		seen := make(map[string]bool, len(meta.VisitorMethods))
		for _, m := range meta.VisitorMethods {
			switch {
			case !visitorListableMethods[m]:
				errs = append(errs, fmt.Errorf("route %q (%s): visitor methods: unknown method %q", p, meta.RouteID, m))
			case seen[m]:
				errs = append(errs, fmt.Errorf("route %q (%s): visitor methods: method %q listed twice", p, meta.RouteID, m))
			case qualified && !patternServesMethod(patternMethod, m):
				errs = append(errs, fmt.Errorf("route %q (%s): visitor methods: method %q is not served by the pattern's method %q",
					p, meta.RouteID, m, patternMethod))
			}
			seen[m] = true
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("invalid visitor route metadata: %w", errors.Join(errs...))
	}
	return nil
}
