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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/k8s"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
)

// Per-run names for the per-agent Secrets and SecretProviderClass
// (ptone/scion#3101).
//
// A start that carries a run ID names its objects after the run, so the
// objects of two runs of one agent name never share a name. A later run's
// pre-clean then has no reason to delete an earlier run's objects to make
// room, and it does not: objects of another run that has no pod are left
// to that run's own cleanup, to a delete naming that run, or, once stale,
// to the age-based sweep (staleRunObject). A start without a run ID keeps
// the fixed names scion-agent-<pod> and scion-auth-<pod>.
//
// The per-run prefixes differ from the fixed ones, so a per-run name can
// never equal a fixed name (for example the fixed name of an agent whose
// pod name happens to end in a run token).

const (
	// runSecretPrefix names the env/variable/file Secret and the
	// SecretProviderClass of a run (distinct kinds, as with the fixed
	// names).
	runSecretPrefix = "scion-run-secret-"
	// runAuthPrefix names the ResolvedAuth file Secret of a run.
	runAuthPrefix = "scion-run-auth-"

	// runTokenLen is the length of the run token (hex of sha256 of the
	// run ID) ending every per-run name.
	runTokenLen = 16
	// podHashLen is the length of the pod-name hash a truncated pod name
	// carries.
	podHashLen = 8
	// maxObjectNameLen is the longest Secret or SecretProviderClass name
	// (a DNS-1123 subdomain).
	maxObjectNameLen = 253

	// annotationPodName holds the pod name on every per-run object, so the
	// pod-gone sweeper finds the pod without parsing a possibly truncated
	// object name. (The scion.agent label holds "true", not the pod name.)
	annotationPodName = "scion.pod_name"

	// annotationStartDeadlineOffset holds, on every per-run object, the
	// whole seconds from the object's creation to the creating start's
	// deadline, or startDeadlineNone when the start had no deadline. Being
	// relative to creationTimestamp, it is compared in server time only.
	annotationStartDeadlineOffset = "scion.start_deadline_offset"
	// startDeadlineNone marks a per-run object whose start had no deadline
	// (a local CLI start, or a synchronous hub start, which the hub bounds
	// by dropping the connection rather than with a deadline).
	startDeadlineNone = "none"

	// minStaleRunObjectAge is the youngest a per-run object of another run
	// can be when the sweep removes it, whatever its start's deadline. A
	// start without a deadline is assumed over after this long.
	minStaleRunObjectAge = time.Hour
	// staleRunObjectMargin is added to a start's deadline before its
	// objects count as stale, and covers the skew between broker and
	// server clocks in the verify trigger (verifyStartObjectsAfter).
	staleRunObjectMargin = 15 * time.Minute

	// maxStartDeadlineOffsetSeconds bounds an offset the sweep accepts. A
	// larger value cannot be a real start deadline and is treated as
	// malformed (never stale by age).
	maxStartDeadlineOffsetSeconds = 10 * 365 * 24 * 3600
)

// neverVerifyStartObjects is a verify trigger no start reaches.
const neverVerifyStartObjects = time.Duration(math.MaxInt64)

// k8sObjectNames are the names of one start's per-agent objects.
type k8sObjectNames struct {
	Secret string // env/variable/file Secret
	Auth   string // ResolvedAuth file Secret
	SPC    string // SecretProviderClass (GKE secrets path)
}

// k8sAgentObjectNames returns the names of the per-agent objects of pod
// podName for run runID. With no run ID they are the fixed names, as
// before run IDs existed. With one, they are
// <prefix><podpart>-<token>, where token is the first runTokenLen hex
// characters of sha256(runID) and podpart is the pod name, or, when the
// name would exceed maxObjectNameLen, the pod name truncated (trailing "."
// and "-" trimmed, to stay a DNS-1123 subdomain) followed by "-" and
// podHashLen hex characters of sha256(podName).
//
// Two equal per-run names have equal tokens, so the same run ID up to a
// 64-bit hash collision, and a run belongs to one agent. Even for one run
// ID, two pod names that share a truncated prefix differ in the pod hash.
// A collision is still safe: the object carries its own run's label, and
// replaceExistingAgentObject refuses another run's object (run_conflict)
// rather than delete it.
func k8sAgentObjectNames(podName, runID string) k8sObjectNames {
	if runID == "" {
		return k8sObjectNames{
			Secret: agentSecretPrefix + podName,
			Auth:   agentAuthSecretPrefix + podName,
			SPC:    agentSecretPrefix + podName,
		}
	}
	token := shortHash(runID, runTokenLen)
	secret := perRunName(runSecretPrefix, podName, token)
	return k8sObjectNames{
		Secret: secret,
		Auth:   perRunName(runAuthPrefix, podName, token),
		SPC:    secret,
	}
}

