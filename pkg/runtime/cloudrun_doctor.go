package runtime

import (
	"context"
	"fmt"
	"time"

	"cloud.google.com/go/run/apiv2/runpb"
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

	return report
}
