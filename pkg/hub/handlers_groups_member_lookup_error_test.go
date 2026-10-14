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

//go:build !no_sqlite

package hub

// POST /groups/{G}/members with memberType group: a store fault while
// resolving the child group (by ID, or by the slug fallback) is reported as
// a server error, not as "group not found" and not as success.
//
// Run: go test ./pkg/hub/ -run 'TestAddGroupMember_GroupLookupError' -count=1

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var errGroupLookupOutage = errors.New("simulated group store outage")

// failingGroupLookupStore fails GetGroup for idRef and GetGroupBySlug for
// slugRef. All other calls delegate to the embedded store.
type failingGroupLookupStore struct {
	store.Store
	idRef   string
	slugRef string
}

func (f *failingGroupLookupStore) GetGroup(ctx context.Context, id string) (*store.Group, error) {
	if f.idRef != "" && id == f.idRef {
		return nil, errGroupLookupOutage
	}
	return f.Store.GetGroup(ctx, id)
}

func (f *failingGroupLookupStore) GetGroupBySlug(ctx context.Context, slug string) (*store.Group, error) {
	if f.slugRef != "" && slug == f.slugRef {
		return nil, errGroupLookupOutage
	}
	return f.Store.GetGroupBySlug(ctx, slug)
}

func TestAddGroupMember_GroupLookupError(t *testing.T) {
	cases := []struct {
		name  string
		store func(child *store.Group) *failingGroupLookupStore
		ref   func(child *store.Group) string
	}{
		{
			name:  "by_id",
			store: func(c *store.Group) *failingGroupLookupStore { return &failingGroupLookupStore{idRef: c.ID} },
			ref:   func(c *store.Group) string { return c.ID },
		},
		{
			name:  "slug_fallback",
			store: func(c *store.Group) *failingGroupLookupStore { return &failingGroupLookupStore{slugRef: c.Slug} },
			ref:   func(c *store.Group) string { return c.Slug },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, s := testServer(t)
			ctx := context.Background()

			parent := &store.Group{ID: tid("lookuperr-parent"), Name: "Lookup Err Parent", Slug: "lookuperr-parent"}
			require.NoError(t, s.CreateGroup(ctx, parent))
			child := &store.Group{ID: tid("lookuperr-child"), Name: "Lookup Err Child", Slug: "lookuperr-child"}
			require.NoError(t, s.CreateGroup(ctx, child))

			fs := tc.store(child)
			fs.Store = s
			srv.store = fs

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/groups/"+parent.ID+"/members",
				map[string]interface{}{"memberType": "group", "memberId": tc.ref(child), "role": "member"})
			assert.GreaterOrEqual(t, rec.Code, 500, "store fault must be a server error: %s", rec.Body.String())
			assert.Less(t, rec.Code, 600, rec.Body.String())

			members, err := s.GetGroupMembers(ctx, parent.ID)
			require.NoError(t, err)
			assert.Empty(t, members, "no membership may be created on a lookup fault")
		})
	}
}
