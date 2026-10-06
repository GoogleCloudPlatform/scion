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
	"fmt"
	"regexp"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/k8s"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// This file implements the delete half of an async launch's resource
// tracking (design t1-async-create-v11.md §3.8.4): each runtime reports a
// ResourceHandle after every true create (see launchHooks), and an aborted
// launch deletes exactly those handles here. Every delete is conditional on
// the identity recorded at create time, so a stale launch's cleanup can
// never remove a same-named resource a newer launch has since recreated.
// These methods satisfy agent.UIDPreconditionDeleter.

// containerIDPattern matches a Docker/Podman container ID: lowercase hex,
// from the 12-character short form to the full 64 characters.
var containerIDPattern = regexp.MustCompile(`^[0-9a-f]{12,64}$`)

// reportContainerCreated reports a Docker or Podman create (design §3.8.4:
// "a Docker container ID"). The container ID is the handle's UID, the
// identity DeleteResource removes by. out is the run command's output,
// which interleaves stdout and stderr, so a warning line may surround the
// ID: the last line that is a well-formed ID is used. If no line is, no
// handle is recorded (the launch's cleanup cannot remove this container by
// ID) and a warning is logged. Run itself still returns the whole trimmed
// output; only the handle is restricted.
func reportContainerCreated(hooks launchHooks, name, out string) {
	if !hooks.active() {
		return
	}
	lines := strings.Split(out, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if id := strings.TrimSpace(lines[i]); containerIDPattern.MatchString(id) {
			hooks.created(api.ResourceHandle{Kind: api.ResourceKindContainer, Name: name, UID: id})
			return
		}
	}
	runtimeLog.Warn("Container run output has no well-formed container ID; launch cleanup will not track this container",
		"name", name, "output_lines", len(lines))
}

// reportAppleContainerCreated reports an Apple container create. Apple's
// container CLI prints the container name as its ID, so the last non-empty
// output line is used without a hex check.
func reportAppleContainerCreated(hooks launchHooks, name, out string) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	id := strings.TrimSpace(lines[len(lines)-1])
	if id == "" {
		return
	}
	hooks.created(api.ResourceHandle{Kind: api.ResourceKindContainer, Name: name, UID: id})
}

// errNoUID is returned for a handle without a recorded UID: deleting it
// unconditionally by name is exactly what the precondition exists to
// prevent, so it is refused instead.
func errNoUID(h api.ResourceHandle) error {
	return fmt.Errorf("resource %s %s/%s has no recorded UID; refusing an unconditional delete", h.Kind, h.Namespace, h.Name)
}

// DeleteResource deletes one Kubernetes object an async launch created, with
// a UID precondition. An object that is already gone, or whose UID no longer
// matches (a newer launch recreated the name), is left alone and is not an
// error: there is nothing of this launch's left to delete.
func (r *KubernetesRuntime) DeleteResource(ctx context.Context, h api.ResourceHandle) error {
	if h.UID == "" {
		return errNoUID(h)
	}
	opts := metav1.DeleteOptions{Preconditions: k8sUIDPrecondition(types.UID(h.UID))}

	var err error
	switch h.Kind {
	case api.ResourceKindSecret:
		err = r.Client.Clientset.CoreV1().Secrets(h.Namespace).Delete(ctx, h.Name, opts)
	case api.ResourceKindSecretProviderClass:
		err = r.Client.Dynamic().Resource(k8s.SecretProviderClassGVR).Namespace(h.Namespace).Delete(ctx, h.Name, opts)
	case api.ResourceKindPod:
		// Immediate termination, as Delete does for force-removal: the pod
		// belongs to a launch that has already ended.
		gracePeriod := int64(0)
		opts.GracePeriodSeconds = &gracePeriod
		err = r.Client.Clientset.CoreV1().Pods(h.Namespace).Delete(ctx, h.Name, opts)
	default:
		return fmt.Errorf("unsupported resource kind %q for the kubernetes runtime", h.Kind)
	}
	switch {
	case err == nil, k8serrors.IsNotFound(err):
		return nil
	case k8serrors.IsConflict(err):
		// The UID precondition failed: the name now belongs to another
		// object, which this launch must not delete.
		runtimeLog.Info("Launch cleanup skipped a resource recreated by another launch",
			"kind", h.Kind, "namespace", h.Namespace, "name", h.Name, "uid", h.UID)
		return nil
	default:
		return fmt.Errorf("delete %s %s/%s: %w", h.Kind, h.Namespace, h.Name, err)
	}
}

// deleteContainerByID removes a container by its ID (never by name, so a
// newer container with the same name is untouched). A container that is
// already gone is not an error.
func deleteContainerByID(ctx context.Context, command string, h api.ResourceHandle) error {
	if h.Kind != api.ResourceKindContainer {
		return fmt.Errorf("unsupported resource kind %q for a container runtime", h.Kind)
	}
	if h.UID == "" {
		return errNoUID(h)
	}
	out, err := runSimpleCommand(ctx, command, "rm", "-f", h.UID)
	if err != nil {
		if isNoSuchContainer(out) {
			return nil
		}
		return fmt.Errorf("remove container %s (%s): %w (output: %s)", h.Name, h.UID, err, strings.TrimSpace(out))
	}
	return nil
}

// isNoSuchContainer reports whether a docker/podman rm output says the
// container does not exist.
func isNoSuchContainer(out string) bool {
	lower := strings.ToLower(out)
	return strings.Contains(lower, "no such container") || strings.Contains(lower, "no container with name or id")
}

// DeleteResource removes a container an async launch created, by ID.
func (r *DockerRuntime) DeleteResource(ctx context.Context, h api.ResourceHandle) error {
	return deleteContainerByID(ctx, r.Command, h)
}

// DeleteResource removes a container an async launch created, by ID.
func (r *PodmanRuntime) DeleteResource(ctx context.Context, h api.ResourceHandle) error {
	return deleteContainerByID(ctx, r.Command, h)
}

// DeleteResource removes a container an async launch created, by the ID
// `container run -d` returned. Apple's container CLI uses the container
// name as its ID, so unlike Docker and Podman this cannot tell a recreated
// container of the same name apart.
func (r *AppleContainerRuntime) DeleteResource(ctx context.Context, h api.ResourceHandle) error {
	if h.Kind != api.ResourceKindContainer {
		return fmt.Errorf("unsupported resource kind %q for a container runtime", h.Kind)
	}
	if h.UID == "" {
		return errNoUID(h)
	}
	return r.Delete(ctx, RunRef{ID: h.UID})
}
