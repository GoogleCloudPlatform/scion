package hub

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type authorizedListTestItem struct{ id string }

func makeAllowed(n int) []bool {
	allowed := make([]bool, n)
	for i := range allowed {
		allowed[i] = true
	}
	return allowed
}

// TestAuthorizedListDegradesOnCandidateCap is the ptone/scion#1916
// follow-up (C3) regression: crossing authorizedListMaxCandidates must
// degrade the response (an approximate total), never fail the whole
// request. Every item here is allowed, so the page-fill pass fills its
// (small) page well before the cap and returns a full, exact page; only the
// count pass's total is expected to come back approximate.
func TestAuthorizedListDegradesOnCandidateCap(t *testing.T) {
	items := make([]authorizedListTestItem, authorizedListMaxCandidates+1)
	for i := range items {
		items[i] = authorizedListTestItem{id: fmt.Sprint(i)}
	}
	fetch := func(_ context.Context, cursor string, limit int) (authorizedCandidatePage[authorizedListTestItem], error) {
		start := 0
		if cursor != "" {
			_, _ = fmt.Sscan(cursor, &start)
			start++
		}
		end := start + limit
		if end > len(items) {
			end = len(items)
		}
		next := ""
		if end < len(items) {
			next = items[end-1].id
		}
		return authorizedCandidatePage[authorizedListTestItem]{Items: items[start:end], NextCursor: next}, nil
	}
	result, err := authorizedList(context.Background(), nil, "", 1, fetch,
		func(*authorizedListTestItem) Resource { return Resource{} }, func(item *authorizedListTestItem) string { return item.id },
		func(_ context.Context, _ Identity, resources []Resource) ([]bool, error) {
			return makeAllowed(len(resources)), nil
		})
	require.NoError(t, err, "crossing the candidate cap must not fail the request")
	assert.True(t, result.TotalCountApproximate, "the total must be flagged approximate once the cap is crossed")
	assert.GreaterOrEqual(t, result.TotalCount, authorizedListMaxCandidates, "the approximate total must be at least the scanned candidate count")
	require.Len(t, result.Items, 1, "the page-fill pass must still return a full page well ahead of the cap")
	assert.Equal(t, items[0].id, result.Items[0].id)
	assert.NotEmpty(t, result.NextCursor)
}

// TestAuthorizedListPageFillDegradesOnCandidateCap is the page-fill-side
// twin: when every candidate is denied, the page-fill pass must stop at the
// scan budget with a short (here: empty) page and a resume cursor, rather
// than scanning the full candidate pool in one request.
func TestAuthorizedListPageFillDegradesOnCandidateCap(t *testing.T) {
	items := make([]authorizedListTestItem, authorizedListMaxCandidates+1)
	for i := range items {
		items[i] = authorizedListTestItem{id: fmt.Sprint(i)}
	}
	fetch := func(_ context.Context, cursor string, limit int) (authorizedCandidatePage[authorizedListTestItem], error) {
		start := 0
		if cursor != "" {
			_, _ = fmt.Sscan(cursor, &start)
			start++
		}
		end := start + limit
		if end > len(items) {
			end = len(items)
		}
		next := ""
		if end < len(items) {
			next = items[end-1].id
		}
		return authorizedCandidatePage[authorizedListTestItem]{Items: items[start:end], NextCursor: next}, nil
	}
	denyAll := func(_ context.Context, _ Identity, resources []Resource) ([]bool, error) {
		return make([]bool, len(resources)), nil // all false
	}
	result, err := authorizedList(context.Background(), nil, "", 10, fetch,
		func(*authorizedListTestItem) Resource { return Resource{} }, func(item *authorizedListTestItem) string { return item.id },
		denyAll)
	require.NoError(t, err, "an exhausted scan budget must not fail the request")
	assert.Empty(t, result.Items, "no candidate was allowed, so the page must be empty, not a leaked denied item")
	assert.NotEmpty(t, result.NextCursor, "a resume cursor must be returned so a follow-up request continues the scan")
	assert.True(t, result.TotalCountApproximate)
}

func TestAuthorizedListReturnsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	_, err := authorizedList(ctx, nil, "", 1,
		func(context.Context, string, int) (authorizedCandidatePage[authorizedListTestItem], error) {
			called = true
			return authorizedCandidatePage[authorizedListTestItem]{}, nil
		},
		func(*authorizedListTestItem) Resource { return Resource{} }, func(*authorizedListTestItem) string { return "" },
		func(context.Context, Identity, []Resource) ([]bool, error) { return nil, errors.New("unexpected") })
	assert.ErrorIs(t, err, context.Canceled)
	assert.False(t, called)
}

func TestAuthorizedListFailsClosedOnLaterFetchOrAuthorizationError(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fetch func(context.Context, string, int) (authorizedCandidatePage[authorizedListTestItem], error)
		read  func(context.Context, Identity, []Resource) ([]bool, error)
	}{
		{
			name: "later fetch error",
			fetch: func(_ context.Context, cursor string, _ int) (authorizedCandidatePage[authorizedListTestItem], error) {
				if cursor != "" {
					return authorizedCandidatePage[authorizedListTestItem]{}, errors.New("store unavailable")
				}
				return authorizedCandidatePage[authorizedListTestItem]{Items: []authorizedListTestItem{{id: "one"}}, NextCursor: "next"}, nil
			},
			read: func(_ context.Context, _ Identity, resources []Resource) ([]bool, error) {
				return makeAllowed(len(resources)), nil
			},
		},
		{
			name: "authorization error",
			fetch: func(_ context.Context, _ string, _ int) (authorizedCandidatePage[authorizedListTestItem], error) {
				return authorizedCandidatePage[authorizedListTestItem]{Items: []authorizedListTestItem{{id: "one"}}}, nil
			},
			read: func(context.Context, Identity, []Resource) ([]bool, error) {
				return nil, errors.New("authorization unavailable")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := authorizedList(context.Background(), nil, "", 1, tc.fetch,
				func(*authorizedListTestItem) Resource { return Resource{} }, func(item *authorizedListTestItem) string { return item.id }, tc.read)
			require.Error(t, err)
			assert.Empty(t, result.Items)
			assert.Zero(t, result.TotalCount)
		})
	}
}

// authorizedListBoundaryFetch serves items (in the order supplied) in
// authorizedListBatchSize-sized batches, using each item's id (its index, as
// a string) as the resume cursor — the same cursor-is-an-opaque-index
// convention the other authorizedList tests in this file use.
func authorizedListBoundaryFetch(items []authorizedListTestItem) func(context.Context, string, int) (authorizedCandidatePage[authorizedListTestItem], error) {
	return func(_ context.Context, cursor string, limit int) (authorizedCandidatePage[authorizedListTestItem], error) {
		start := 0
		if cursor != "" {
			_, _ = fmt.Sscan(cursor, &start)
			start++
		}
		end := start + limit
		if end > len(items) {
			end = len(items)
		}
		next := ""
		if end < len(items) {
			next = items[end-1].id
		}
		return authorizedCandidatePage[authorizedListTestItem]{Items: items[start:end], NextCursor: next}, nil
	}
}

// buildAuthorizedListBoundaryItems returns visibleCount*3 items, interleaved
// two denied items for every one visible (allowed) item, in list
// order — item i is visible when (i+1)%3==0. That places a visible item at
// (or immediately after) nearly every page boundary the walk below will hit,
// which is exactly the position ptone/scion#1974's page-filled branch used
// to drop.
func buildAuthorizedListBoundaryItems(visibleCount int) (items []authorizedListTestItem, visible map[string]bool) {
	total := 3 * visibleCount
	items = make([]authorizedListTestItem, total)
	visible = make(map[string]bool, visibleCount)
	for i := 0; i < total; i++ {
		id := fmt.Sprint(i)
		items[i] = authorizedListTestItem{id: id}
		if (i+1)%3 == 0 {
			visible[id] = true
		}
	}
	return items, visible
}

