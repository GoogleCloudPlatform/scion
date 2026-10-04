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

package agent

import (
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// homeStorageGID is the group of NFS homes: the pod's RunAsGroup, which the
// export's group must match.
const homeStorageGID = 1000

// homeLeafHost performs the host-side steps of the broker leaf mode, through
// the broker's own mount of the export. Tests replace homeLeafOps to check
// which steps run.
type homeLeafHost interface {
	// CheckMount fails unless hostBase is a mounted NFS file system.
	CheckMount(hostBase string) error
	// EnsureHome creates the agent directory agentDirRel under hostBase
	// (if absent) and the home directory homeName in it, owned by the
	// export group gid.
	EnsureHome(hostBase, agentDirRel, homeName string, gid int) error
}

var homeLeafOps homeLeafHost = osHomeLeafHost{}

// prepareHomeStorage turns a resolved home storage plan into the runtime's
// home description. In broker leaf mode it first checks the broker's mount
// of the export and creates the agent and home directories through it; in
// pod leaf mode it does nothing on the host (the home-leaf init container
// creates the home directory). A local plan returns nil.
func prepareHomeStorage(plan *homeStoragePlan, projectID, slug string) (*runtime.HomeStorageRealization, error) {
	if plan == nil || plan.Backend != config.HomeStorageBackendNFS {
		return nil, nil
	}
	agentDir, home, err := runtime.NFSHomeSubPaths(plan.SubPathRoot, projectID, slug, plan.AgentID)
	if err != nil {
		return nil, homeStorageUnavailable("%v", err)
	}
	if plan.Leaf == config.HomeStorageLeafBroker {
		if plan.MountRoot == "" {
			return nil, homeStorageUnavailable("home_storage_leaf broker needs shared_dir_storage nfs mount_root")
		}
		hostBase := filepath.Join(plan.MountRoot, plan.ShareID)
		if err := homeLeafOps.CheckMount(hostBase); err != nil {
			return nil, homeStorageUnavailable("host mount %s missing: %v", hostBase, err)
		}
		if err := homeLeafOps.EnsureHome(hostBase, agentDir, filepath.Base(home), homeStorageGID); err != nil {
			return nil, homeStorageUnavailable("creating the agent home through the host mount %s: %v", hostBase, err)
		}
	}
	return &runtime.HomeStorageRealization{
		PVClaimName:            plan.PVClaimName,
		SubPathRoot:            plan.SubPathRoot,
		ProjectID:              projectID,
		AgentSlug:              slug,
		AgentID:                plan.AgentID,
		Leaf:                   plan.Leaf,
		GID:                    homeStorageGID,
		StopGraceSeconds:       plan.Settings.StopGrace(),
		TerminationWaitSeconds: plan.Settings.TerminationWait(),
		SkeletonMaxBytes:       plan.Settings.SkeletonMax(),
	}, nil
}
