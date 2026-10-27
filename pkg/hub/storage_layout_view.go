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

package hub

import (
	"context"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// storageValuesAllowed reports whether the caller of ctx may see storage
// layout values (servers, exports, share ids, subpath roots, claim names,
// export identity markers): an unscoped hub admin, the same role check
// requireAdmin makes. Everyone else sees the differing fields by name
// only.
func storageValuesAllowed(ctx context.Context) bool {
	user, ok := GetIdentityFromContext(ctx).(UserIdentity)
	return ok && IsUnscopedLocalPlatformAdmin(user)
}

// storageIssueView is the projectfs.IssueView for the caller of ctx:
// ViewAdmin for a hub admin, else ViewPublic.
func storageIssueView(ctx context.Context) projectfs.IssueView {
	if storageValuesAllowed(ctx) {
		return projectfs.ViewAdmin
	}
	return projectfs.ViewPublic
}

// brokerStorageForViewer returns b with its workspace storage descriptor's
// values removed unless the caller of ctx may see them
// (storageValuesAllowed) or self is set (the broker reading its own
// record). The backend, health, homes and shared-dir placement are kept;
// the nfs server, export, subpath root and export identity marker and the
// volume claim and subpath root are omitted, and the descriptor is marked
// Redacted so a client can tell redaction from an empty value.
func brokerStorageForViewer(ctx context.Context, b store.RuntimeBroker, self bool) store.RuntimeBroker {
	if b.WorkspaceStorage == nil || self || storageValuesAllowed(ctx) {
		return b
	}
	ws := *b.WorkspaceStorage
	if ws.NFS != nil {
		ws.NFS = &api.BrokerNFSWorkspaceStorage{Healthy: ws.NFS.Healthy}
	}
	if ws.Volume != nil {
		ws.Volume = &api.BrokerVolumeWorkspaceStorage{}
	}
	ws.Redacted = true
	b.WorkspaceStorage = &ws
	return b
}
