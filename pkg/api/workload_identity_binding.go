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

package api

// WorkloadIdentityBinding is one entry of a GCP service account mint
// response's workloadIdentityBindings (ptone/scion#3329 phase 5): the
// roles/iam.workloadIdentityUser grant the hub made, or skipped, on the
// minted account for one Kubernetes runtime target (pool and namespace)
// the account can be dispatched to.
type WorkloadIdentityBinding struct {
	// Pool and Namespace are the target's Workload Identity pool and agent
	// namespace. Both are empty for a no_workload_identity_pool entry.
	Pool      string `json:"pool,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	// KSA is the conventional Kubernetes ServiceAccount: the minted
	// account's ID, the part of its email before "@".
	KSA string `json:"ksa,omitempty"`
	// Member is the IAM member granted, serviceAccount:<pool>[<ns>/<ksa>].
	Member string `json:"member,omitempty"`
	// Profiles name the runtime targets this entry covers: "broker/profile"
	// for a legacy Runtime Broker profile, the broker name for a flat
	// Runtime Broker instance.
	Profiles []string `json:"profiles,omitempty"`
	// State is WorkloadIdentityBindingBound, ...Skipped or ...Failed.
	State string `json:"state"`
	// Reason is the code of a state other than bound
	// (WorkloadIdentityReason*).
	Reason string `json:"reason,omitempty"`
	// Detail explains Reason in a sentence.
	Detail string `json:"detail,omitempty"`
	// ManualStep is the gcloud command that makes the grant by hand, for a
	// state other than bound. For a no_workload_identity_pool entry it has
	// placeholders for the pool and the namespace.
	ManualStep string `json:"manualStep,omitempty"`
}

// WorkloadIdentityBinding states.
const (
	WorkloadIdentityBindingBound   = "bound"
	WorkloadIdentityBindingSkipped = "skipped"
	WorkloadIdentityBindingFailed  = "failed"
)

// WorkloadIdentityBinding reasons.
const (
	// WorkloadIdentityReasonPermissionDenied: the hub may not change the
	// account's IAM policy.
	WorkloadIdentityReasonPermissionDenied = "permission_denied"
	// WorkloadIdentityReasonPoolNotFound: IAM does not know the pool
	// (Workload Identity is not enabled in that project, or the pool is
	// misspelt).
	WorkloadIdentityReasonPoolNotFound = "pool_not_found"
	// WorkloadIdentityReasonNoPool: a Kubernetes runtime target reports no
	// pool (none configured, or a broker too old to report one).
	WorkloadIdentityReasonNoPool = "no_workload_identity_pool"
	// WorkloadIdentityReasonIAMError: any other IAM failure (state failed).
	WorkloadIdentityReasonIAMError = "iam_error"
)
