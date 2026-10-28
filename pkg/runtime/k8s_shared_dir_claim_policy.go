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

package runtime

import (
	"errors"
	"fmt"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
)

// sharedDirClaimPolicy decides how a NEW per-dir shared-dir claim is
// created on the local backend. It applies only when a claim must be
// created: an existing claim is always reused as it is, whatever the
// policy.
type sharedDirClaimPolicy struct {
	// Class is the resolved shared_dir_storage_class; empty means not set.
	Class string
}

// errSharedDirClaimUnresolved is returned by createSpec when no setting
// says how a new claim should be provisioned. The cluster default class
// usually cannot provide ReadWriteMany volumes, so such a claim would stay
// Pending and the agent pod with it.
var errSharedDirClaimUnresolved = errors.New("no storage class is set for a new shared-dir claim")

// sharedDirClaimPolicyFor returns the claim policy for a start's resolved
// Kubernetes settings (settings defaults already applied to k).
func sharedDirClaimPolicyFor(k *api.KubernetesConfig) sharedDirClaimPolicy {
	if k == nil {
		return sharedDirClaimPolicy{}
	}
	return sharedDirClaimPolicy{Class: k.SharedDirStorageClass}
}

// createSpec returns the storage class a new claim is created with, or
// errSharedDirClaimUnresolved when the policy does not allow creating one.
func (p sharedDirClaimPolicy) createSpec() (storageClassName *string, err error) {
	if p.Class == "" {
		return nil, errSharedDirClaimUnresolved
	}
	class := p.Class
	return &class, nil
}

// sharedDirClaimUnresolvedError is the error a start gets when a shared
// dir's claim does not exist and createSpec refuses to create it.
func sharedDirClaimUnresolvedError(dirName, namespace, claimName string) error {
	return &sharedDirClaimUnresolved{msg: fmt.Sprintf("shared dir %q: claim %s/%s does not exist and no shared_dir_storage_class is set, "+
		"so it would get the cluster default storage class, which usually cannot provide ReadWriteMany volumes; "+
		"no claim was created. Fix one of: configure server.project_storage backend nfs "+
		"(one share for all projects; recommended), or set shared_dir_storage_class to a ReadWriteMany class "+
		"(on the runtime or profile entry in settings.yaml, a broker instance's runtime_target, or the agent's kubernetes block). "+
		"If the cluster default class already supports ReadWriteMany: set shared_dir_storage_class to that class, "+
		"or configure server.project_storage backend nfs.",
		dirName, namespace, claimName)}
}

// sharedDirClaimUnresolved carries the refusal message and matches
// errSharedDirClaimUnresolved with errors.Is.
type sharedDirClaimUnresolved struct{ msg string }

func (e *sharedDirClaimUnresolved) Error() string { return e.msg }
func (e *sharedDirClaimUnresolved) Unwrap() error { return errSharedDirClaimUnresolved }

// sharedDirClaimOtherProjectError is the error a start gets when a shared
// dir's existing claim is labelled for a different project.
func sharedDirClaimOtherProjectError(dirName, namespace, claimName, otherProjectID, projectID string) error {
	return fmt.Errorf("shared dir %q: claim %s/%s is labelled for project %s (%s), not this project %s; "+
		"the start is refused and the claim is left unchanged. "+
		"Delete or rename that claim if it is no longer used by project %s",
		dirName, namespace, claimName, otherProjectID, projectkeys.LabelProjectID, projectID, otherProjectID)
}
