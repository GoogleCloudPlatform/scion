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
	"reflect"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/gcp"
)

// TestHubWorkspaceTransfers_UseWorkspaceSyncList pins the gcp transfers the
// hub's workspace legs use by default: the create-time upload of the hub's
// project workspace and the sync-back download into it both go through the
// workspace sync list (content plus git data; never repository
// configuration, hooks or any .scion entry), and the download is staged and
// applied without following symlinks. The transfers' behaviour is tested in
// pkg/gcp.
func TestHubWorkspaceTransfers_UseWorkspaceSyncList(t *testing.T) {
	srv, _ := testServer(t)
	same := func(a, b any) bool { return reflect.ValueOf(a).Pointer() == reflect.ValueOf(b).Pointer() }
	if !same(srv.hubWorkspaceDownloader(), gcp.SyncWorkspaceFromGCS) {
		t.Error("hub workspace sync-back download is not gcp.SyncWorkspaceFromGCS")
	}
	if !same(syncToGCSForWorkspaceUpload, gcp.SyncWorkspaceToGCS) {
		t.Error("hub create-time workspace upload is not gcp.SyncWorkspaceToGCS")
	}
}
