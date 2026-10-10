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

// Runtime broker error codes the hub matches on to relay a specific broker
// refusal instead of folding it into a generic dispatch failure. They live
// here so the broker (which writes them) and the hub (which reads them)
// share one definition without the hub importing pkg/runtimebroker.
const (
	// BrokerErrCodeSkillResolution marks a create whose required skills
	// could not be resolved.
	BrokerErrCodeSkillResolution = "skill_resolution_failed"

	// BrokerErrCodeWorkspaceStorageUnconfigured marks a create whose
	// workspace was uploaded to bucket storage (workspaceStoragePath set)
	// when neither the request nor the broker names the bucket to download
	// it from (ptone/scion#3422).
	BrokerErrCodeWorkspaceStorageUnconfigured = "workspace_storage_unconfigured"

	// BrokerErrCodeHarnessConfigUnusable marks a dispatch whose
	// harness-config provisioner cannot run (422). It is a configuration
	// error the caller must fix (ptone/scion#3132).
	BrokerErrCodeHarnessConfigUnusable = "harness_config_unusable"

	// BrokerErrCodeIdentityNotMapped marks a dispatch in GCP identity mode
	// "assign" on the Kubernetes runtime whose GCP service account has no
	// kubernetes_service_account_mappings entry on the selected profile
	// (400). The error details name the account, profile, runtime entry
	// and broker so the hub can say who must add the mapping
	// (ptone/scion#4024).
	BrokerErrCodeIdentityNotMapped = "identity_not_mapped"

	// BrokerErrCodeIdentityKSAMismatch marks the same kind of dispatch
	// whose explicitly requested Kubernetes ServiceAccount differs from the
	// one mapped to its GCP service account (400). The details also name
	// the requested and mapped Kubernetes ServiceAccounts.
	BrokerErrCodeIdentityKSAMismatch = "identity_ksa_mismatch"
)

// Detail keys of a broker identity_not_mapped or identity_ksa_mismatch
// error. The broker writes them and the hub reads them to build its own
// message.
const (
	BrokerErrDetailServiceAccount = "serviceAccount"
	BrokerErrDetailProfile        = "profile"
	BrokerErrDetailRuntimeEntry   = "runtimeEntry"
	BrokerErrDetailBroker         = "broker"
	BrokerErrDetailRequestedKSA   = "requestedKubernetesServiceAccount"
	BrokerErrDetailMappedKSA      = "mappedKubernetesServiceAccount"

	// BrokerErrDetailKSASource says where the Kubernetes ServiceAccount
	// named by BrokerErrDetailMappedKSA came from: BrokerKSASourceMapped
	// (kubernetes_service_account_mappings) or BrokerKSASourceDiscovered
	// (the iam.gke.io/gcp-service-account annotation). Absent means mapped.
	BrokerErrDetailKSASource = "kubernetesServiceAccountSource"

	// BrokerErrDetailDiscovery is the result of looking for a Kubernetes
	// ServiceAccount by annotation when no mapping names the GCP service
	// account, on an identity_not_mapped refusal: one of the
	// BrokerKSADiscovery values. BrokerErrDetailNamespace names the
	// namespace that was searched.
	BrokerErrDetailDiscovery = "kubernetesServiceAccountDiscovery"
	BrokerErrDetailNamespace = "namespace"
)

// Values of BrokerErrDetailKSASource.
const (
	BrokerKSASourceMapped     = "mapped"
	BrokerKSASourceDiscovered = "discovered"
)

// Values of BrokerErrDetailDiscovery.
const (
	// BrokerKSADiscoveryNoMatch: no ServiceAccount in the namespace carries
	// the annotation for the GCP service account.
	BrokerKSADiscoveryNoMatch = "no_match"
	// BrokerKSADiscoveryListFailed: the broker could not list the
	// ServiceAccounts in the namespace (for example, forbidden).
	BrokerKSADiscoveryListFailed = "list_failed"
	// BrokerKSADiscoveryUnavailable: the lookup could not run, for example
	// because the selected runtime has no Kubernetes client.
	BrokerKSADiscoveryUnavailable = "unavailable"
	// BrokerKSADiscoveryPending: no discovery result exists yet (the first
	// lookup for the namespace has not finished). Used only as a heartbeat
	// report's incomplete reason (hubclient.ProfileSAMappingsState).
	BrokerKSADiscoveryPending = "pending"
)
