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
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// sharedDirClaimLabel is the label ensureProjectRWXClaim sets on a per-dir
// shared-dir claim, naming the shared dir.
const sharedDirClaimLabel = "scion.shared-dir"

// sharedDirClaimPrefix is the name prefix of every per-dir shared-dir claim
// (sharedDirPVCName: scion-shared-<project>-<dir>).
const sharedDirClaimPrefix = "scion-shared-"

// SharedDirClaimRelease is the outcome of ReleaseProjectSharedDirClaims in
// one namespace.
type SharedDirClaimRelease struct {
	Namespace string
	// Deleted are the claims deleted.
	Deleted []string
	// Failed maps a claim that could not be deleted to the error.
	Failed map[string]error
	// Unlabelled are the per-dir shared-dir claims of the project's name
	// that carry no project-id label. They are never deleted (a name is not
	// an identity); they are listed for an operator to check.
	Unlabelled []string
	// InUse are the project's claims kept because an existing pod in the
	// namespace references them (a pod of a forced delete's agent still
	// running, or another project's pod bound to the claim).
	InUse []string
}

// ReleaseProjectSharedDirClaims deletes, in this runtime's namespace, the
// per-dir shared-dir PersistentVolumeClaims of the deleted project
// projectID (ptone/scion#5835). A claim is deleted only when it carries
// all three of:
//
//   - the project-id label (projectkeys.LabelProjectID) equal to projectID,
//     as ensureProjectRWXClaim writes it;
//   - a scion.shared-dir label naming a valid shared dir;
//   - a name of the form scion-shared-<...>-<that dir>.
//
// Never deleted: a claim an existing pod in the namespace references
// (returned in InUse), a claim without the project-id label (claims of the
// project's name projectName without it are returned in Unlabelled), a
// claim named in keep (the layout's shared claim), and anything matched by
// name only. A failure to list the namespace's pods releases nothing. Each deletion is preconditioned on the claim's UID, so a claim
// re-created meanwhile is not deleted. It is best-effort: a claim that
// cannot be deleted is reported in Failed and the others are still
// deleted; a failure to list the claims is the returned error.
func (r *KubernetesRuntime) ReleaseProjectSharedDirClaims(ctx context.Context, projectID, projectName string, keep []string) (SharedDirClaimRelease, error) {
	ns := r.DefaultNamespace
	out := SharedDirClaimRelease{Namespace: ns, Failed: map[string]error{}}
	if projectID == "" {
		return out, errors.New("project ID is required")
	}
	if r.Client == nil || r.Client.Clientset == nil {
		return out, errors.New("kubernetes client is not configured")
	}
	kept := map[string]bool{}
	for _, k := range keep {
		if k != "" {
			kept[k] = true
		}
	}
	claims := r.Client.Clientset.CoreV1().PersistentVolumeClaims(ns)
	list, err := claims.List(ctx, metav1.ListOptions{LabelSelector: sharedDirClaimLabel})
	if err != nil {
		return out, fmt.Errorf("list shared-dir claims in namespace %q: %w", ns, err)
	}
	inUse, err := r.claimsReferencedByPods(ctx, ns)
	if err != nil {
		return out, err
	}
	for i := range list.Items {
		pvc := &list.Items[i]
		dir := pvc.Labels[sharedDirClaimLabel]
		if kept[pvc.Name] || !sharedDirClaimShape(pvc.Name, dir) {
			continue
		}
		id, hasID := pvc.Labels[projectkeys.LabelProjectID]
		switch {
		case hasID && id == projectID && inUse[pvc.Name]:
			out.InUse = append(out.InUse, pvc.Name)
		case hasID && id == projectID:
			uid := pvc.UID
			err := claims.Delete(ctx, pvc.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
			switch {
			case err == nil:
				out.Deleted = append(out.Deleted, pvc.Name)
			case apierrors.IsNotFound(err):
				// Gone meanwhile: nothing to do.
			default:
				out.Failed[pvc.Name] = err
			}
		case !hasID && projectName != "" &&
			(pvc.Labels[projectkeys.LabelProject] == projectName || pvc.Name == sharedDirPVCName(projectName, dir)):
			out.Unlabelled = append(out.Unlabelled, pvc.Name)
		}
	}
	sort.Strings(out.Deleted)
	sort.Strings(out.Unlabelled)
	sort.Strings(out.InUse)
	return out, nil
}

// claimsReferencedByPods returns the claims that an existing pod in ns
// references in a volume, whatever the pod's phase: a claim a pod still
// mounts is never released (pvc-protection would only defer its deletion
// until the pod ends, and then destroy the data). Known narrow window: a
// pod created between this listing and the claim deletes is not seen; the
// hub refuses new starts once the project row is gone.
func (r *KubernetesRuntime) claimsReferencedByPods(ctx context.Context, ns string) (map[string]bool, error) {
	pods, err := r.Client.Clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list pods in namespace %q: %w", ns, err)
	}
	used := map[string]bool{}
	for i := range pods.Items {
		for _, v := range pods.Items[i].Spec.Volumes {
			if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName != "" {
				used[v.PersistentVolumeClaim.ClaimName] = true
			}
		}
	}
	return used, nil
}

// sharedDirClaimShape reports whether name is scion-shared-<...>-<dir> for
// a valid shared dir name dir, with a non-empty middle.
func sharedDirClaimShape(name, dir string) bool {
	if dir == "" || api.ValidateSharedDirs([]api.SharedDir{{Name: dir}}) != nil {
		return false
	}
	rest, ok := strings.CutPrefix(name, sharedDirClaimPrefix)
	if !ok {
		return false
	}
	middle, ok := strings.CutSuffix(rest, "-"+dir)
	return ok && middle != ""
}
