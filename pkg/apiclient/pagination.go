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

package apiclient

import (
	"net/url"
	"strconv"
)

// PageOptions configures pagination for list requests.
type PageOptions struct {
	Limit  int    // Maximum results per page (default varies by endpoint)
	Cursor string // Pagination cursor from previous response
}

// PageResult contains pagination metadata from a list response.
type PageResult struct {
	NextCursor string // Cursor for the next page (empty if no more pages)
	TotalCount int    // Total count of items (if available)
}

// HasMore returns true if there are more pages available.
func (p *PageResult) HasMore() bool {
	return p.NextCursor != ""
}

// ToQuery adds pagination parameters to a URL query.
func (p *PageOptions) ToQuery(q url.Values) url.Values {
	if q == nil {
		q = url.Values{}
	}
	if p.Limit > 0 {
		q.Set("limit", strconv.Itoa(p.Limit))
	}
	if p.Cursor != "" {
		q.Set("cursor", p.Cursor)
	}
	return q
}
