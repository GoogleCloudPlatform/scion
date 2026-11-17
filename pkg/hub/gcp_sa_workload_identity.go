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
	"time"
	"unicode"

	"google.golang.org/api/googleapi"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
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

// maxWorkloadIdentityTargets caps the Workload Identity targets stored for
// one broker. A legacy broker reports one per Kubernetes profile and a flat
// instance at most one, so the cap only bounds a broken or hostile report.
const maxWorkloadIdentityTargets = 64

// maxWorkloadIdentityProfileLen bounds a reported profile name, which is
// shown in the mint response and the CLI.
const maxWorkloadIdentityProfileLen = 253

// workloadIdentityTargetDrops counts what normalizeWorkloadIdentityTargets
// left out or changed.
type workloadIdentityTargetDrops struct {
	// profiles: profiles cleared on a flat row.
	profiles int
	// invalid: entries dropped for a malformed pool, namespace or profile.
	invalid int
	// overCap: valid entries dropped past maxWorkloadIdentityTargets.
	overCap int
}

// normalizeWorkloadIdentityTargets returns the targets to store from a
// heartbeat's reported list. The hub does not trust what a broker sends:
// the pool and namespace end up in an IAM member and in the gcloud command
// a person is told to run, so an entry is kept only when its pool passes
// config.ValidateWorkloadIdentityPool, its namespace is a DNS-1123 label,
// and its profile has no control characters and a bounded length. Values
// are trimmed, at most maxWorkloadIdentityTargets entries are kept, and on
// a flat row (which has no profiles) each profile is cleared. The result is
// never nil.
func normalizeWorkloadIdentityTargets(reported []store.WorkloadIdentityTarget, flat bool) ([]store.WorkloadIdentityTarget, workloadIdentityTargetDrops) {
	out := make([]store.WorkloadIdentityTarget, 0, min(len(reported), maxWorkloadIdentityTargets))
	var drops workloadIdentityTargetDrops
	for _, t := range reported {
		t.Profile = strings.TrimSpace(t.Profile)
		t.Pool = strings.TrimSpace(t.Pool)
		t.Namespace = strings.TrimSpace(t.Namespace)
		if t.Pool == "" {
			continue
		}
		if config.ValidateWorkloadIdentityPool(t.Pool) != nil ||
			len(validation.IsDNS1123Label(t.Namespace)) > 0 ||
			!validWorkloadIdentityProfile(t.Profile) {
			drops.invalid++
			continue
		}
		if flat && t.Profile != "" {
			t.Profile = ""
			drops.profiles++
		}
		if len(out) == maxWorkloadIdentityTargets {
			drops.overCap++
			continue
		}
		out = append(out, t)
	}
	return out, drops
}

// validWorkloadIdentityProfile reports whether a reported profile name may
// be stored: at most maxWorkloadIdentityProfileLen bytes of printable,
// non-space characters (empty is allowed).
func validWorkloadIdentityProfile(p string) bool {
	if len(p) > maxWorkloadIdentityProfileLen {
		return false
	}
	for _, r := range p {
		if r <= ' ' || r == 0x7f || !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

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
	// why says why no pool is reported.
	why workloadIdentityMissingReason
}

// workloadIdentityMissingReason is why a broker's Kubernetes targets report
// no Workload Identity pool.
type workloadIdentityMissingReason int

const (
	// wiMissingNotConfigured: the broker reports targets
	// (wi_targets_v1), and these have no workload_identity_pool.
	wiMissingNotConfigured workloadIdentityMissingReason = iota
	// wiMissingTooOld: a legacy broker without wi_targets_v1, too old to
	// report a pool.
	wiMissingTooOld
	// wiMissingFlat: a flat Runtime Broker instance, which does not report
	// a pool yet (ptone/scion#3329 phase 5 P3b), whatever its version.
	wiMissingFlat
)

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
			why := wiMissingNotConfigured
			switch {
			case flat:
				why = wiMissingFlat
			case b.Capabilities == nil || !b.Capabilities.WITargetsV1:
				why = wiMissingTooOld
			}
			plan.missing = append(plan.missing, workloadIdentityMissing{profiles: missing, why: why})
		}
	}
	for i := range plan.targets {
		sort.Strings(plan.targets[i].profiles)
	}
	sort.Slice(plan.missing, func(i, j int) bool {
		return plan.missing[i].profiles[0] < plan.missing[j].profiles[0]
	})
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
	return fmt.Sprintf("gcloud iam service-accounts add-iam-policy-binding %s --role %s --member %s",
		shellSingleQuote(saEmail), roleWorkloadIdentityUser, shellSingleQuote(member))
}

