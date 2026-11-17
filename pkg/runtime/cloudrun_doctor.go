package runtime

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"cloud.google.com/go/run/apiv2/runpb"
	iamv1 "google.golang.org/api/iam/v1"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// RunDiagnostics implements the Diagnosable interface for CloudRunRuntime.
func (r *CloudRunRuntime) RunDiagnostics(opts DiagnosticOpts) DiagnosticReport {
	report := DiagnosticReport{
		Runtime: r.Name(),
		Checks:  []CheckResult{},
	}

	// The homes storage setting does not apply to this runtime: say so
	// (fixed text, no paths), as the broker does at start.
	if w := CloudRunHomesWarningFor(r.Name(), opts.Server); w != "" {
		report.Checks = append(report.Checks, CheckResult{
			Name:        "Cloud Run Agent Homes",
			Status:      "warn",
			Message:     w,
			Remediation: "No action is needed for Cloud Run agents; remove the setting if no other runtime on this broker uses it.",
		})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Check 0: Resolve config (auto-discover project/region if needed)
	if err := r.resolveConfig(ctx); err != nil {
		report.Checks = append(report.Checks, CheckResult{
			Name:        "Cloud Run Config Resolution",
			Status:      "fail",
			Message:     fmt.Sprintf("Failed to resolve Cloud Run config from GCP metadata: %v", err),
			Remediation: "Set project_id and location in the cloudrun runtime config, or verify the GCE metadata server is reachable.",
		})
		return report
	}

	// Check 1: API Access & Project/Location configuration
	check := CheckResult{
		Name: "Cloud Run API & Configuration",
	}

	c, err := r.client(ctx)
	if err != nil {
		check.Status = "fail"
		check.Message = fmt.Sprintf("Failed to initialize Cloud Run client: %v", err)
		if len(r.clientOpts) > 0 {
			check.Remediation = "Verify runtime_target.credentials_file for this Cloud Run instance."
		} else {
			check.Remediation = "Verify GCP credentials (e.g., gcloud auth application-default login)."
		}
	} else {
		defer func() { _ = c.Close() }()

		parent := fmt.Sprintf("projects/%s/locations/%s", r.config.ProjectID, r.config.Location)
		req := &runpb.ListInstancesRequest{
			Parent:   parent,
			PageSize: 1,
		}

		it := c.ListInstances(ctx, req, defaultCallOpts...)
		_, err := it.Next()
		if err != nil && err != iterator.Done {
			check.Status = "fail"
			check.Message = fmt.Sprintf("Failed to list instances in %s: %v", parent, err)
			check.Remediation = "Verify that the configured ProjectID and Location are correct, and that the service account has 'roles/run.viewer' or 'roles/run.admin'."
		} else {
			check.Status = "pass"
			check.Message = fmt.Sprintf("Successfully connected to Cloud Run API in %s", parent)
		}
	}
	report.Checks = append(report.Checks, check)

	// Check 2: actAs across every configured agent service account
	// (service_account, block_service_account, assignable_service_accounts)
	// using the runtime's own credentials when configured (ptone/scion#7102,
	// ptone/scion#8187).
	tester := r.testActAs
	if tester == nil {
		tester = newIAMActAsTester(r.clientOpts...)
	}
	// Its own timeout, so a slow API check above cannot starve it.
	iamCtx, iamCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer iamCancel()
	report.Checks = append(report.Checks, cloudRunActAsCheckForAccounts(iamCtx, cloudRunAgentServiceAccounts(r.config), tester, len(r.clientOpts) > 0))

	return report
}

// actAsPermission is the permission Cloud Run checks on the caller when an
// Instance is created to run as a service account.
const actAsPermission = "iam.serviceAccounts.actAs"

// actAsTester reports whether the caller's credentials hold actAsPermission
// on the service account sa.
type actAsTester func(ctx context.Context, sa string) (bool, error)

// newIAMActAsTester builds an actAsTester using opts (for flat Cloud Run
// instances, the runtime_target.credentials_file client options; for legacy
// runtimes, empty options so Application Default Credentials are used).
func newIAMActAsTester(opts ...option.ClientOption) actAsTester {
	return func(ctx context.Context, sa string) (bool, error) {
		svc, err := iamv1.NewService(ctx, opts...)
		if err != nil {
			return false, err
		}
		resp, err := svc.Projects.ServiceAccounts.TestIamPermissions(
			"projects/-/serviceAccounts/"+sa,
			&iamv1.TestIamPermissionsRequest{Permissions: []string{actAsPermission}},
		).Context(ctx).Do()
		if err != nil {
			return false, err
		}
		return slices.Contains(resp.Permissions, actAsPermission), nil
	}
}

// cloudRunAgentServiceAccounts returns the unique normalized agent service
// account emails configured on cfg, in deterministic order:
// service_account, block_service_account, then assignable_service_accounts.
func cloudRunAgentServiceAccounts(cfg *config.CloudRunConfig) []string {
	if cfg == nil {
		return nil
	}
	raw := make([]string, 0, 2+len(cfg.AssignableServiceAccounts))
	raw = append(raw, cfg.ServiceAccount, cfg.BlockServiceAccount)
	raw = append(raw, cfg.AssignableServiceAccounts...)
	return normalizeCloudRunServiceAccounts(raw)
}

func normalizeCloudRunServiceAccounts(raw []string) []string {
	seen := make(map[string]struct{}, len(raw))
	out := make([]string, 0, len(raw))
	for _, sa := range raw {
		norm := strings.ToLower(strings.TrimSpace(sa))
		if norm == "" {
			continue
		}
		if _, dup := seen[norm]; dup {
			continue
		}
		seen[norm] = struct{}{}
		out = append(out, norm)
	}
	return out
}

// cloudRunActAsCheck checks that the caller may act as sa.
func cloudRunActAsCheck(ctx context.Context, sa string, test actAsTester) CheckResult {
	return cloudRunActAsCheckForAccounts(ctx, normalizeCloudRunServiceAccounts([]string{sa}), test, false)
}

// cloudRunActAsCheckForAccounts checks that the caller may act as every
// configured Cloud Run agent service account. Without iam.serviceAccounts.actAs
// on a target account, starts as that account fail with "Permission
// 'iam.serviceaccounts.actAs' denied on service account <sa> (or it may not
// exist)", including when it is the hub's own service account. When
// flatCredentials is true, doctor is already running with the flat instance's
// runtime_target.credentials_file principal rather than ambient Hub ADC.
func cloudRunActAsCheckForAccounts(ctx context.Context, accounts []string, test actAsTester, flatCredentials bool) CheckResult {
	check := CheckResult{Name: "Cloud Run Instance Service Account (actAs)"}
	if len(accounts) == 0 {
		check.Status = "skip"
		check.Message = "cloudrun.service_account is not set; Instances run as the project's default service account"
		return check
	}
	if len(accounts) == 1 && !flatCredentials {
		sa := accounts[0]
		remediation := fmt.Sprintf("Grant the identity that starts agents (the hub's service account, even when it is %s itself) roles/iam.serviceAccountUser on %s: "+
			"gcloud iam service-accounts add-iam-policy-binding %s --member=serviceAccount:HUB_SA_EMAIL --role=roles/iam.serviceAccountUser", sa, sa, sa)
		granted, err := test(ctx, sa)
		switch {
		case err != nil:
			check.Status = "warn"
			check.Message = fmt.Sprintf("Could not check %s on %s: %v", actAsPermission, sa, err)
			check.Remediation = remediation
		case granted:
			check.Status = "pass"
			check.Message = fmt.Sprintf("The credentials doctor runs with hold %s on %s", actAsPermission, sa)
		default:
			check.Status = "warn"
			check.Message = fmt.Sprintf("The credentials doctor runs with lack %s on %s. If these are the credentials that start agents, every start fails with \"Permission 'iam.serviceaccounts.actAs' denied on service account %s (or it may not exist)\"; if not, run doctor with the hub's credentials to check the identity that starts agents.", actAsPermission, sa, sa)
			check.Remediation = remediation
		}
		return check
	}

	memberPlaceholder := "HUB_SA_EMAIL"
	if flatCredentials {
		memberPlaceholder = "INSTANCE_CREDENTIAL_PRINCIPAL"
	}
	var (
		parts    []string
		affected []string
		cmds     []string
	)
	for _, sa := range accounts {
		granted, err := test(ctx, sa)
		switch {
		case err != nil:
			affected = append(affected, sa)
			parts = append(parts, fmt.Sprintf("could not check %s on %s: %v", actAsPermission, sa, err))
			cmds = append(cmds, fmt.Sprintf("gcloud iam service-accounts add-iam-policy-binding %s --member=serviceAccount:%s --role=roles/iam.serviceAccountUser", sa, memberPlaceholder))
		case !granted:
			affected = append(affected, sa)
			parts = append(parts, fmt.Sprintf("missing %s on %s (starts as this account fail with \"Permission 'iam.serviceaccounts.actAs' denied on service account %s (or it may not exist)\")", actAsPermission, sa, sa))
			cmds = append(cmds, fmt.Sprintf("gcloud iam service-accounts add-iam-policy-binding %s --member=serviceAccount:%s --role=roles/iam.serviceAccountUser", sa, memberPlaceholder))
		}
	}
	if len(affected) == 0 {
		check.Status = "pass"
		check.Message = fmt.Sprintf("The credentials doctor runs with hold %s on all %d configured agent service accounts (%s)",
			actAsPermission, len(accounts), strings.Join(accounts, ", "))
		return check
	}
	check.Status = "warn"
	if flatCredentials {
		check.Message = fmt.Sprintf("Using the credentials doctor runs with: %s.",
			strings.Join(parts, "; "))
	} else {
		check.Message = fmt.Sprintf("Using the credentials doctor runs with: %s; if these are not the credentials that start agents, run doctor with the broker's credentials.",
			strings.Join(parts, "; "))
	}
	check.Remediation = fmt.Sprintf("Grant the identity that starts agents roles/iam.serviceAccountUser on each affected account (%s): %s",
		strings.Join(affected, ", "), strings.Join(cmds, " ; "))
	return check
}
