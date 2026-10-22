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

// Runtime broker error codes the hub or CLI matches on, to relay or show a
// specific broker refusal instead of folding it into a generic dispatch
// failure. They live here so the broker (which writes them) and the hub
// and CLI (which read them) share one definition without importing
// pkg/runtimebroker.
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

	// BrokerErrCodePriorStartInProgress marks a create refused because an
	// earlier start of the same agent name on the broker was still
	// finishing its failure cleanup when the broker's supersede wait ended
	// (503, with Retry-After). Retrying the same create shortly is expected
	// to succeed (ptone/scion#2632).
	BrokerErrCodePriorStartInProgress = "prior_start_in_progress"

	// BrokerErrCodeStartCancelled marks a create whose start was cancelled
	// on the broker before it took the agent name: a stop or delete of the
	// agent, or a newer start of the same name, reached the broker first
	// (409). Retry only if the create is still wanted (ptone/scion#2632).
	BrokerErrCodeStartCancelled = "start_cancelled"

	// BrokerErrCodeBrokerDraining marks a create, start or restart the
	// broker refused because it is shutting down (503, with Retry-After).
	// The handlers refuse at entry, before any work. An async create can
	// also be refused later, in admission, after its start context (and
	// so possibly its worktree) was prepared but before the launch was
	// accepted, like the other pre-acceptance refusals (start_cancelled,
	// prior_start_in_progress). Either way no agent was started; retrying
	// shortly, against the restarted broker, is expected to succeed
	// (ptone/scion#4668).
	BrokerErrCodeBrokerDraining = "broker_draining"

	// BrokerErrCodeLocalWorktreeRefused marks a worktree-per-agent dispatch
	// for a linked checkout that the co-located broker refused because the
	// checkout failed verification (409). Nothing was created. The hub
	// relays it like any other broker failure; the CLI reads
	// BrokerErrDetailReason from its details to show the cause and the fix.
	BrokerErrCodeLocalWorktreeRefused = "local_worktree_refused"

	// BrokerErrCodeRetainedFilesRefused marks a PurgeRetainedQueryParam
	// delete that removed nothing because the files are not, or may not
	// be, the retained files of a soft-deleted agent (409): a runtime entry
	// still holds the agent, a start of it is in flight, its project
	// directory is unknown, its agent-info.json carries no soft-delete mark,
	// or one of its directories is a symlink (ptone/scion#4744). The cause
	// is in BrokerErrDetailReason. A retry gets the same answer while that
	// state lasts.
	BrokerErrCodeRetainedFilesRefused = "retained_files_refused"

	// BrokerErrCodeRetainedPurgeInProgress marks a start, restart or create
	// the broker refused because a removal of the agent's retained files
	// (PurgeRetainedQueryParam) is running there (409, with Retry-After).
	// Nothing was started or written. Retry after the removal ends.
	BrokerErrCodeRetainedPurgeInProgress = "retained_purge_in_progress"

	// BrokerErrCodeMoveTargetNotFresh marks a forced move's provision
	// (moved-agent expectation "fresh-agent-dir") the broker refused before
	// creating anything, because it has files for the agent or cannot
	// confirm that it has none (409). The message names no host path. The
	// hub then rolls the move back without removing anything on that
	// broker: the files there were not created by the move
	// (ptone/scion#5441).
	BrokerErrCodeMoveTargetNotFresh = "move_target_not_fresh"

	// BrokerErrCodeMoveMarkerMismatch marks a forced move's rollback delete
	// (MoveReincarnationIDQueryParam) that removed nothing because an agent
	// directory on the broker exists without that move's marker: the files
	// were not created by the move (409). The message names no host path.
	BrokerErrCodeMoveMarkerMismatch = "move_marker_mismatch"

	// BrokerErrCodeReloadUnconfirmed marks a reset-auth that wrote the new
	// credential into the agent container but could not deliver the reload
	// signal to the agent's init process (502), so the running agent did not
	// confirm the reload. The credential file is in place; the hub relays the
	// code to the caller.
	BrokerErrCodeReloadUnconfirmed = "reload_unconfirmed"
)

// ReloadUnconfirmedMessage is the caller-facing text for
// BrokerErrCodeReloadUnconfirmed, shared by the broker, the hub and the CLI.
// It must stay true whether or not the agent also reloads the credential on
// its own (for example from a credential file watch, where present).
const ReloadUnconfirmedMessage = "credential issued and written; the running agent did not confirm the reload. " +
	"If it keeps failing to authenticate, restart the agent"

// BrokerErrDetailReason is the details key of a
// BrokerErrCodeLocalWorktreeRefused error (the client-facing message, with
// any remote shown without credentials) and of a
// BrokerErrCodeRetainedFilesRefused or BrokerErrCodeRetainedPurgeInProgress
// error (the cause, naming no host path).
const BrokerErrDetailReason = "reason"

// Values of details.cause on a broker runtime_error answer to a create
// whose runtime start failed with a Kubernetes API status the operator can
// act on (ptone/scion#4555). Each is a fixed code: the broker never puts the
// API server message or any namespace, ServiceAccount or object name in the
// body. The hub relays details.cause only when it is one of these.
const (
	// BrokerRuntimeCauseForbidden: the runtime's credentials may not
	// perform a call the start needs (for example, the namespace
	// ServiceAccount may not get or list pods).
	BrokerRuntimeCauseForbidden = "runtime_forbidden"
	// BrokerRuntimeCauseUnauthorized: the runtime's own credentials were
	// refused (expired or invalid).
	BrokerRuntimeCauseUnauthorized = "runtime_unauthorized"
)

// IsBrokerRuntimeCause reports whether code is one of the
// BrokerRuntimeCause values.
func IsBrokerRuntimeCause(code string) bool {
	switch code {
	case BrokerRuntimeCauseForbidden, BrokerRuntimeCauseUnauthorized:
		return true
	}
	return false
}

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
	// BrokerSAReportForceRuntime: the broker runs with ForceRuntime, so
	// dispatch ignores the profile and its report may not apply. Used only
	// as a heartbeat report's incomplete reason.
	BrokerSAReportForceRuntime = "force_runtime"
)

// BrokerSAReportVersion is the version of a broker's per-profile service
// account report (hubclient.ProfileSAMappingsState.ReportVersion). Version
// 2 means the broker reports a profile incomplete whenever dispatch would
// not use the profile's entries (BrokerSAReportForceRuntime). The Hub
// refuses a dispatch from a report only at this version or later; a report
// without it (an older broker) is unknown.
const BrokerSAReportVersion = 2
