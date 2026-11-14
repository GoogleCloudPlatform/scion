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

package hub

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"google.golang.org/api/googleapi"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Workload Identity binding at mint (ptone/scion#3329 phase 5, design
// §3.4-§3.5). When the hub mints a GCP service account (GSA) and the
// hub.k8s_workload_identity_setup experiment is on, it grants
// roles/iam.workloadIdentityUser on the new GSA to the conventional
// Kubernetes ServiceAccount (KSA) of every Kubernetes runtime target the
// GSA can be dispatched to:
//
//	serviceAccount:<pool>[<namespace>/<account ID>]
//
// The KSA name is the GSA's account ID (the part of its email before "@"),
// a stable, documented contract. The pool and namespace come from the
// Workload Identity targets the brokers report in their heartbeats
// (store.RuntimeBroker.WorkloadIdentityTargets). The grant is best effort:
// it never fails the mint, and every target that is not bound is reported
// with a reason and the gcloud command that makes the grant by hand. The
// hub never calls a project-level IAM API and never calls Kubernetes: the
// only IAM write is on the minted GSA's own policy.

// roleWorkloadIdentityUser is the role a Workload Identity member needs on
// a GSA to act as it.
const roleWorkloadIdentityUser = "roles/iam.workloadIdentityUser"

// GCPTokenEventWIBinding audits one Workload Identity grant made, or
// skipped, at mint.
const GCPTokenEventWIBinding GCPTokenEventType = "gcp_sa.wi_binding"

// workloadIdentityMintTarget is one (pool, namespace) a mint binds, with the
// runtime targets that report it.
type workloadIdentityMintTarget struct {
	pool      string
	namespace string
	profiles  []string
}

// workloadIdentityMissing is one broker whose Kubernetes runtime targets
// report no Workload Identity pool.
type workloadIdentityMissing struct {
	profiles []string
	// tooOld is true when the broker does not report Workload Identity
	// targets at all (no wi_targets_v1 capability).
	tooOld bool
}

// workloadIdentityMintPlan is what a mint binds: the (pool, namespace)
// targets, sorted, and the brokers whose Kubernetes targets report no pool.
type workloadIdentityMintPlan struct {
	targets []workloadIdentityMintTarget
	missing []workloadIdentityMissing
}

// empty reports whether the plan has nothing to report (no Kubernetes
// target at all).
func (p workloadIdentityMintPlan) empty() bool {
	return len(p.targets) == 0 && len(p.missing) == 0
}

// workloadIdentityMintPlanFor computes the plan over brokers (design §3.4).
// It works the same on legacy and flat Runtime Broker rows:
//
//   - every stored Workload Identity target is a binding target; equal
//     (pool, namespace) pairs are merged, keeping every runtime target that
//     reports them;
//   - a legacy broker's Kubernetes profile (isKubernetesBrokerProfile) with
//     no stored target, and a flat Kubernetes instance with none, is
//     missing: no pool configured, or a broker too old to report one.
//
// Non-Kubernetes profiles and instances are never part of the plan.
func workloadIdentityMintPlanFor(brokers []*store.RuntimeBroker) workloadIdentityMintPlan {
	var plan workloadIdentityMintPlan
	byKey := map[string]int{}
	for _, b := range brokers {
		if b == nil {
			continue
		}
		flat := b.IsFlat()
		label := func(profile string) string {
			if flat || profile == "" {
				return b.Name
			}
			return b.Name + "/" + profile
		}
		covered := map[string]bool{}
		for _, t := range b.WorkloadIdentityTargets {
			if t.Pool == "" {
				continue
			}
			key := t.Pool + "\x00" + t.Namespace
			i, ok := byKey[key]
			if !ok {
				i = len(plan.targets)
				byKey[key] = i
				plan.targets = append(plan.targets, workloadIdentityMintTarget{pool: t.Pool, namespace: t.Namespace})
			}
			plan.targets[i].profiles = append(plan.targets[i].profiles, label(t.Profile))
			covered[t.Profile] = true
		}
		var missing []string
		if flat {
			if b.RuntimeTarget != nil && isKubernetesRuntimeType(b.RuntimeTarget.Type) && len(covered) == 0 {
				missing = append(missing, label(""))
			}
		} else {
			for _, p := range b.Profiles {
				if isKubernetesBrokerProfile(p) && !covered[p.Name] {
					missing = append(missing, label(p.Name))
				}
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			plan.missing = append(plan.missing, workloadIdentityMissing{
				profiles: missing,
				tooOld:   b.Capabilities == nil || !b.Capabilities.WITargetsV1,
			})
		}
	}
	for i := range plan.targets {
		sort.Strings(plan.targets[i].profiles)
	}
	sort.Slice(plan.targets, func(i, j int) bool {
		if plan.targets[i].pool != plan.targets[j].pool {
			return plan.targets[i].pool < plan.targets[j].pool
		}
		return plan.targets[i].namespace < plan.targets[j].namespace
	})
	return plan
}

// workloadIdentityMember is the IAM member of the conventional KSA ksa in
// namespace on pool.
func workloadIdentityMember(pool, namespace, ksa string) string {
	return fmt.Sprintf("serviceAccount:%s[%s/%s]", pool, namespace, ksa)
}

// workloadIdentityManualStep is the gcloud command that grants member the
// Workload Identity role on saEmail by hand.
func workloadIdentityManualStep(saEmail, member string) string {
	return fmt.Sprintf("gcloud iam service-accounts add-iam-policy-binding %s --role %s --member %q",
		saEmail, roleWorkloadIdentityUser, member)
}

// gcpAccountID returns the account ID of a service account email: the part
// before "@".
func gcpAccountID(saEmail string) string {
	id, _, _ := strings.Cut(saEmail, "@")
	return id
}

// grantWorkloadIdentityBestEffort grants roles/iam.workloadIdentityUser on
// saEmail to the conventional KSA of every target in plan, in one
// get-modify-set of the account's policy, and returns one entry per target
// and per broker with no pool. It never returns an error: a failed grant is
// an entry that is not bound, with a reason and the manual command.
//
// The batched write is retried on the eventual-consistency error of a
// just-created account (retryIAMGrant) and once more on an etag conflict.
// When it fails for any reason other than a denied permission and more than
// one member was in it, each member is granted on its own, so one bad
// target (for example a pool that does not exist) does not block the
// others.
func (s *Server) grantWorkloadIdentityBestEffort(ctx context.Context, saEmail string, plan workloadIdentityMintPlan) []api.WorkloadIdentityBinding {
	ksa := gcpAccountID(saEmail)
	out := make([]api.WorkloadIdentityBinding, 0, len(plan.targets)+len(plan.missing))
	members := make([]string, 0, len(plan.targets))
	for _, t := range plan.targets {
		member := workloadIdentityMember(t.pool, t.namespace, ksa)
		members = append(members, member)
		out = append(out, api.WorkloadIdentityBinding{
			Pool: t.pool, Namespace: t.namespace, KSA: ksa, Member: member,
			Profiles: t.profiles, State: api.WorkloadIdentityBindingBound,
		})
	}
	if len(members) > 0 {
		err := s.addWorkloadIdentityMembers(ctx, saEmail, members)
		switch {
		case err == nil:
		case len(members) == 1 || workloadIdentityErrorReason(err) == api.WorkloadIdentityReasonPermissionDenied:
			for i := range out {
				setWorkloadIdentityFailure(&out[i], saEmail, err)
			}
		default:
			slog.Warn("GCP SA mint: batched Workload Identity grant failed; granting each member on its own",
				"sa_email", saEmail, "members", len(members), "error", err)
			for i := range out {
				if err := s.addWorkloadIdentityMembers(ctx, saEmail, []string{out[i].Member}); err != nil {
					setWorkloadIdentityFailure(&out[i], saEmail, err)
				}
			}
		}
	}
	placeholder := workloadIdentityMember("<CLUSTER_PROJECT>.svc.id.goog", "<NAMESPACE>", ksa)
	for _, m := range plan.missing {
		detail := "no workload_identity_pool is configured for these Kubernetes runtime targets on the Runtime Broker"
		if m.tooOld {
			detail = "the Runtime Broker does not report a Workload Identity pool (upgrade it and set workload_identity_pool)"
		}
		out = append(out, api.WorkloadIdentityBinding{
			KSA: ksa, Profiles: m.profiles,
			State:      api.WorkloadIdentityBindingSkipped,
			Reason:     api.WorkloadIdentityReasonNoPool,
			Detail:     detail,
			ManualStep: workloadIdentityManualStep(saEmail, placeholder),
		})
	}
	for _, b := range out {
		attrs := []any{"sa_email", saEmail, "state", b.State, "profiles", b.Profiles}
		if b.Member != "" {
			attrs = append(attrs, "member", b.Member)
		}
		if b.State == api.WorkloadIdentityBindingBound {
			slog.Info("GCP SA mint: Workload Identity binding granted", attrs...)
		} else {
			slog.Warn("GCP SA mint: Workload Identity binding not granted", append(attrs, "reason", b.Reason, "detail", b.Detail)...)
		}
	}
	return out
}

// addWorkloadIdentityMembers makes the batched grant with its retries.
func (s *Server) addWorkloadIdentityMembers(ctx context.Context, saEmail string, members []string) error {
	op := func() error {
		return s.gcpIAMAdmin.AddRoleMembers(ctx, saEmail, roleWorkloadIdentityUser, members)
	}
	err := retryIAMGrant(ctx, op)
	if err != nil && isIAMPolicyConflict(err) {
		err = retryIAMGrant(ctx, op)
	}
	return err
}

// setWorkloadIdentityFailure marks b as not bound because of err.
func setWorkloadIdentityFailure(b *api.WorkloadIdentityBinding, saEmail string, err error) {
	b.Reason = workloadIdentityErrorReason(err)
	b.State = api.WorkloadIdentityBindingSkipped
	switch b.Reason {
	case api.WorkloadIdentityReasonPermissionDenied:
		b.Detail = "the hub's service account may not change this account's IAM policy (it needs iam.serviceAccounts.setIamPolicy, part of roles/iam.serviceAccountAdmin)"
	case api.WorkloadIdentityReasonPoolNotFound:
		b.Detail = fmt.Sprintf("IAM does not know the Workload Identity pool %s; check workload_identity_pool and that Workload Identity is enabled on the cluster", b.Pool)
	default:
		b.State = api.WorkloadIdentityBindingFailed
		b.Detail = "the IAM policy update failed: " + err.Error()
	}
	b.ManualStep = workloadIdentityManualStep(saEmail, b.Member)
}

// workloadIdentityErrorReason maps an IAM error to a binding reason.
func workloadIdentityErrorReason(err error) string {
	var gerr *googleapi.Error
	if errors.As(err, &gerr) && gerr.Code == http.StatusForbidden {
		return api.WorkloadIdentityReasonPermissionDenied
	}
	if isWorkloadIdentityPoolNotFoundError(err) {
		return api.WorkloadIdentityReasonPoolNotFound
	}
	if msg := err.Error(); strings.Contains(msg, "PERMISSION_DENIED") || strings.Contains(msg, "Error 403") {
		return api.WorkloadIdentityReasonPermissionDenied
	}
	return api.WorkloadIdentityReasonIAMError
}

// isWorkloadIdentityPoolNotFoundError reports whether err is IAM's refusal
// of a member whose Workload Identity pool does not exist ("Identity Pool
// does not exist (<pool>)", HTTP 400).
func isWorkloadIdentityPoolNotFoundError(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "identity pool does not exist")
}

// isIAMPolicyConflict reports whether err is an etag conflict on a policy
// write (HTTP 409 or 412).
func isIAMPolicyConflict(err error) bool {
	var gerr *googleapi.Error
	if errors.As(err, &gerr) {
		return gerr.Code == http.StatusConflict || gerr.Code == http.StatusPreconditionFailed
	}
	return false
}

// projectWorkloadIdentityBindings runs the Workload Identity binding of a
// project-scoped mint when the experiment is on: the plan over the
// project's provider brokers, then the grant. Nil when the experiment is
// off or the project has no Kubernetes runtime target.
func (s *Server) projectWorkloadIdentityBindings(ctx context.Context, projectID, saEmail string) []api.WorkloadIdentityBinding {
	if !s.experimentEnabled(experiments.K8sWorkloadIdentitySetup) {
		return nil
	}
	brokers, _ := s.projectProviderBrokers(ctx, projectID)
	plan := workloadIdentityMintPlanFor(brokers)
	if plan.empty() {
		return nil
	}
	return s.grantWorkloadIdentityBestEffort(ctx, saEmail, plan)
}

// logWorkloadIdentityBindingsAudit audits each binding entry of a mint: a
// bound entry as a success, any other as a failure whose reason names the
// reason code and the member (or the runtime targets with no pool). The
// structured log lines of grantWorkloadIdentityBestEffort carry the member
// of a bound entry.
func (s *Server) logWorkloadIdentityBindingsAudit(ctx context.Context, projectID string, sa *store.GCPServiceAccount, bindings []api.WorkloadIdentityBinding) {
	for _, b := range bindings {
		bound := b.State == api.WorkloadIdentityBindingBound
		reason := ""
		if !bound {
			target := b.Member
			if target == "" {
				target = strings.Join(b.Profiles, ",")
			}
			reason = b.Reason + " " + target
		}
		LogGCPTokenGeneration(ctx, s.auditLogger, GCPTokenEventWIBinding,
			"", projectID, sa.Email, sa.ID, bound, reason)
	}
}
