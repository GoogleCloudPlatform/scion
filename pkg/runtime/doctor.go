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

package runtime

// CheckResult represents the outcome of a single diagnostic check.
type CheckResult struct {
	Name        string `json:"name"`
	Status      string `json:"status"` // "pass", "warn", "fail", "skip", "info"
	Message     string `json:"message"`
	Remediation string `json:"remediation,omitempty"`
}

// DiagnosticReport contains the results of all diagnostic checks for a runtime.
type DiagnosticReport struct {
	Runtime string        `json:"runtime"`
	Checks  []CheckResult `json:"checks"`
}

// Diagnosable is implemented by runtimes that support diagnostic checks.
type Diagnosable interface {
	RunDiagnostics(opts DiagnosticOpts) DiagnosticReport
}

// DiagnosticOpts configures which diagnostic checks to run.
type DiagnosticOpts struct {
	Namespace string // namespace to validate access for
	GKEMode   bool   // whether GKE-specific checks should run
}
