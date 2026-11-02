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

import (
	"context"
	"database/sql"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestChatSearch_InvalidCursorIsBadRequest checks that a malformed cursor is
// reported as a client error, not a server error.
func TestChatSearch_InvalidCursorIsBadRequest(t *testing.T) {
	srv, s := testServer(t)
	dbp, ok := s.(interface{ DB() *sql.DB })
	require.True(t, ok, "test store must expose DB()")
	wcs := NewWebChatStore(dbp.DB(), "sqlite")
	require.NoError(t, wcs.Init())
	srv.SetWebChatStore(wcs)

	ctx := context.Background()
	proj := &store.Project{ID: tid("search-bad-cursor"), Name: "search-bad-cursor", Slug: "search-bad-cursor",
		Created: time.Now(), Updated: time.Now()}
	require.NoError(t, s.CreateProject(ctx, proj))
	srv.createProjectMembersGroup(ctx, proj)
	base := "/api/v1/chat/search?q=needle&projectId=" + proj.ID + "&cursor="

	rec := doRequest(t, srv, http.MethodGet, base+url.QueryEscape("not-a-time|some-id"), nil)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), ErrCodeInvalidCursor)

	// A well-formed cursor still searches normally.
	rec = doRequest(t, srv, http.MethodGet, base+url.QueryEscape("2026-10-01T04:00:00Z|some-id"), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}