// shellSingleQuote quotes s for a POSIX shell: inside single quotes nothing
// is expanded, and an embedded single quote is closed, escaped and reopened.
// The member and email are validated before they get here, so this is
// defence in depth: no reported value can turn the manual step into
// another command.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
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
		switch m.why {
		case wiMissingTooOld:
			detail = "the Runtime Broker does not report a Workload Identity pool (upgrade it and set workload_identity_pool)"
		case wiMissingFlat:
			detail = "flat Runtime Broker instances do not report a Workload Identity pool yet; make the grant by hand"
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
	if err == nil {
		s.wiRoleMembersCache.invalidate(saEmail)
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
		b.Detail = "the IAM policy update failed: " + sanitizeWorkloadIdentityDetail(err.Error())
	}
	b.ManualStep = workloadIdentityManualStep(saEmail, b.Member)
}

// maxWorkloadIdentityDetailLen bounds the IAM error text echoed in a
// failed entry's detail.
const maxWorkloadIdentityDetailLen = 300

// sanitizeWorkloadIdentityDetail makes IAM error text safe to show on one
// line: control characters (newlines included) become spaces, runs of
// spaces collapse, and the text is cut at maxWorkloadIdentityDetailLen
// runes. It can then never add a line of its own (such as a fake manual
// step) to the CLI output.
func sanitizeWorkloadIdentityDetail(msg string) string {
	var b strings.Builder
	space := false
	n := 0
	for _, r := range msg {
		blank := unicode.IsControl(r) || unicode.IsSpace(r) || !unicode.IsPrint(r)
		if blank && (space || b.Len() == 0) {
			continue
		}
		if n == maxWorkloadIdentityDetailLen {
			b.WriteString("…")
			break
		}
		if blank {
			b.WriteByte(' ')
		} else {
			b.WriteRune(r)
		}
		space = blank
		n++
	}
	return strings.TrimSpace(b.String())
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
// project-scoped mint when the experiment is on (the plan over the project's
// provider brokers, then the grant) and returns a mapping-warning view that
// reuses the provider broker list it loaded.
func (s *Server) projectWorkloadIdentityBindings(ctx context.Context, projectID, saEmail string) ([]api.WorkloadIdentityBinding, func() projectSAMappingView) {
	if !s.experimentEnabled(experiments.K8sWorkloadIdentitySetup) {
		return nil, func() projectSAMappingView { return s.projectSAMappings(ctx, projectID) }
	}
	brokers, complete := s.projectProviderBrokers(ctx, projectID)
	if !complete {
		// The plan may miss a broker, or (when the provider list could not
		// be read) be empty, which would read as "no Kubernetes target".
		slog.Warn("GCP SA mint: the project's provider brokers could not all be read; Workload Identity bindings may miss some runtime targets",
			"project_id", projectID, "sa_email", saEmail, "brokers_read", len(brokers))
	}
	warningView := func() projectSAMappingView {
		return projectSAMappingViewFrom(s.brokerKubernetesProfileMappings(brokers))
	}
	plan := workloadIdentityMintPlanFor(brokers)
	if plan.empty() {
		return nil, warningView
	}
	return s.grantWorkloadIdentityBestEffort(ctx, saEmail, plan), warningView
}

// hubWorkloadIdentityBindings runs the Workload Identity binding of a
// hub-scoped mint when the experiment is on (the plan over every Runtime
// Broker on the hub, then the grant; design §3.4) and returns a
// mapping-warning view that reuses a complete broker walk.
func (s *Server) hubWorkloadIdentityBindings(ctx context.Context, saEmail string) ([]api.WorkloadIdentityBinding, func() projectSAMappingView) {
	if !s.experimentEnabled(experiments.K8sWorkloadIdentitySetup) {
		return nil, func() projectSAMappingView { return s.hubSAMappings(ctx) }
	}
	brokers, complete := s.hubRuntimeBrokers(ctx)
	warningView := func() projectSAMappingView { return s.hubSAMappings(ctx) }
	if complete {
		warningView = func() projectSAMappingView {
			return mappingViewFrom("this hub", s.brokerKubernetesProfileMappings(brokers))
		}
	} else {
		slog.Warn("GCP SA mint (hub scope): the hub's Runtime Brokers could not all be read; Workload Identity bindings may miss some runtime targets",
			"sa_email", saEmail, "brokers_read", len(brokers))
	}
	plan := workloadIdentityMintPlanFor(brokers)
	if plan.empty() {
		return nil, warningView
	}
	return s.grantWorkloadIdentityBestEffort(ctx, saEmail, plan), warningView
}

// logWorkloadIdentityBindingsAudit audits each binding entry of a mint: a
// bound entry as a success naming the granted IAM member, and any other as
// a failure whose reason names the reason code and the member (or the
// runtime targets with no pool).
func (s *Server) logWorkloadIdentityBindingsAudit(ctx context.Context, projectID string, sa *store.GCPServiceAccount, bindings []api.WorkloadIdentityBinding) {
	if s.auditLogger == nil {
		return
	}
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
		_ = s.auditLogger.LogGCPTokenEvent(ctx, &GCPTokenEvent{
			EventType:           GCPTokenEventWIBinding,
			ProjectID:           projectID,
			ServiceAccountEmail: sa.Email,
			ServiceAccountID:    sa.ID,
			Member:              b.Member,
			Success:             bound,
			FailReason:          reason,
			Timestamp:           time.Now(),
		})
	}
}
