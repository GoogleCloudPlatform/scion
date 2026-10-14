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
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGetLinkedProject: only a 404 means "not linked". Any other failure is
// returned, even when its message or request ID happens to contain
// "not found" or "404".
func TestGetLinkedProject(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		wantID  string
		wantErr bool
	}{
		{
			name: "found",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"p1","name":"demo"}`))
			},
			wantID: "p1",
		},
		{
			name: "not found",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"project not found"}}`))
			},
		},
		{
			name: "server error mentioning not found",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-Request-ID", "req-404")
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":{"code":"internal_error","message":"template not found"}}`))
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(tt.handler)
			defer srv.Close()
			client, err := hubclient.New(srv.URL)
			require.NoError(t, err)

			project, err := getLinkedProject(context.Background(), client, "p1")
			if tt.wantErr {
				require.Error(t, err)
				assert.Nil(t, project)
				return
			}
			require.NoError(t, err)
			if tt.wantID == "" {
				assert.Nil(t, project)
				return
			}
			require.NotNil(t, project)
			assert.Equal(t, tt.wantID, project.ID)
		})
	}
}
