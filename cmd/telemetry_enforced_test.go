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
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheckDisableTelemetryAllowed(t *testing.T) {
	t.Run("refuses when lockedBy hub", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/projects/proj-1/settings/resolved" {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"project":{},"settings":{"scion.io/telemetry-enabled":{"projectSet":false,"projectValue":null,"hubDefault":"present","hubValue":true,"lockedBy":"hub","effective":true,"storedValue":null}}}`))
				return
			}
			http.NotFound(w, r)
		}))
		defer srv.Close()

		c, err := hubclient.New(srv.URL)
		require.NoError(t, err)

		err = checkDisableTelemetryAllowed(c, "proj-1")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--disable-telemetry")
		assert.Contains(t, err.Error(), "enforced by the hub operator")
	})

	t.Run("allows when unlocked", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"project":{},"settings":{"scion.io/telemetry-enabled":{"projectSet":false,"projectValue":null,"hubDefault":"present","hubValue":true,"lockedBy":null,"effective":null,"storedValue":null}}}`))
		}))
		defer srv.Close()

		c, err := hubclient.New(srv.URL)
		require.NoError(t, err)

		require.NoError(t, checkDisableTelemetryAllowed(c, "proj-1"))
	})

	t.Run("falls through on 404 older hub", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		defer srv.Close()

		c, err := hubclient.New(srv.URL)
		require.NoError(t, err)

		require.NoError(t, checkDisableTelemetryAllowed(c, "proj-1"))
	})
}
