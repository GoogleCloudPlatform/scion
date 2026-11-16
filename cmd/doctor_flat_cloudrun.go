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
	"errors"
	"fmt"
	"slices"

	"cloud.google.com/go/auth"
	"cloud.google.com/go/auth/credentials"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	scionruntime "github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

type flatCloudRunDoctorDeps struct {
	openCreds  func(path string) ([]byte, error)
	buildCreds func(credType credentials.CredType, data []byte) (*auth.Credentials, error)
	newRuntime func(cfg config.CloudRunConfig, creds *auth.Credentials) (scionruntime.Diagnosable, error)
}

func defaultFlatCloudRunDoctorDeps() flatCloudRunDoctorDeps {
	return flatCloudRunDoctorDeps{
		openCreds:  openFlatCredentialsFile,
		buildCreds: flatCloudRunCredentialsFromJSON,
		newRuntime: func(cfg config.CloudRunConfig, creds *auth.Credentials) (scionruntime.Diagnosable, error) {
			return scionruntime.NewFlatCloudRunRuntime(cfg, creds)
		},
	}
}

func hasDoctorFlatCloudRunTarget(srv *config.V1ServerConfig) bool {
	if srv == nil || srv.Broker == nil {
		return false
	}
	for _, inst := range srv.Broker.Instances {
		if inst.RuntimeTarget != nil && inst.RuntimeTarget.Type == config.RuntimeTargetTypeCloudRun {
			return true
		}
	}
	return false
}

// checkDoctorFlatCloudRunInstancesHere runs Cloud Run diagnostics (API
// reachability and iam.serviceAccounts.actAs across every configured agent
// service account) for each flat cloudrun instance in this host's global
// settings using that instance's runtime_target.credentials_file. Nothing is
// reported on hosts without flat cloudrun instances, and neither credential
// contents nor credentials_file paths appear in any check output.
func checkDoctorFlatCloudRunInstancesHere(serverCfg *config.V1ServerConfig) []scionruntime.CheckResult {
	instances, err := config.LoadRuntimeBrokerInstances("")
	if err != nil {
		if hasDoctorFlatCloudRunTarget(serverCfg) {
			return []scionruntime.CheckResult{{
				Name:        "flat-cloudrun",
				Status:      "warn",
				Message:     "Could not load server.broker.instances to check flat Cloud Run instances",
				Remediation: "Fix server.broker.instances (scion config validate), then run scion doctor again.",
			}}
		}
		return nil
	}
	return checkDoctorFlatCloudRunInstances(instances, serverCfg, defaultFlatCloudRunDoctorDeps())
}

func checkDoctorFlatCloudRunInstances(
	instances []config.V1RuntimeBrokerInstanceConfig,
	serverCfg *config.V1ServerConfig,
	deps flatCloudRunDoctorDeps,
) []scionruntime.CheckResult {
	var out []scionruntime.CheckResult
	for _, inst := range instances {
		t := inst.RuntimeTarget
		if t == nil || t.Type != config.RuntimeTargetTypeCloudRun {
			continue
		}

		openCreds := deps.openCreds
		if openCreds == nil {
			openCreds = openFlatCredentialsFile
		}
		data, err := openCreds(t.CredentialsFile)
		if errors.Is(err, errors.ErrUnsupported) {
			out = append(out, scionruntime.CheckResult{
				Name:    fmt.Sprintf("flat-cloudrun[%s]", inst.Key),
				Status:  "skip",
				Message: fmt.Sprintf("Flat Cloud Run instance %q credentials_file check is only supported on Unix hosts", inst.Key),
			})
			continue
		}
		if err != nil {
			out = append(out, scionruntime.CheckResult{
				Name:        fmt.Sprintf("flat-cloudrun[%s]: credentials", inst.Key),
				Status:      "fail",
				Message:     fmt.Sprintf("Flat Cloud Run instance %q credentials check failed: %v", inst.Key, err),
				Remediation: "Ensure runtime_target.credentials_file is a regular non-symlink file owned by the service user with mode 0600 or 0400.",
			})
			continue
		}

		info, err := parseFlatCloudRunCredentials(data)
		if err == nil {
			err = flatCheckPrincipalNotAgentSA(info, t)
		}
		if err != nil {
			out = append(out, scionruntime.CheckResult{
				Name:        fmt.Sprintf("flat-cloudrun[%s]: credentials", inst.Key),
				Status:      "fail",
				Message:     fmt.Sprintf("Flat Cloud Run instance %q credentials check failed: %v", inst.Key, err),
				Remediation: "Configure runtime_target.credentials_file with a dedicated service_account or impersonated_service_account credential distinct from every configured agent service account.",
			})
			continue
		}

		buildCreds := deps.buildCreds
		if buildCreds == nil {
			buildCreds = flatCloudRunCredentialsFromJSON
		}
		creds, err := buildCreds(info.credType, data)
		if err != nil {
			out = append(out, scionruntime.CheckResult{
				Name:        fmt.Sprintf("flat-cloudrun[%s]: credentials", inst.Key),
				Status:      "fail",
				Message:     fmt.Sprintf("Flat Cloud Run instance %q credentials check failed: credentials_file could not be loaded as its credential type", inst.Key),
				Remediation: "Configure runtime_target.credentials_file with a valid service_account or impersonated_service_account JSON credential.",
			})
			continue
		}

		newRT := deps.newRuntime
		if newRT == nil {
			newRT = func(cfg config.CloudRunConfig, c *auth.Credentials) (scionruntime.Diagnosable, error) {
				return scionruntime.NewFlatCloudRunRuntime(cfg, c)
			}
		}
		diag, err := newRT(config.CloudRunConfig{
			ProjectID:                 t.ProjectID,
			Location:                  t.Region,
			ServiceAccount:            t.ServiceAccount,
			BlockServiceAccount:       t.BlockServiceAccount,
			AssignableServiceAccounts: slices.Clone(t.AssignableServiceAccounts),
			Network:                   t.Network,
			Subnetwork:                t.Subnetwork,
		}, creds)
		if err != nil {
			out = append(out, scionruntime.CheckResult{
				Name:    fmt.Sprintf("flat-cloudrun[%s]", inst.Key),
				Status:  "fail",
				Message: fmt.Sprintf("Flat Cloud Run instance %q runtime initialization failed: %v", inst.Key, err),
			})
			continue
		}

		report := diag.RunDiagnostics(scionruntime.DiagnosticOpts{Server: serverCfg})
		for _, c := range report.Checks {
			c.Name = fmt.Sprintf("flat-cloudrun[%s]: %s", inst.Key, c.Name)
			out = append(out, c)
		}
	}
	return out
}
