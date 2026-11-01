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
// export identity markers): a user holding hub.config.read, the permission
// that already reads the hub's server configuration (where those values
// are set), decided by the authorization pipeline. Everyone else sees the
// differing fields by name only. The permission is evaluated at hub scope
// (resource hub/hub), so a project-scoped grant never admits it. Anything
// other than an allowed decision, including an authorization or store
// error (the decision is then a deny), a caller that is not a user, or a
// missing authorization service, yields false: the public view.
func (s *Server) storageValuesAllowed(ctx context.Context) bool {
	user, ok := GetIdentityFromContext(ctx).(UserIdentity)
	if !ok || isNilIdentity(user) || s.authzService == nil {
		return false
	}
	return s.authzService.Decide(ctx, AuthzRequest{
		Principal:  principalContextForIdentity(user),
		Credential: credentialContextForIdentity(user),
		Resource:   Resource{Type: "hub", ID: "hub"},
		Action:     Action("read"),
		Permission: "hub.config.read",
	}).Allowed
}

// storageIssueView is the projectfs.IssueView for the caller of ctx:
// ViewAdmin when it may see storage values, else ViewPublic.
func (s *Server) storageIssueView(ctx context.Context) projectfs.IssueView {
	if s.storageValuesAllowed(ctx) {
		return projectfs.ViewAdmin
	}
	return projectfs.ViewPublic
}

// brokerStorageForViewer returns b with its workspace storage descriptor's
// values removed unless the caller of ctx may see them
// (storageValuesAllowed) or self is set (the broker reading its own
// record). See redactBrokerStorage.
func (s *Server) brokerStorageForViewer(ctx context.Context, b store.RuntimeBroker, self bool) store.RuntimeBroker {
	if b.WorkspaceStorage == nil || self {
		return b
	}
	return redactBrokerStorage(b, s.storageValuesAllowed(ctx))
}

// redactBrokerStorage returns b unchanged when show is set; otherwise its
// workspace storage descriptor keeps the backend, health, homes and
// shared-dir placement, omits the nfs server, export, subpath root and
// export identity marker and the volume claim and subpath root, and is
// marked Redacted so a client can tell redaction from an empty value. A
// list handler decides show once per request (storageValuesAllowed) and
// applies it to every row.
func redactBrokerStorage(b store.RuntimeBroker, show bool) store.RuntimeBroker {
	if b.WorkspaceStorage == nil || show {
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
