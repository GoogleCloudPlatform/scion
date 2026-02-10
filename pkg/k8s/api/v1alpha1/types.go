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

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// GroupVersion is group version used to register these objects
var GroupVersion = metav1.GroupVersion{Group: "agents.x-k8s.io", Version: "v1alpha1"}
var ExtensionsGroupVersion = metav1.GroupVersion{Group: "extensions.agents.x-k8s.io", Version: "v1alpha1"}

// ConditionType is a type of condition for a resource.
type ConditionType string

func (c ConditionType) String() string { return string(c) }

const (
	// SandboxConditionReady indicates readiness for Sandbox
	SandboxConditionReady ConditionType = "Ready"
)

type PodMetadata struct {
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

type EmbeddedObjectMetadata struct {
	Name        string            `json:"name,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

type PodTemplate struct {
	Spec       corev1.PodSpec `json:"spec"`
	ObjectMeta PodMetadata    `json:"metadata,omitempty"`
}

type PersistentVolumeClaimTemplate struct {
	EmbeddedObjectMetadata `json:"metadata,omitempty"`
	Spec                   corev1.PersistentVolumeClaimSpec `json:"spec"`
}

// ShutdownPolicy describes the policy for deleting the Sandbox when it expires.
type ShutdownPolicy string

const (
	ShutdownPolicyDelete ShutdownPolicy = "Delete"
	ShutdownPolicyRetain ShutdownPolicy = "Retain"
)

type Lifecycle struct {
	ShutdownTime   *metav1.Time    `json:"shutdownTime,omitempty"`
	ShutdownPolicy *ShutdownPolicy `json:"shutdownPolicy,omitempty"`
}

// SandboxSpec defines the desired state of Sandbox
type SandboxSpec struct {
	PodTemplate          PodTemplate                     `json:"podTemplate"`
	VolumeClaimTemplates []PersistentVolumeClaimTemplate `json:"volumeClaimTemplates,omitempty"`
	Lifecycle            `json:",inline"`
	Replicas             *int32 `json:"replicas,omitempty"`
}

type SandboxStatus struct {
	ServiceFQDN   string             `json:"serviceFQDN,omitempty"`
	Service       string             `json:"service,omitempty"`
	Conditions    []metav1.Condition `json:"conditions,omitempty"`
	Replicas      int32              `json:"replicas"`
	LabelSelector string             `json:"selector,omitempty"`
}

// Sandbox is the Schema for the sandboxes API
type Sandbox struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SandboxSpec   `json:"spec"`
	Status SandboxStatus `json:"status,omitempty"`
}

type SandboxList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Sandbox `json:"items"`
}

// --- Extensions Types (Claims & Templates) ---

const (
	SandboxIDLabel = "agents.x-k8s.io/claim-uid"
)

type NetworkPolicySpec struct {
	Ingress []networkingv1.NetworkPolicyIngressRule `json:"ingress,omitempty"`
	Egress  []networkingv1.NetworkPolicyEgressRule  `json:"egress,omitempty"`
}

type SandboxTemplateSpec struct {
	PodTemplate   PodTemplate        `json:"podTemplate"`
	NetworkPolicy *NetworkPolicySpec `json:"networkPolicy,omitempty"`
}

type SandboxTemplateStatus struct {
}

type SandboxTemplate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SandboxTemplateSpec   `json:"spec"`
	Status SandboxTemplateStatus `json:"status,omitempty"`
}

type SandboxTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SandboxTemplate `json:"items"`
}

type SandboxTemplateRef struct {
	Name string `json:"name,omitempty"`
}

type SandboxClaimSpec struct {
	TemplateRef SandboxTemplateRef `json:"sandboxTemplateRef,omitempty"`
}

type SandboxClaimStatus struct {
	Conditions    []metav1.Condition `json:"conditions,omitempty"`
	SandboxStatus SandboxStatusRef   `json:"sandboxStatus,omitempty"`
}

type SandboxStatusRef struct {
	Name string `json:"Name,omitempty"`
}

type SandboxClaim struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SandboxClaimSpec   `json:"spec"`
	Status SandboxClaimStatus `json:"status,omitempty"`
}

type SandboxClaimList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SandboxClaim `json:"items"`
}

func (in *SandboxClaimList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *SandboxClaimList) DeepCopy() *SandboxClaimList {
	if in == nil {
		return nil
	}
	out := new(SandboxClaimList)
	in.DeepCopyInto(out)
	return out
}

func (in *SandboxClaimList) DeepCopyInto(out *SandboxClaimList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		in, out := &in.Items, &out.Items
		*out = make([]SandboxClaim, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
}

func (in *SandboxClaim) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *SandboxClaim) DeepCopy() *SandboxClaim {
	if in == nil {
		return nil
	}
	out := new(SandboxClaim)
	in.DeepCopyInto(out)
	return out
}

func (in *SandboxClaim) DeepCopyInto(out *SandboxClaim) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = in.Spec
	in.Status.DeepCopyInto(&out.Status)
}

func (in *SandboxClaimStatus) DeepCopyInto(out *SandboxClaimStatus) {
	*out = *in
	if in.Conditions != nil {
		in, out := &in.Conditions, &out.Conditions
		*out = make([]metav1.Condition, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
	out.SandboxStatus = in.SandboxStatus
}

func (in *Sandbox) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *Sandbox) DeepCopy() *Sandbox {
	if in == nil {
		return nil
	}
	out := new(Sandbox)
	in.DeepCopyInto(out)
	return out
}

func (in *Sandbox) DeepCopyInto(out *Sandbox) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = in.Spec // PodTemplate and Lifecycle are simple enough or handled
	in.Status.DeepCopyInto(&out.Status)
}

func (in *SandboxStatus) DeepCopyInto(out *SandboxStatus) {
	*out = *in
	if in.Conditions != nil {
		in, out := &in.Conditions, &out.Conditions
		*out = make([]metav1.Condition, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
}

func (in *SandboxList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *SandboxList) DeepCopy() *SandboxList {
	if in == nil {
		return nil
	}
	out := new(SandboxList)
	in.DeepCopyInto(out)
	return out
}

func (in *SandboxList) DeepCopyInto(out *SandboxList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		in, out := &in.Items, &out.Items
		*out = make([]Sandbox, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
}

func (in *SandboxTemplate) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *SandboxTemplate) DeepCopy() *SandboxTemplate {
	if in == nil {
		return nil
	}
	out := new(SandboxTemplate)
	in.DeepCopyInto(out)
	return out
}

func (in *SandboxTemplate) DeepCopyInto(out *SandboxTemplate) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = in.Spec
}

func (in *SandboxTemplateList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *SandboxTemplateList) DeepCopy() *SandboxTemplateList {
	if in == nil {
		return nil
	}
	out := new(SandboxTemplateList)
	in.DeepCopyInto(out)
	return out
}

func (in *SandboxTemplateList) DeepCopyInto(out *SandboxTemplateList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	if in.Items != nil {
		in, out := &in.Items, &out.Items
		*out = make([]SandboxTemplate, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
}
