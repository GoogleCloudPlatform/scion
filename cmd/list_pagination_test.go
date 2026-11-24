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
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateListLimitFlags(t *testing.T) {
	t.Run("nil cmd valid limit", func(t *testing.T) {
		assert.NoError(t, validateListLimitFlags(nil, 50, false, "limit", "all"))
	})

	t.Run("nil cmd all true", func(t *testing.T) {
		assert.NoError(t, validateListLimitFlags(nil, 0, true, "limit", "all"))
	})

	t.Run("zero limit rejected", func(t *testing.T) {
		err := validateListLimitFlags(nil, 0, false, "limit", "all")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--limit")
		assert.Contains(t, err.Error(), "--all")
	})

	t.Run("negative limit rejected", func(t *testing.T) {
		err := validateListLimitFlags(nil, -5, false, "limit", "all")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--limit")
	})

	t.Run("all with explicit limit rejected", func(t *testing.T) {
		var lim int
		var all bool
		c := &cobra.Command{}
		c.Flags().IntVar(&lim, "limit", 50, "")
		c.Flags().BoolVar(&all, "all", false, "")
		require.NoError(t, c.Flags().Set("limit", "10"))
		require.NoError(t, c.Flags().Set("all", "true"))

		err := validateListLimitFlags(c, lim, all, "limit", "all")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--all")
		assert.Contains(t, err.Error(), "--limit")
	})

	t.Run("all with default unchanged limit allowed", func(t *testing.T) {
		var lim int
		var all bool
		c := &cobra.Command{}
		c.Flags().IntVar(&lim, "limit", 50, "")
		c.Flags().BoolVar(&all, "all", false, "")
		require.NoError(t, c.Flags().Set("all", "true"))

		assert.NoError(t, validateListLimitFlags(c, lim, all, "limit", "all"))
	})
}

func TestPrintListTruncationNotice(t *testing.T) {
	t.Run("not truncated emits nothing", func(t *testing.T) {
		var buf bytes.Buffer
		printListTruncationNotice(&buf, 2, 2, false, "messages", "limit", "all")
		assert.Empty(t, buf.String())
	})

	t.Run("known total greater than shown", func(t *testing.T) {
		var buf bytes.Buffer
		printListTruncationNotice(&buf, 25, 100, true, "messages", "limit", "all")
		assert.Equal(t, "Showing 25 of 100 messages; use --limit or --all to see more.\n", buf.String())
	})

	t.Run("unknown or stale total less than or equal to shown", func(t *testing.T) {
		var buf bytes.Buffer
		printListTruncationNotice(&buf, 50, 50, true, "runtime brokers", "limit", "all")
		assert.Equal(t, "Showing 50 runtime brokers (more available); use --limit or --all to see more.\n", buf.String())

		buf.Reset()
		printListTruncationNotice(&buf, 50, 0, true, "scheduled events", "limit", "all")
		assert.Equal(t, "Showing 50 scheduled events (more available); use --limit or --all to see more.\n", buf.String())
	})
}

