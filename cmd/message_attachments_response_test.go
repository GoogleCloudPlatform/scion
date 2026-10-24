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
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/require"
)

func TestAttachUpload_UnexpectedResponseDoesNotPanic(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusNoContent, http.StatusTemporaryRedirect, http.StatusCreated} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/api/v1/chat/attachments" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				if status != http.StatusNoContent {
					_, _ = io.WriteString(w, `{"attachments":[{"id":"att-1","name":"a.txt","size":5}]}`)
				}
			}))
			defer srv.Close()
			upload := &attachmentUpload{
				uploader: attachHubClient(t, srv).(hubclient.AttachmentUploader),
				paths:    []string{writeAttachFile(t, t.TempDir(), "a.txt", "alpha")},
			}
			var ids []string
			var err error
			require.NotPanics(t, func() {
				ids, err = upload.ids(context.Background())
			})
			if status == http.StatusCreated {
				require.NoError(t, err)
				require.Equal(t, []string{"att-1"}, ids)
			} else {
				require.ErrorContains(t, err, strconv.Itoa(status))
				require.ErrorContains(t, err, "attachment upload failed")
				require.Empty(t, ids)
			}
		})
	}
}
