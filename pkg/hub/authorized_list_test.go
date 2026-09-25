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
