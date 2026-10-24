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

package runtimebroker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/stretchr/testify/require"
)

func TestInboxRedemption_UnexpectedResponseDoesNotPanic(t *testing.T) {
	for _, status := range []int{201, 204, 206, 301, 308, 200} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				if status != http.StatusNoContent {
					_, _ = io.WriteString(w, "alpha")
				}
			}))
			defer srv.Close()
			c, err := hubclient.New(srv.URL)
			require.NoError(t, err)
			d := &inboxDelivery{spool: t.TempDir()}
			sum := sha256.Sum256([]byte("alpha"))
			wire := messages.WireAttachment{Name: "a.txt", Size: 5, SHA256: hex.EncodeToString(sum[:]), URL: "/api/v1/attachment-deliveries/fixture"}
			var path, reason string
			require.NotPanics(t, func() {
				path, reason = d.redeemOne(context.Background(), c.(hubclient.AttachmentDeliveryFetcher), wire)
			})
			if status == http.StatusOK {
				require.Empty(t, reason)
				data, err := os.ReadFile(path)
				require.NoError(t, err)
				require.Equal(t, "alpha", string(data))
			} else {
				require.Empty(t, path)
				require.Equal(t, reasonNotDownloaded, reason)
				entries, err := os.ReadDir(d.spool)
				require.NoError(t, err)
				require.Empty(t, entries)
			}
		})
	}
}