// walkAuthorizedListPages drives authorizedList to exhaustion, following
// NextCursor from a fresh call each time (mirroring how each HTTP request
// re-runs authorizedList from the caller's cursor). It fails the test if any
// item is returned on more than one page.
func walkAuthorizedListPages(t *testing.T, items []authorizedListTestItem, visible map[string]bool, limit int) (pages [][]string) {
	t.Helper()
	fetch := authorizedListBoundaryFetch(items)
	read := func(_ context.Context, _ Identity, resources []Resource) ([]bool, error) {
		allowed := make([]bool, len(resources))
		for i, r := range resources {
			allowed[i] = visible[r.ID]
		}
		return allowed, nil
	}
	resource := func(item *authorizedListTestItem) Resource { return Resource{ID: item.id} }
	cursorFor := func(item *authorizedListTestItem) string { return item.id }

	seen := map[string]bool{}
	cursor := ""
	for {
		result, err := authorizedList(context.Background(), nil, cursor, limit, fetch, resource, cursorFor, read)
		require.NoError(t, err)
		ids := make([]string, len(result.Items))
		for i, it := range result.Items {
			require.False(t, seen[it.id], "item %s returned on more than one page", it.id)
			seen[it.id] = true
			ids[i] = it.id
		}
		pages = append(pages, ids)
		if result.NextCursor == "" {
			return pages
		}
		cursor = result.NextCursor
		require.Less(t, len(pages), len(items)+10, "did not converge within a sane number of pages")
	}
}

// TestAuthorizedListPageFillKeepsLastIncludedCursor is the ptone/scion#1974
// regression test. Before the fix, the page-filled branch of the page-fill
// pass advanced NextCursor to the next (not returned) allowed candidate
// instead of leaving it at the last included item; since store cursors are
// exclusive, the next page then resumed strictly after that unreturned item
// and it was never returned by any page. This walks a full multi-page list
// at several limits, including page counts that land exactly on a limit
// boundary and one past it, and checks the union of every page against the
// expected set, that no item repeats, that the page count matches
// ceil(visible/limit), and that two independent walks agree (cursor
// stability).
func TestAuthorizedListPageFillKeepsLastIncludedCursor(t *testing.T) {
	for _, limit := range []int{1, 10, 25, authorizedListBatchSize} {
		for _, extra := range []int{0, 1} { // exact multiple of limit, then one past it
			visibleCount := 2*limit + extra
			t.Run(fmt.Sprintf("limit=%d/visible=%d", limit, visibleCount), func(t *testing.T) {
				items, visible := buildAuthorizedListBoundaryItems(visibleCount)

				pages1 := walkAuthorizedListPages(t, items, visible, limit)
				pages2 := walkAuthorizedListPages(t, items, visible, limit)
				assert.Equal(t, pages1, pages2, "two independent walks over the same list must return identical pages")

				union := map[string]bool{}
				for i, page := range pages1 {
					if i < len(pages1)-1 {
						assert.Len(t, page, limit, "page %d of %d should be full", i+1, len(pages1))
					}
					for _, id := range page {
						union[id] = true
					}
				}
				assert.Equal(t, visible, union, "union of all pages must equal the expected visible set, with no drops and no denied items included")

				wantPages := (visibleCount + limit - 1) / limit
				assert.Equal(t, wantPages, len(pages1), "page count must equal ceil(visible count / limit)")
			})
		}
	}
}

func TestAuthorizedListStopsAfterInFlightCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	fetches := 0
	reads := 0
	_, err := authorizedList(ctx, nil, "", 1,
		func(_ context.Context, cursor string, _ int) (authorizedCandidatePage[authorizedListTestItem], error) {
			fetches++
			if cursor == "" {
				return authorizedCandidatePage[authorizedListTestItem]{Items: []authorizedListTestItem{{id: "one"}}, NextCursor: "next"}, nil
			}
			return authorizedCandidatePage[authorizedListTestItem]{}, nil
		}, func(*authorizedListTestItem) Resource { return Resource{} }, func(item *authorizedListTestItem) string { return item.id },
		func(_ context.Context, _ Identity, resources []Resource) ([]bool, error) {
			reads++
			cancel()
			return makeAllowed(len(resources)), nil
		})
	assert.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, fetches)
	assert.Equal(t, 1, reads)
}
