/*
Copyright 2026 The Scion Authors.
*/

package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/auth"
	"google.golang.org/api/option"

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

func TestCloudRunRunDiagnostics_ChecksAllConfiguredAgentAccounts(t *testing.T) {
	const (
		workSA    = "sa-work@test-project.iam.gserviceaccount.com"
		blockSA   = "sa-block@test-project.iam.gserviceaccount.com"
		assignSA1 = "sa-assign-1@test-project.iam.gserviceaccount.com"
		assignSA2 = "sa-assign-2@test-project.iam.gserviceaccount.com"
	)

	newTestRT := func(t *testing.T) *CloudRunRuntime {
		t.Helper()
		rt, err := NewCloudRunRuntime(&config.CloudRunConfig{
			ProjectID:           "test-project",
			Location:            "us-central1",
			ServiceAccount:      workSA,
			BlockServiceAccount: blockSA,
			AssignableServiceAccounts: []string{
				"  SA-WORK@test-project.iam.gserviceaccount.com ", // duplicate of ServiceAccount after normalization
				assignSA1,
				assignSA2,
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		rt.newClient = func(context.Context) (cloudrun.InstancesAPI, error) { return nil, errors.New("no client") }
		return rt
	}

	wantOrder := []string{workSA, blockSA, assignSA1, assignSA2}

	t.Run("all granted passes and deduplicates normalized emails", func(t *testing.T) {
		rt := newTestRT(t)
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
		if !reflect.DeepEqual(calls, wantOrder) {
			t.Fatalf("tester calls = %v, want %v", calls, wantOrder)
		}
		if found.Status != "pass" {
			t.Fatalf("Status = %q, want pass (%+v)", found.Status, *found)
		}
		for _, sa := range wantOrder {
			if !strings.Contains(found.Message, sa) {
				t.Errorf("pass Message %q does not name %q", found.Message, sa)
			}
		}
	})

	t.Run("missing or errored account warns naming affected accounts on single-line remediation", func(t *testing.T) {
		rt := newTestRT(t)
		var calls []string
		rt.testActAs = func(_ context.Context, sa string) (bool, error) {
			calls = append(calls, sa)
			switch sa {
			case assignSA1:
				return false, nil
			case assignSA2:
				return false, errors.New("iam boom")
			default:
				return true, nil
			}
		}

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
		if !reflect.DeepEqual(calls, wantOrder) {
			t.Fatalf("tester calls = %v, want %v", calls, wantOrder)
		}
		if found.Status != "warn" {
			t.Fatalf("Status = %q, want warn (%+v)", found.Status, *found)
		}
		for _, want := range []string{
			"Permission 'iam.serviceaccounts.actAs' denied on service account " + assignSA1 + " (or it may not exist)",
			assignSA2,
			"iam boom",
		} {
			if !strings.Contains(found.Message, want) {
				t.Errorf("Message %q lacks %q", found.Message, want)
			}
		}
		for _, want := range []string{
			"gcloud iam service-accounts add-iam-policy-binding " + assignSA1,
			"roles/iam.serviceAccountUser",
		} {
			if !strings.Contains(found.Remediation, want) {
				t.Errorf("Remediation %q lacks %q", found.Remediation, want)
			}
		}
		if strings.Contains(found.Remediation, "\n") {
			t.Errorf("Remediation spans lines: %q", found.Remediation)
		}
	})
}

type fixedBearerTokenProvider string

func (p fixedBearerTokenProvider) Token(context.Context) (*auth.Token, error) {
	return &auth.Token{
		Value:  string(p),
		Type:   "Bearer",
		Expiry: time.Now().Add(time.Hour),
	}, nil
}

// TestFlatCloudRunRunDiagnostics_UsesInstanceCredentialsNotADC verifies that a
// flat CloudRunRuntime runs iam.serviceAccounts.actAs with its own instance
// credentials (r.clientOpts from runtime_target.credentials_file), never
// ambient Application Default Credentials, across all configured agent SAs.
func TestFlatCloudRunRunDiagnostics_UsesInstanceCredentialsNotADC(t *testing.T) {
	// Poison ambient ADC so any default iamv1.NewService(ctx) call without
	// r.clientOpts fails immediately.
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", filepath.Join(t.TempDir(), "nonexistent-adc.json"))

	const (
		instanceToken = "flat-instance-cred-token-987"
		workSA        = "sa-work@test-project.iam.gserviceaccount.com"
		blockSA       = "sa-block@test-project.iam.gserviceaccount.com"
		assignSA      = "sa-assign@test-project.iam.gserviceaccount.com"
	)

	var (
		mu       sync.Mutex
		authSeen []string
		saSeen   []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const prefix = "/v1/projects/-/serviceAccounts/"
		const suffix = ":testIamPermissions"
		if r.Method != http.MethodPost || !strings.HasPrefix(r.URL.Path, prefix) || !strings.HasSuffix(r.URL.Path, suffix) {
			http.NotFound(w, r)
			return
		}
		sa := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, prefix), suffix)
		mu.Lock()
		authSeen = append(authSeen, r.Header.Get("Authorization"))
		saSeen = append(saSeen, sa)
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"permissions": []string{actAsPermission},
		})
	}))
	defer srv.Close()

	creds := auth.NewCredentials(&auth.CredentialsOptions{
		TokenProvider: fixedBearerTokenProvider(instanceToken),
	})
	rt, err := NewFlatCloudRunRuntime(config.CloudRunConfig{
		ProjectID:                 "test-project",
		Location:                  "us-central1",
		ServiceAccount:            workSA,
		BlockServiceAccount:       blockSA,
		AssignableServiceAccounts: []string{assignSA, workSA},
	}, creds)
	if err != nil {
		t.Fatalf("NewFlatCloudRunRuntime: %v", err)
	}
	rt.clientOpts = append(rt.clientOpts, option.WithEndpoint(srv.URL))
	rt.newClient = func(context.Context) (cloudrun.InstancesAPI, error) { return &fakeInstancesClient{}, nil }

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
	if found.Status != "pass" {
		t.Fatalf("actAs check = %+v, want pass", *found)
	}

	wantSAs := []string{workSA, blockSA, assignSA}
	if !reflect.DeepEqual(saSeen, wantSAs) {
		t.Fatalf("IAM testIamPermissions SAs = %v, want %v", saSeen, wantSAs)
	}
	for i, gotAuth := range authSeen {
		if want := "Bearer " + instanceToken; gotAuth != want {
			t.Errorf("request %d Authorization = %q, want %q", i, gotAuth, want)
		}
	}
}
