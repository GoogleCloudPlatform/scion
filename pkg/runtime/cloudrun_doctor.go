package runtime

import (
	"context"
	"fmt"
	"slices"
	"time"

	"cloud.google.com/go/run/apiv2/runpb"
	iamv1 "google.golang.org/api/iam/v1"
	"google.golang.org/api/iterator"
)

// RunDiagnostics implements the Diagnosable interface for CloudRunRuntime.
func (r *CloudRunRuntime) RunDiagnostics(opts DiagnosticOpts) DiagnosticReport {
	report := DiagnosticReport{
		Runtime: r.Name(),
		Checks:  []CheckResult{},
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
		check.Remediation = "Verify GCP credentials (e.g., gcloud auth application-default login)."
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

	// Check 2: actAs on the instance service account (ptone/scion#7102).
	tester := r.testActAs
	if tester == nil {
		tester = testActAsIAM
	}
	// Its own timeout, so a slow API check above cannot starve it.
	iamCtx, iamCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer iamCancel()
	report.Checks = append(report.Checks, cloudRunActAsCheck(iamCtx, r.config.ServiceAccount, tester))

	return report
}

// actAsPermission is the permission Cloud Run checks on the caller when an
// Instance is created to run as a service account.
const actAsPermission = "iam.serviceAccounts.actAs"

// actAsTester reports whether the caller's credentials hold actAsPermission
// on the service account sa.
type actAsTester func(ctx context.Context, sa string) (bool, error)

// testActAsIAM asks IAM (testIamPermissions on the service account) whether
// the credentials doctor runs with may act as sa. It is read-only and needs
// no permission of its own.
func testActAsIAM(ctx context.Context, sa string) (bool, error) {
	svc, err := iamv1.NewService(ctx)
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

// cloudRunActAsCheck checks that the caller may act as the configured
// cloudrun.service_account. Without iam.serviceAccounts.actAs on it every
// agent start fails with "Permission 'iam.serviceaccounts.actAs' denied on
// service account <sa> (or it may not exist)", including when
// it is the hub's own service account. testIamPermissions answers for the
// identity doctor runs as, which is the launching identity only when doctor
// runs with the hub's (broker's) credentials, so a missing grant is a warning
// rather than a failure.
func cloudRunActAsCheck(ctx context.Context, sa string, test actAsTester) CheckResult {
	check := CheckResult{Name: "Cloud Run Instance Service Account (actAs)"}
	if sa == "" {
		check.Status = "skip"
		check.Message = "cloudrun.service_account is not set; Instances run as the project's default service account"
		return check
	}
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
