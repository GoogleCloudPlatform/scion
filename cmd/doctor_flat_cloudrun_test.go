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

package cmd

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"cloud.google.com/go/auth"
	"cloud.google.com/go/auth/credentials"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	scionruntime "github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

type stubDiagnosable struct {
	report scionruntime.DiagnosticReport
	opts   scionruntime.DiagnosticOpts
}

func (s *stubDiagnosable) RunDiagnostics(opts scionruntime.DiagnosticOpts) scionruntime.DiagnosticReport {
	s.opts = opts
	return s.report
}

type noopTokenProvider struct{}

func (noopTokenProvider) Token(context.Context) (*auth.Token, error) {
	return nil, errors.New("unused in stub test")
}

func TestDoctorFlatCloudRun_SkipsNonCloudRunInstances(t *testing.T) {
	instances := []config.V1RuntimeBrokerInstanceConfig{
		{Key: "local-docker", RuntimeTarget: &config.V1RuntimeTargetConfig{Type: config.RuntimeTargetTypeDocker}},
		{Key: "cluster-k8s", RuntimeTarget: &config.V1RuntimeTargetConfig{Type: config.RuntimeTargetTypeKubernetes, Namespace: "agents"}},
	}
	got := checkDoctorFlatCloudRunInstances(instances, nil, defaultFlatCloudRunDoctorDeps())
	if len(got) != 0 {
		t.Fatalf("expected zero checks for non-cloudrun instances, got %+v", got)
	}
}

func TestDoctorFlatCloudRun_UnsupportedPlatformSkipsWithoutLeakingPath(t *testing.T) {
	const secretPath = "/secret/dir/cr-broker-creds.json"
	instances := []config.V1RuntimeBrokerInstanceConfig{{
		Key: "cr-east",
		RuntimeTarget: &config.V1RuntimeTargetConfig{
			Type:                config.RuntimeTargetTypeCloudRun,
			ProjectID:           "example-project",
			Region:              "us-east1",
			ServiceAccount:      "work@example-project.iam.gserviceaccount.com",
			BlockServiceAccount: "block@example-project.iam.gserviceaccount.com",
			CredentialsFile:     secretPath,
		},
	}}
	deps := flatCloudRunDoctorDeps{
		openCreds: func(string) ([]byte, error) {
			return nil, fmt.Errorf("checking a cloudrun credentials_file: %w on this platform", errors.ErrUnsupported)
		},
	}
	got := checkDoctorFlatCloudRunInstances(instances, nil, deps)
	if len(got) != 1 || got[0].Status != "skip" || got[0].Name != "flat-cloudrun[cr-east]" {
		t.Fatalf("got %+v, want single skip check for flat-cloudrun[cr-east]", got)
	}
	for _, field := range []string{got[0].Name, got[0].Message, got[0].Remediation} {
		if strings.Contains(field, secretPath) {
			t.Errorf("check leaked credentials_file path %q in %q", secretPath, field)
		}
	}
}

func TestDoctorFlatCloudRun_BuildsRuntimeWithAllAgentSAsAndPrefixesChecks(t *testing.T) {
	const (
		secretPath  = "/secret/dir/cr-broker-creds.json"
		principalSA = "broker-principal@example-project.iam.gserviceaccount.com"
		workSA      = "work@example-project.iam.gserviceaccount.com"
		blockSA     = "block@example-project.iam.gserviceaccount.com"
		assignSA1   = "assign-1@example-project.iam.gserviceaccount.com"
		assignSA2   = "assign-2@example-project.iam.gserviceaccount.com"
	)

	instances := []config.V1RuntimeBrokerInstanceConfig{
		{Key: "local-docker", RuntimeTarget: &config.V1RuntimeTargetConfig{Type: config.RuntimeTargetTypeDocker}},
		{
			Key: "cr-east",
			RuntimeTarget: &config.V1RuntimeTargetConfig{
				Type:                      config.RuntimeTargetTypeCloudRun,
				ProjectID:                 "example-project",
				Region:                    "us-east1",
				ServiceAccount:            workSA,
				BlockServiceAccount:       blockSA,
				AssignableServiceAccounts: []string{assignSA1, assignSA2},
				Network:                   "default",
				Subnetwork:                "agents",
				CredentialsFile:           secretPath,
			},
		},
	}
	srvCfg := &config.V1ServerConfig{
		Broker: &config.V1BrokerConfig{Instances: instances},
	}

	stubDiag := &stubDiagnosable{
		report: scionruntime.DiagnosticReport{
			Runtime: "cloudrun",
			Checks: []scionruntime.CheckResult{
				{Name: "Cloud Run API & Configuration", Status: "pass", Message: "ok"},
				{Name: "Cloud Run Instance Service Account (actAs)", Status: "pass", Message: "all 4 agent SAs ok"},
			},
		},
	}

	wantCreds := auth.NewCredentials(&auth.CredentialsOptions{TokenProvider: noopTokenProvider{}})
	var (
		gotPath  string
		gotCfg   config.CloudRunConfig
		gotCreds *auth.Credentials
	)
	deps := flatCloudRunDoctorDeps{
		openCreds: func(p string) ([]byte, error) {
			gotPath = p
			return []byte(`{"type":"service_account","client_email":"` + principalSA + `"}`), nil
		},
		buildCreds: func(ct credentials.CredType, _ []byte) (*auth.Credentials, error) {
			if ct != credentials.ServiceAccount {
				t.Fatalf("credType = %q, want service_account", ct)
			}
			return wantCreds, nil
		},
		newRuntime: func(cfg config.CloudRunConfig, creds *auth.Credentials) (scionruntime.Diagnosable, error) {
			gotCfg = cfg
			gotCreds = creds
			return stubDiag, nil
		},
	}

	got := checkDoctorFlatCloudRunInstances(instances, srvCfg, deps)
	if gotPath != secretPath {
		t.Fatalf("openCreds path = %q, want %q", gotPath, secretPath)
	}
	if gotCreds != wantCreds {
		t.Fatal("newRuntime did not receive the built instance credentials")
	}
	wantCfg := config.CloudRunConfig{
		ProjectID:                 "example-project",
		Location:                  "us-east1",
		ServiceAccount:            workSA,
		BlockServiceAccount:       blockSA,
		AssignableServiceAccounts: []string{assignSA1, assignSA2},
		Network:                   "default",
		Subnetwork:                "agents",
	}
	if !reflect.DeepEqual(gotCfg, wantCfg) {
		t.Fatalf("CloudRunConfig = %+v, want %+v", gotCfg, wantCfg)
	}
	if stubDiag.opts.Server != srvCfg {
		t.Fatal("DiagnosticOpts.Server was not forwarded to RunDiagnostics")
	}
	if len(got) != 2 {
		t.Fatalf("got %d checks (%+v), want 2", len(got), got)
	}
	if got[0].Name != "flat-cloudrun[cr-east]: Cloud Run API & Configuration" || got[0].Status != "pass" {
		t.Errorf("check[0] = %+v", got[0])
	}
	if got[1].Name != "flat-cloudrun[cr-east]: Cloud Run Instance Service Account (actAs)" || got[1].Status != "pass" {
		t.Errorf("check[1] = %+v", got[1])
	}
	for _, c := range got {
		for _, field := range []string{c.Name, c.Message, c.Remediation} {
			if strings.Contains(field, secretPath) {
				t.Errorf("check leaked credentials_file path %q in %q", secretPath, field)
			}
		}
	}
}