func perRunName(prefix, podName, token string) string {
	name := prefix + podName + "-" + token
	if len(name) <= maxObjectNameLen {
		return name
	}
	keep := maxObjectNameLen - len(prefix) - 1 - podHashLen - 1 - runTokenLen
	part := strings.TrimRight(podName[:keep], ".-")
	return prefix + part + "-" + shortHash(podName, podHashLen) + "-" + token
}

func shortHash(s string, n int) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:n]
}

// isPerRunObjectName reports whether name is a per-run object name.
func isPerRunObjectName(name string) bool {
	return strings.HasPrefix(name, runSecretPrefix) || strings.HasPrefix(name, runAuthPrefix)
}

// perRunAnnotations returns the annotations of a per-run object of pod
// podName created now by a start whose context is ctx.
func perRunAnnotations(ctx context.Context, podName string, now time.Time) map[string]string {
	offset := startDeadlineNone
	if deadline, ok := ctx.Deadline(); ok {
		secs := math.Ceil(deadline.Sub(now).Seconds())
		if secs < 0 {
			secs = 0
		}
		offset = strconv.FormatInt(int64(secs), 10)
	}
	return map[string]string{
		annotationPodName:             podName,
		annotationStartDeadlineOffset: offset,
	}
}

// staleRunObjectAge returns how old a per-run object whose
// annotationStartDeadlineOffset is value must be before the sweep may
// remove it: max(minStaleRunObjectAge, offset + staleRunObjectMargin), or
// minStaleRunObjectAge for startDeadlineNone. A missing or malformed value
// returns false: such an object is never removed by age.
func staleRunObjectAge(value string) (time.Duration, bool) {
	if value == startDeadlineNone {
		return minStaleRunObjectAge, true
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n < 0 || n > maxStartDeadlineOffsetSeconds || strconv.FormatInt(n, 10) != value {
		return 0, false
	}
	return max(minStaleRunObjectAge, time.Duration(n)*time.Second+staleRunObjectMargin), true
}

// verifyStartObjectsAfter returns how long after its first object create a
// start whose objects carry the offset value must check that they still
// exist before creating its pod (Run, verifyStartObjects): the youngest age
// at which the sweep could remove them, less staleRunObjectMargin for skew
// between the broker's clock (which measures the start) and the server's
// (which stamps creationTimestamp). That is max(45m, offset), or 45m with
// no deadline. A start younger than that cannot have been swept, so a
// normal start makes no extra API calls.
func verifyStartObjectsAfter(value string) time.Duration {
	age, ok := staleRunObjectAge(value)
	if !ok {
		return neverVerifyStartObjects
	}
	return age - staleRunObjectMargin
}

// staleRunObject reports whether the per-run object obj is old enough for
// the sweep to remove, by its creationTimestamp and its
// annotationStartDeadlineOffset (see staleRunObjectAge). Callers decide
// that obj belongs to another run with no pod; this only judges age. An
// object with no creationTimestamp is never stale.
func (r *KubernetesRuntime) staleRunObject(obj metav1.Object) bool {
	if !isPerRunObjectName(obj.GetName()) {
		return false
	}
	age, ok := staleRunObjectAge(obj.GetAnnotations()[annotationStartDeadlineOffset])
	if !ok {
		return false
	}
	created := obj.GetCreationTimestamp().Time
	if created.IsZero() {
		return false
	}
	return r.now().After(created.Add(age))
}

// now is the runtime's wall clock (a test seam).
func (r *KubernetesRuntime) now() time.Time {
	if r.nowFn != nil {
		return r.nowFn()
	}
	return time.Now()
}

// since is time.Since on the monotonic clock (a test seam).
func (r *KubernetesRuntime) since(t time.Time) time.Duration {
	if r.sinceFn != nil {
		return r.sinceFn(t)
	}
	return time.Since(t)
}

// objectAnnotations returns the annotations of a per-agent object of pod
// podName created now: perRunAnnotations for a start with a run ID, nil
// (no annotations, as before) without one.
func (r *KubernetesRuntime) objectAnnotations(ctx context.Context, podName, runID string) map[string]string {
	if runID == "" {
		return nil
	}
	return perRunAnnotations(ctx, podName, r.now())
}

// errStartObjectsGone is the fixed text of the start error returned when an
// object the start created is gone or replaced before its pod create.
const errStartObjectsGone = "the agent's secrets were removed before its pod was created; retry the start"

// verifyStartObjects checks, immediately before the pod create, that every
// Secret and SecretProviderClass the start created (handles, with the UIDs
// from their creates) still exists with that UID. Each is listed by a field
// selector on its name, within the list permission the runtime already
// needs. A missing or replaced object, or a failed list, fails the start
// with a fixed-text error, so Run never creates a pod that cannot mount its
// secrets; the start's deferred cleanup removes what is left, and a retry
// recreates them.
//
// Run calls it only for a start older than verifyStartObjectsAfter, the
// youngest age at which the per-run object sweep could have removed its
// objects. Residual case: a start with no deadline (a local CLI start)
// that blocks for more than that before its pod create can have its
// objects swept by another start or delete of the agent; it then fails
// here.
func (r *KubernetesRuntime) verifyStartObjects(ctx context.Context, handles []api.ResourceHandle) error {
	for _, h := range handles {
		opts := metav1.ListOptions{FieldSelector: fields.OneTermEqualSelector("metadata.name", h.Name).String()}
		var uids []types.UID
		switch h.Kind {
		case api.ResourceKindSecret:
			list, err := r.Client.Clientset.CoreV1().Secrets(h.Namespace).List(ctx, opts)
			if err != nil {
				return opaqueStartError(errStartObjectsGone, err, "kind", h.Kind, "name", h.Name, "namespace", h.Namespace)
			}
			for _, s := range list.Items {
				if s.Name == h.Name {
					uids = append(uids, s.UID)
				}
			}
		case api.ResourceKindSecretProviderClass:
			list, err := r.Client.Dynamic().Resource(k8s.SecretProviderClassGVR).Namespace(h.Namespace).List(ctx, opts)
			if err != nil {
				return opaqueStartError(errStartObjectsGone, err, "kind", h.Kind, "name", h.Name, "namespace", h.Namespace)
			}
			for _, spc := range list.Items {
				if spc.GetName() == h.Name {
					uids = append(uids, spc.GetUID())
				}
			}
		default:
			continue
		}
		if len(uids) != 1 || string(uids[0]) != h.UID {
			return opaqueStartError(errStartObjectsGone, errors.New("object gone or replaced"),
				"kind", h.Kind, "name", h.Name, "namespace", h.Namespace, "uid", h.UID)
		}
	}
	return nil
}

// perRunNameMatches reports whether objectName is the per-run name of kind
// ("Secret" or "SecretProviderClass") for pod podName and run runID.
func perRunNameMatches(kind, objectName, podName, runID string) bool {
	n := k8sAgentObjectNames(podName, runID)
	switch kind {
	case "Secret":
		return objectName == n.Secret || objectName == n.Auth
	case "SecretProviderClass":
		return objectName == n.SPC
	}
	return false
}

// podBelongsToAgent reports whether podName is the pod name of the agent
// with slug agentSlug: the slug itself, or "<project>--<slug>" (see
// containerName in pkg/agent/run.go).
func podBelongsToAgent(podName, agentSlug string) bool {
	return podName == agentSlug || strings.HasSuffix(podName, "--"+agentSlug)
}
