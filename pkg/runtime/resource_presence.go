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
	"strings"

	"cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	k8svalidation "k8s.io/apimachinery/pkg/util/validation"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/k8s"
)

// Exact presence of launch-created objects, for a flat Runtime Broker
// instance's ownership records (ptone/scion#3274): a successful delete
// request is not confirmed absence.

// ResourceAbsenceChecker establishes whether the object a handle names is
// gone, by its immutable identity. ResourceAbsent returns true only for an
// exact not-found or for a different object now holding the name; an
// object that still exists (a terminating one included) is present; any
// other failure is returned as an error and never read as absence.
type ResourceAbsenceChecker interface {
	ResourceAbsent(ctx context.Context, h api.ResourceHandle) (bool, error)
}

// OwnedResource is a launch child object found by its owner label, with
// its labels (for reconstruction).
type OwnedResource struct {
	Handle api.ResourceHandle
	Labels map[string]string
}

// OwnedResourceLister lists the per-launch child objects carrying a flat
// instance's owner label (api.LabelRuntimeBrokerID = runtimeBrokerID), so
// they can be recovered without a surviving main object. A list failure is
// returned and never read as absence; an empty or invalid ID is an error.
type OwnedResourceLister interface {
	ListOwnedResources(ctx context.Context, runtimeBrokerID string) ([]OwnedResource, error)
}

// ListedObjectHandler is implemented by a runtime whose launch records its
// main object under a handle that a listed api.AgentInfo does not spell out
// on its own (the generic handle is the object's name and container or pod
// ID). ListedObjectHandle returns the handle the launch recorded for the
// listed object o, built by the same runtime function, so an ownership
// record reconstructed from a listing equals the launched one.
type ListedObjectHandler interface {
	ListedObjectHandle(o api.AgentInfo) api.ResourceHandle
}

var (
	_ ResourceAbsenceChecker = (*KubernetesRuntime)(nil)
	_ ResourceAbsenceChecker = (*DockerRuntime)(nil)
	_ ResourceAbsenceChecker = (*PodmanRuntime)(nil)
	_ ResourceAbsenceChecker = (*CloudRunRuntime)(nil)
	_ OwnedResourceLister    = (*KubernetesRuntime)(nil)
	_ ListedObjectHandler    = (*CloudRunRuntime)(nil)
)

// exactNotFound reports whether err is a not-found for the named object
// itself. A missing resource type (for example an absent CRD) is also a
// 404, but without the object's name in its details: that is an error, not
// absence.
func exactNotFound(err error, name string) bool {
	var status k8serrors.APIStatus
	if !k8serrors.IsNotFound(err) || !errors.As(err, &status) {
		return false
	}
	d := status.Status().Details
	return d != nil && d.Name == name
}

// ResourceAbsent checks a Secret, SecretProviderClass or pod by namespace,
// name and UID.
func (r *KubernetesRuntime) ResourceAbsent(ctx context.Context, h api.ResourceHandle) (bool, error) {
	if h.UID == "" || h.Namespace == "" || h.Name == "" {
		return false, fmt.Errorf("presence check of %s %s/%s: namespace, name and UID are required", h.Kind, h.Namespace, h.Name)
	}
	var uid types.UID
	var err error
	switch h.Kind {
	case api.ResourceKindSecret:
		var s interface{ GetUID() types.UID }
		s, err = r.Client.Clientset.CoreV1().Secrets(h.Namespace).Get(ctx, h.Name, metav1.GetOptions{})
		if err == nil {
			uid = s.GetUID()
		}
	case api.ResourceKindPod:
		var p interface{ GetUID() types.UID }
		p, err = r.Client.Clientset.CoreV1().Pods(h.Namespace).Get(ctx, h.Name, metav1.GetOptions{})
		if err == nil {
			uid = p.GetUID()
		}
	case api.ResourceKindSecretProviderClass:
		var o interface{ GetUID() types.UID }
		o, err = r.Client.Dynamic().Resource(k8s.SecretProviderClassGVR).Namespace(h.Namespace).Get(ctx, h.Name, metav1.GetOptions{})
		if err == nil {
			uid = o.GetUID()
		}
	default:
		return false, fmt.Errorf("presence check: unsupported resource kind %q for the kubernetes runtime", h.Kind)
	}
	switch {
	case err == nil:
		// The same object is still there (terminating or not); a different
		// UID means the recorded object is gone and the name was reused.
		return uid != types.UID(h.UID), nil
	case exactNotFound(err, h.Name):
		return true, nil
	default:
		return false, fmt.Errorf("presence check of %s %s/%s: %w", h.Kind, h.Namespace, h.Name, err)
	}
}

