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

package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/spf13/cobra"
)

const (
	defaultListTimeout = 30 * time.Second
	allPagesTimeout    = 2 * time.Minute
)

// cmdContext returns cmd.Context() if available, otherwise context.Background().
func cmdContext(cmd *cobra.Command) context.Context {
	if cmd != nil && cmd.Context() != nil {
		return cmd.Context()
	}
	return context.Background()
}

// listRequestContext returns a request context with a 2-minute timeout when
// paging all results, or the standard 30-second timeout otherwise.
func listRequestContext(parent context.Context, all bool) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	timeout := defaultListTimeout
	if all {
		timeout = allPagesTimeout
	}
	return context.WithTimeout(parent, timeout)
}

// validateListLimitFlags rejects conflicting --<allFlag> + --<limitFlag>
// combinations and non-positive --<limitFlag> values.
func validateListLimitFlags(cmd *cobra.Command, limit int, all bool, limitFlag, allFlag string) error {
	if limitFlag == "" {
		limitFlag = "limit"
	}
	if allFlag == "" {
		allFlag = "all"
	}
	if all && cmd != nil && cmd.Flags().Changed(limitFlag) {
		return fmt.Errorf("cannot use --%s together with --%s", allFlag, limitFlag)
	}
	if !all && limit <= 0 {
		return fmt.Errorf("--%s must be a positive integer (got %d); use --%s to list all", limitFlag, limit, allFlag)
	}
	return nil
}

// printListTruncationNotice writes a human-readable truncation notice to w
// (defaulting to os.Stderr) when truncated is true. When total <= shown (for
// example when an endpoint's totalCount is zeroed or stale after per-row
// authorization filtering), it omits the unknown total rather than printing
// a misleading "Showing N of N".
func printListTruncationNotice(w io.Writer, shown, total int, truncated bool, noun, limitFlag, allFlag string) {
	if !truncated {
		return
	}
	if w == nil {
		w = os.Stderr
	}
	if limitFlag == "" {
		limitFlag = "limit"
	}
	if allFlag == "" {
		allFlag = "all"
	}
	if total > shown {
		_, _ = fmt.Fprintf(w, "Showing %d of %d %s; use --%s or --%s to see more.\n",
			shown, total, noun, limitFlag, allFlag)
		return
	}
	_, _ = fmt.Fprintf(w, "Showing %d %s (more available); use --%s or --%s to see more.\n",
		shown, noun, limitFlag, allFlag)
}

// listPageResult holds aggregated items and pagination metadata from fetchListPages.
type listPageResult[T any] struct {
	Items      []T
	NextCursor string
	TotalCount int
	Truncated  bool
}

// fetchListPages retrieves one or more cursor-paginated pages from fetch.
//
//   - When !all and limit <= maxPageSize, a single request with pageLimit=limit
//     is issued (continuing only if the server returns a short page with a
//     non-empty nextCursor, as authorized-list endpoints can).
//   - When !all and limit > maxPageSize, pages of size min(remaining, maxPageSize)
//     are fetched until len(items) >= limit or nextCursor is empty.
//   - When all is true, pages of size maxPageSize are fetched until nextCursor
//     is empty, bounded by maxListPages with a stuck-cursor guard.
func fetchListPages[T any](
	ctx context.Context,
	limit int,
	all bool,
	maxPageSize int,
	fetch func(ctx context.Context, pageLimit int, cursor string) (items []T, nextCursor string, totalCount int, err error),
) (listPageResult[T], error) {
	if maxPageSize <= 0 {
		return listPageResult[T]{}, fmt.Errorf("invalid maxPageSize: %d", maxPageSize)
	}
	if !all && limit <= 0 {
		return listPageResult[T]{}, fmt.Errorf("invalid limit: %d", limit)
	}

	var (
		allItems   []T
		cursor     string
		nextCursor string
		total      int
		truncated  bool
	)

	for page := 0; ; page++ {
		if page >= maxListPages {
			return listPageResult[T]{}, fmt.Errorf("stopped after %d pages", maxListPages)
		}

		pageLimit := maxPageSize
		if !all {
			remaining := limit - len(allItems)
			if remaining <= 0 {
				break
			}
			if remaining < pageLimit {
				pageLimit = remaining
			}
		}

		items, next, pageTotal, err := fetch(ctx, pageLimit, cursor)
		if err != nil {
			return listPageResult[T]{}, err
		}

		if pageTotal > 0 {
			total = pageTotal
		}
		allItems = append(allItems, items...)
		nextCursor = next

		if next == "" {
			truncated = false
			break
		}
		if !all && len(allItems) >= limit {
			truncated = true
			break
		}
		if next == cursor {
			return listPageResult[T]{}, fmt.Errorf("pagination cursor did not advance (%q)", next)
		}
		cursor = next
	}

	if allItems == nil {
		allItems = []T{}
	}
	if !truncated {
		total = len(allItems)
	} else if total > 0 && total < len(allItems) {
		total = len(allItems)
	}

	return listPageResult[T]{
		Items:      allItems,
		NextCursor: nextCursor,
		TotalCount: total,
		Truncated:  truncated,
	}, nil
}

// messageListJSONOutput wraps store.ListResult[store.Message] fields with an
// additive truncated boolean for CLI --json output.
type messageListJSONOutput struct {
	Items      []store.Message `json:"items"`
	NextCursor string          `json:"nextCursor,omitempty"`
	TotalCount int             `json:"totalCount,omitempty"`
	Truncated  bool            `json:"truncated"`
}
