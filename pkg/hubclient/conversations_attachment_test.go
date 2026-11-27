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

package hubclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConversationService_DownloadAttachment_DirectStream(t *testing.T) {
	var gotPath string
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("direct bytes"))
	}))
	defer srv.Close()

	c, err := New(srv.URL, WithBearerToken("secret-hub-token"))
	require.NoError(t, err)

	rc, err := c.Conversations().DownloadAttachment(context.Background(), "conv-1", "msg-1", "att-1")
	require.NoError(t, err)
	defer func() { _ = rc.Close() }()

	data, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, "/api/v1/conversations/conv-1/messages/msg-1/attachments/att-1", gotPath)
	assert.Equal(t, "Bearer secret-hub-token", gotAuth)
	assert.Equal(t, "direct bytes", string(data))
}

func TestConversationService_DownloadAttachment_RedirectNoCreds(t *testing.T) {
	var objAuth string
	objSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		objAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte("object store bytes"))
	}))
	defer objSrv.Close()

	hubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, objSrv.URL+"/blob/123", http.StatusFound)
	}))
	defer hubSrv.Close()

	c, err := New(hubSrv.URL, WithBearerToken("secret-hub-token"))
	require.NoError(t, err)

	rc, err := c.Conversations().DownloadAttachment(context.Background(), "conv-1", "msg-1", "att-1")
	require.NoError(t, err)
	defer func() { _ = rc.Close() }()

	data, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, "object store bytes", string(data))
	assert.Empty(t, objAuth, "hub credentials must never be forwarded to object store")
}

func TestConversationService_DownloadAttachment_RedirectFallbackStream(t *testing.T) {
	// Object store fails with 503; client should fall back to hub ?stream=1.
	objSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer objSrv.Close()

	var seenQueries []string
	hubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenQueries = append(seenQueries, r.URL.RawQuery)
		if r.URL.Query().Get("stream") == "1" {
			_, _ = w.Write([]byte("streamed fallback bytes"))
			return
		}
		http.Redirect(w, r, objSrv.URL+"/blob/123", http.StatusFound)
	}))
	defer hubSrv.Close()

	c, err := New(hubSrv.URL)
	require.NoError(t, err)

	rc, err := c.Conversations().DownloadAttachment(context.Background(), "conv-1", "msg-1", "att-1")
	require.NoError(t, err)
	defer func() { _ = rc.Close() }()

	data, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, "streamed fallback bytes", string(data))
	assert.Equal(t, []string{"", "stream=1"}, seenQueries)
}

func TestConversationService_DownloadAttachment_InvalidRedirectScheme(t *testing.T) {
	var calls int
	hubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Location", "file:///etc/passwd")
		w.WriteHeader(http.StatusFound)
	}))
	defer hubSrv.Close()

	c, err := New(hubSrv.URL)
	require.NoError(t, err)

	_, err = c.Conversations().DownloadAttachment(context.Background(), "conv-1", "msg-1", "att-1")
	require.Error(t, err)
	assert.True(t, errors.Is(err, errArtifactRedirect), "expected errArtifactRedirect, got %v", err)
	assert.Equal(t, 1, calls, "invalid redirect scheme must not trigger ?stream=1 retry")
}

func TestConversationService_DownloadAttachment_NotFound(t *testing.T) {
	hubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"Attachment not found"}}`))
	}))
	defer hubSrv.Close()

	c, err := New(hubSrv.URL)
	require.NoError(t, err)

	_, err = c.Conversations().DownloadAttachment(context.Background(), "conv-1", "msg-1", "att-1")
	require.Error(t, err)
	var apiErr *apiclient.APIError
	require.True(t, errors.As(err, &apiErr))
	assert.Equal(t, http.StatusNotFound, apiErr.StatusCode)
}