// ownedListPageSize bounds each page of ListOwnedResources.
const ownedListPageSize = 500

// ListOwnedResources lists the Secrets (and, in GKE mode, the
// SecretProviderClasses) carrying the owner label, in the namespace scope
// the leftover cleanup uses, page by page.
func (r *KubernetesRuntime) ListOwnedResources(ctx context.Context, runtimeBrokerID string) ([]OwnedResource, error) {
	if runtimeBrokerID == "" {
		return nil, errors.New("owned resource listing: a Runtime Broker ID is required")
	}
	if errs := k8svalidation.IsValidLabelValue(runtimeBrokerID); len(errs) > 0 {
		return nil, fmt.Errorf("owned resource listing: invalid Runtime Broker ID %q: %s", runtimeBrokerID, strings.Join(errs, "; "))
	}
	selector, err := labels.ValidatedSelectorFromSet(map[string]string{api.LabelRuntimeBrokerID: runtimeBrokerID})
	if err != nil {
		return nil, fmt.Errorf("invalid owner selector: %w", err)
	}
	namespace := r.DefaultNamespace
	if r.ListAllNamespaces {
		namespace = ""
	}
	var out []OwnedResource
	opts := metav1.ListOptions{LabelSelector: selector.String(), Limit: ownedListPageSize}
	for {
		page, err := r.Client.Clientset.CoreV1().Secrets(namespace).List(ctx, opts)
		if err != nil {
			return nil, fmt.Errorf("list owned Secrets: %w", err)
		}
		for _, s := range page.Items {
			out = append(out, OwnedResource{
				Handle: api.ResourceHandle{Kind: api.ResourceKindSecret, Namespace: s.Namespace, Name: s.Name, UID: string(s.UID)},
				Labels: s.Labels,
			})
		}
		if page.Continue == "" {
			break
		}
		opts.Continue = page.Continue
	}
	if r.GKEMode {
		opts = metav1.ListOptions{LabelSelector: selector.String(), Limit: ownedListPageSize}
		for {
			page, err := r.Client.Dynamic().Resource(k8s.SecretProviderClassGVR).Namespace(namespace).List(ctx, opts)
			if err != nil {
				return nil, fmt.Errorf("list owned SecretProviderClasses: %w", err)
			}
			for _, o := range page.Items {
				out = append(out, OwnedResource{
					Handle: api.ResourceHandle{Kind: api.ResourceKindSecretProviderClass, Namespace: o.GetNamespace(), Name: o.GetName(), UID: string(o.GetUID())},
					Labels: o.GetLabels(),
				})
			}
			if page.GetContinue() == "" {
				break
			}
			opts.Continue = page.GetContinue()
		}
	}
	return out, nil
}

// isNoSuchContainerOrObject reports whether a docker or podman inspect/rm
// output says the container does not exist (both CLIs' wordings).
func isNoSuchContainerOrObject(out string) bool {
	lower := strings.ToLower(out)
	for _, s := range []string{"no such container", "no such object", "no container with name or id"} {
		if strings.Contains(lower, s) {
			return true
		}
	}
	return false
}

// containerAbsent checks a container by its ID (never by name).
func containerAbsent(ctx context.Context, command string, h api.ResourceHandle) (bool, error) {
	if h.Kind != api.ResourceKindContainer {
		return false, fmt.Errorf("presence check: unsupported resource kind %q for a container runtime", h.Kind)
	}
	if h.UID == "" {
		return false, errNoUID(h)
	}
	out, err := runSimpleCommand(ctx, command, "inspect", "--type", "container", "--format", "{{.Id}}", h.UID)
	if err == nil {
		return false, nil
	}
	if isNoSuchContainerOrObject(out) || isNoSuchContainerOrObject(err.Error()) {
		return true, nil
	}
	return false, fmt.Errorf("presence check of container %s (%s): %w (output: %s)", h.Name, h.UID, err, strings.TrimSpace(out))
}

