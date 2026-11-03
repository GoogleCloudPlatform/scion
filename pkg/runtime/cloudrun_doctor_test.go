/*
Copyright 2026 The Scion Authors.
*/

package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime/cloudrun"
)

const testInstanceSA = "scion-hub@test-project.iam.gserviceaccount.com"

func stubActAs(granted bool, err error, calls *[]string) actAsTester {
	return func(_ context.Context, sa string) (bool, error) {
		*calls = append(*calls, sa)
		return granted, err
	}
}

func TestCloudRunActAsCheck_Granted(t *testing.T) {
	var calls []string
	got := cloudRunActAsCheck(context.Background(), testInstanceSA, stubActAs(true, nil, &calls))
	if got.Status != "pass" {
		t.Fatalf("Status = %q, want pass (%+v)", got.Status, got)
	}
	if len(calls) != 1 || calls[0] != testInstanceSA {
		t.Fatalf("tester calls = %v, want [%s]", calls, testInstanceSA)
	}
}

// TestCloudRunActAsCheck_Denied: a missing grant is a warning, not a failure
// (doctor may not run as the identity that starts agents), names the exact
// start error and the caveat, and gives the SA-scoped grant.
func TestCloudRunActAsCheck_Denied(t *testing.T) {
	var calls []string
	got := cloudRunActAsCheck(context.Background(), testInstanceSA, stubActAs(false, nil, &calls))
	if got.Status != "warn" {
		t.Fatalf("Status = %q, want warn (%+v)", got.Status, got)
	}
	for _, want := range []string{"Permission 'iam.serviceaccounts.actAs' denied on service account " + testInstanceSA + " (or it may not exist)", "credentials doctor runs with", testInstanceSA} {
		if !strings.Contains(got.Message, want) {
			t.Errorf("Message %q lacks %q", got.Message, want)
		}
	}
	for _, want := range []string{"gcloud iam service-accounts add-iam-policy-binding " + testInstanceSA, "roles/iam.serviceAccountUser"} {
		if !strings.Contains(got.Remediation, want) {
			t.Errorf("Remediation %q lacks %q", got.Remediation, want)
		}
	}
	if strings.Contains(got.Remediation, "\n") {
		t.Errorf("Remediation spans lines: %q", got.Remediation)
	}
}

func TestCloudRunActAsCheck_EmptyServiceAccountSkips(t *testing.T) {
	var calls []string
	got := cloudRunActAsCheck(context.Background(), "", stubActAs(true, nil, &calls))
	if got.Status != "skip" {
		t.Fatalf("Status = %q, want skip (%+v)", got.Status, got)
	}
	if len(calls) != 0 {
		t.Fatalf("tester called %v for an empty service account", calls)
	}
}

func TestCloudRunActAsCheck_APIErrorWarns(t *testing.T) {
	var calls []string
	got := cloudRunActAsCheck(context.Background(), testInstanceSA, stubActAs(false, errors.New("boom"), &calls))
	if got.Status != "warn" || !strings.Contains(got.Message, "boom") {
		t.Fatalf("got %+v, want a warn carrying the API error", got)
	}
}

// TestCloudRunRunDiagnostics_IncludesActAsCheck: RunDiagnostics runs the
// actAs check through the runtime's tester, even when the API check fails.
func TestCloudRunRunDiagnostics_IncludesActAsCheck(t *testing.T) {
	rt, err := NewCloudRunRuntime(&config.CloudRunConfig{
		ProjectID:      "test-project",
		Location:       "us-central1",
		ServiceAccount: testInstanceSA,
	})
	if err != nil {
		t.Fatal(err)
	}
	rt.newClient = func(context.Context) (cloudrun.InstancesAPI, error) { return nil, errors.New("no client") }
	var calls []string
	rt.testActAs = stubActAs(true, nil, &calls)

	report := rt.RunDiagnostics(DiagnosticOpts{})
	var found *CheckResult
	for i := range report.Checks {
		if strings.Contains(report.Checks[i].Name, "actAs") {
			found = &report.Checks[i]
		}
	}
	if found == nil {
		t.Fatalf("no actAs check in %+v", report.Checks)
	}
	if found.Status != "pass" || len(calls) != 1 || calls[0] != testInstanceSA {
		t.Fatalf("actAs check %+v, tester calls %v", *found, calls)
	}
}
