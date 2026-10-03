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
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// BuildWorkspaceStorageDescriptor builds the workspace storage descriptor a
// broker reports to the hub from its server.workspace_storage settings.
//
// backend is the configured backend name ("" means "local"). nfs is the
// validated NFS configuration with defaults applied, or nil when the backend
// is not nfs or its configuration is invalid; an nfs backend with a nil or
// share-less config is reported with a nil NFS block, which the hub treats
// as "not on a usable export". Agent workspaces are always placed on the
// first share (pkg/runtime nfsBackend.Resolve uses Shares[0]), so only that
// share is described. shareHealthy reports the last health check of a share
// by ID; nil reports the share as unhealthy.
func BuildWorkspaceStorageDescriptor(backend string, nfs *config.V1NFSConfig, shareHealthy func(shareID string) bool) *api.BrokerWorkspaceStorage {
	if backend == "" {
		backend = api.WorkspaceStorageBackendLocal
	}
	desc := &api.BrokerWorkspaceStorage{Backend: backend}
	if backend != api.WorkspaceStorageBackendNFS || nfs == nil || len(nfs.Shares) == 0 {
		return desc
	}
	share := nfs.Shares[0]
	desc.NFS = &api.BrokerNFSWorkspaceStorage{
		Server:      share.Server,
		Export:      share.Export,
		SubPathRoot: nfs.SubPathRoot,
		Healthy:     shareHealthy != nil && shareHealthy(share.ID),
	}
	return desc
}

// workspaceStorageDescriptor returns this broker's current workspace storage
// descriptor, with share health taken from the NFS mount reconciler's last
// check (false before the first check, or when no reconciler runs).
func (s *Server) workspaceStorageDescriptor() *api.BrokerWorkspaceStorage {
	return BuildWorkspaceStorageDescriptor(s.config.WorkspaceStorageBackend, s.config.NFSConfig, s.nfsShareHealthy)
}

// nfsShareHealthy reports whether the NFS mount reconciler's last check of
// shareID succeeded.
func (s *Server) nfsShareHealthy(shareID string) bool {
	if s.nfsMountReconciler == nil {
		return false
	}
	st, ok := s.nfsMountReconciler.ShareStatus(shareID)
	return ok && st.Healthy
}