// ResourceAbsent checks a container by its ID.
func (r *DockerRuntime) ResourceAbsent(ctx context.Context, h api.ResourceHandle) (bool, error) {
	return containerAbsent(ctx, r.Command, h)
}

// ResourceAbsent checks a container by its ID.
func (r *PodmanRuntime) ResourceAbsent(ctx context.Context, h api.ResourceHandle) (bool, error) {
	return containerAbsent(ctx, r.Command, h)
}

// ResourceAbsent checks the Cloud Run instance a launch recorded
// (api.ResourceKindCloudRunInstance: Namespace is the runtime's parent,
// Name the instance ID, UID the run ID). The run ID label decides, never the
// name alone:
//   - an exact NotFound is absent;
//   - an instance whose stored run ID label differs from h.UID (or is
//     missing) is absent: the name is held by a different object;
//   - an instance with the same run ID is present, a soft-deleted one
//     (delete_time set) included, until it is NotFound;
//   - any other failure is an error, never absence.
//
// A handle of another kind, without a UID, name or parent, or in another
// parent than this runtime's, is refused before any call.
func (r *CloudRunRuntime) ResourceAbsent(ctx context.Context, h api.ResourceHandle) (bool, error) {
	if h.Kind != api.ResourceKindCloudRunInstance {
		return false, fmt.Errorf("presence check: unsupported resource kind %q for the cloudrun runtime", h.Kind)
	}
	if h.UID == "" || h.Namespace == "" || h.Name == "" {
		return false, fmt.Errorf("presence check of %s %s/%s: parent, name and run ID are required", h.Kind, h.Namespace, h.Name)
	}
	if err := r.resolveConfig(ctx); err != nil {
		return false, fmt.Errorf("presence check of instance %s: failed to resolve Cloud Run config: %w", h.Name, err)
	}
	if parent := r.parent(); h.Namespace != parent {
		return false, fmt.Errorf("presence check of instance %s: it is in %q, not this runtime's %q", h.Name, h.Namespace, parent)
	}
	c, err := r.client(ctx)
	if err != nil {
		return false, fmt.Errorf("presence check of instance %s: failed to create client: %w", h.Name, err)
	}
	defer func() { _ = c.Close() }()
	inst, err := c.GetInstance(ctx, &runpb.GetInstanceRequest{Name: h.Namespace + "/instances/" + h.Name}, defaultCallOpts...)
	switch {
	case err == nil && inst == nil:
		return false, fmt.Errorf("presence check of instance %s: GetInstance returned no instance", h.Name)
	case err == nil:
		// The stored label is the sanitized run ID, so the handle's UID is
		// compared in the same form, as Run's own run check does. Two run IDs
		// that differ only by sanitization would read as present, which keeps
		// the record (the safe side); minted run IDs are lower-case UUIDs, for
		// which this is exact.
		return inst.GetLabels()[r.storedLabelKey(api.LabelRunID)] != sanitizeGCPLabelValue(h.UID), nil
	case status.Code(err) == codes.NotFound:
		return true, nil
	default:
		return false, fmt.Errorf("presence check of instance %s: %w", h.Name, err)
	}
}

// ListedObjectHandle is the handle Run recorded for the listed instance o:
// kind cloudrun_instance in this runtime's parent, named by its instance ID
// and identified by its stored run ID label.
func (r *CloudRunRuntime) ListedObjectHandle(o api.AgentInfo) api.ResourceHandle {
	return api.ResourceHandle{Kind: api.ResourceKindCloudRunInstance, Namespace: r.parent(),
		Name: cloudRunShortInstanceID(o.ContainerID), UID: o.Labels[r.storedLabelKey(api.LabelRunID)]}
}