func TestFetchListPages(t *testing.T) {
	t.Run("single page non-truncated", func(t *testing.T) {
		calls := 0
		res, err := fetchListPages(context.Background(), 25, false, 200, func(_ context.Context, pageLimit int, cursor string) ([]int, string, int, error) {
			calls++
			assert.Equal(t, 25, pageLimit)
			assert.Empty(t, cursor)
			return []int{1, 2, 3}, "", 3, nil
		})
		require.NoError(t, err)
		assert.Equal(t, 1, calls)
		assert.Equal(t, []int{1, 2, 3}, res.Items)
		assert.Equal(t, 3, res.TotalCount)
		assert.False(t, res.Truncated)
		assert.Empty(t, res.NextCursor)
	})

	t.Run("single page truncated", func(t *testing.T) {
		calls := 0
		res, err := fetchListPages(context.Background(), 2, false, 200, func(_ context.Context, pageLimit int, cursor string) ([]int, string, int, error) {
			calls++
			assert.Equal(t, 2, pageLimit)
			assert.Empty(t, cursor)
			return []int{1, 2}, "cur-2", 5, nil
		})
		require.NoError(t, err)
		assert.Equal(t, 1, calls)
		assert.Equal(t, []int{1, 2}, res.Items)
		assert.Equal(t, 5, res.TotalCount)
		assert.True(t, res.Truncated)
		assert.Equal(t, "cur-2", res.NextCursor)
	})

	t.Run("limit exceeding maxPageSize pages across requests", func(t *testing.T) {
		type req struct {
			limit  int
			cursor string
		}
		var reqs []req
		res, err := fetchListPages(context.Background(), 5, false, 2, func(_ context.Context, pageLimit int, cursor string) ([]int, string, int, error) {
			reqs = append(reqs, req{limit: pageLimit, cursor: cursor})
			switch cursor {
			case "":
				return []int{1, 2}, "c1", 8, nil
			case "c1":
				return []int{3, 4}, "c2", 8, nil
			case "c2":
				return []int{5}, "c3", 8, nil
			default:
				return nil, "", 0, fmt.Errorf("unexpected cursor %q", cursor)
			}
		})
		require.NoError(t, err)
		assert.Equal(t, []req{{2, ""}, {2, "c1"}, {1, "c2"}}, reqs)
		assert.Equal(t, []int{1, 2, 3, 4, 5}, res.Items)
		assert.Equal(t, 8, res.TotalCount)
		assert.True(t, res.Truncated)
		assert.Equal(t, "c3", res.NextCursor)
	})

	t.Run("all fetches every page until nextCursor empty", func(t *testing.T) {
		var cursors []string
		res, err := fetchListPages(context.Background(), 50, true, 2, func(_ context.Context, pageLimit int, cursor string) ([]int, string, int, error) {
			assert.Equal(t, 2, pageLimit)
			cursors = append(cursors, cursor)
			switch cursor {
			case "":
				return []int{1, 2}, "c1", 2, nil // simulate stale per-page totalCount
			case "c1":
				return []int{3, 4}, "c2", 2, nil
			case "c2":
				return []int{5}, "", 1, nil
			default:
				return nil, "", 0, fmt.Errorf("unexpected cursor %q", cursor)
			}
		})
		require.NoError(t, err)
		assert.Equal(t, []string{"", "c1", "c2"}, cursors)
		assert.Equal(t, []int{1, 2, 3, 4, 5}, res.Items)
		assert.Equal(t, 5, res.TotalCount, "non-truncated total should be at least len(items)")
		assert.False(t, res.Truncated)
		assert.Empty(t, res.NextCursor)
	})

	t.Run("stuck cursor guard", func(t *testing.T) {
		_, err := fetchListPages(context.Background(), 50, true, 10, func(_ context.Context, _ int, cursor string) ([]int, string, int, error) {
			return []int{1}, "same", 100, nil
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cursor did not advance")
	})

	t.Run("maxListPages cap", func(t *testing.T) {
		n := 0
		_, err := fetchListPages(context.Background(), 50, true, 10, func(_ context.Context, _ int, _ string) ([]int, string, int, error) {
			n++
			return []int{n}, fmt.Sprintf("c-%d", n), 0, nil
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), fmt.Sprintf("stopped after %d pages", maxListPages))
		assert.Equal(t, maxListPages, n)
	})

	t.Run("fetch error propagates", func(t *testing.T) {
		boom := errors.New("hub unavailable")
		_, err := fetchListPages(context.Background(), 10, false, 10, func(_ context.Context, _ int, _ string) ([]int, string, int, error) {
			return nil, "", 0, boom
		})
		require.ErrorIs(t, err, boom)
	})

	t.Run("empty result returns non-nil slice", func(t *testing.T) {
		res, err := fetchListPages(context.Background(), 10, false, 10, func(_ context.Context, _ int, _ string) ([]int, string, int, error) {
			return nil, "", 0, nil
		})
		require.NoError(t, err)
		require.NotNil(t, res.Items)
		assert.Empty(t, res.Items)
		assert.False(t, res.Truncated)
	})

	t.Run("non-truncated reconciles stale larger server total to fetched count", func(t *testing.T) {
		res, err := fetchListPages(context.Background(), 50, false, 50, func(_ context.Context, _ int, _ string) ([]int, string, int, error) {
			return []int{1, 2}, "", 10, nil
		})
		require.NoError(t, err)
		assert.False(t, res.Truncated)
		assert.Equal(t, 2, res.TotalCount)
	})

	t.Run("truncated clamps stale positive server total below fetched count", func(t *testing.T) {
		res, err := fetchListPages(context.Background(), 4, false, 2, func(_ context.Context, _ int, cursor string) ([]int, string, int, error) {
			if cursor == "" {
				return []int{1, 2}, "c1", 2, nil
			}
			return []int{3, 4}, "c2", 2, nil
		})
		require.NoError(t, err)
		assert.True(t, res.Truncated)
		assert.Equal(t, 4, res.TotalCount)
	})

	t.Run("truncated preserves unknown zero server total", func(t *testing.T) {
		res, err := fetchListPages(context.Background(), 2, false, 2, func(_ context.Context, _ int, _ string) ([]int, string, int, error) {
			return []int{1, 2}, "c1", 0, nil
		})
		require.NoError(t, err)
		assert.True(t, res.Truncated)
		assert.Equal(t, 0, res.TotalCount)
	})
}
