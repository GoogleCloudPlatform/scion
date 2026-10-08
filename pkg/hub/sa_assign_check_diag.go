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
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// saAssignCheckDiagCause is the cause recorded when the service account
// assignment check could not run because the hub's own identity was refused
// by the API the check calls.
const saAssignCheckDiagCause = "hub_identity_missing_access"

// saAssignCheckDiagRemedy is the admin-facing remedy. It names no specific
// role or permission; the docs page carries the details.
const saAssignCheckDiagRemedy = "The service account assignment check cannot run because " +
	"the hub's identity does not have the access it needs for the check. " +
	"Grant the hub's identity that access; service account assignment is " +
	"denied until the check can run."

// saAssignCheckDiagDocsURL links the docs section describing the access the
// hub's identity needs for the assignment check.
const saAssignCheckDiagDocsURL = "https://googlecloudplatform.github.io/scion/" +
	"hosted/ha/permissions/#hub-identity-access-for-the-assignment-check"

// saAssignCheckDiagnostic is the admin-only record of why the assignment
// check cannot run.
type saAssignCheckDiagnostic struct {
	since time.Time
	last  time.Time
}

// HealthSummarySACheck is the admin health summary section reporting that
// the service account assignment check cannot run because of the hub's own
// access. Present only while that is the case.
type HealthSummarySACheck struct {
	Status   string    `json:"status"`
	Cause    string    `json:"cause"`
	Remedy   string    `json:"remedy"`
	DocsURL  string    `json:"docs_url"`
	Since    time.Time `json:"since"`
	LastSeen time.Time `json:"last_seen"`
}

// NoteSAAssignCheckCall updates the admin diagnostic from one Policy
// Troubleshooter API call the checker actually made; register it with
// PolicyTroubleshooterChecker.SetCallObserver below any result cache, so
// cached answers neither record nor clear. A permission-denied error records
// the diagnostic, unless it says the API is disabled for the project, where
// granting the hub's identity access would not help. A successful call clears
// it. Any other error leaves it as it was. Nothing is recorded while the
// check is not enforced. This only reports; the assignment decision is made
// elsewhere and is unaffected.
func (s *Server) NoteSAAssignCheckCall(err error) {
	switch {
	case err == nil:
		s.saAssignCheckDiag.Store(nil)
	case status.Code(err) == codes.PermissionDenied && !apiServiceDisabled(err):
		// Hold the read lock so a mode change, which clears the diagnostic
		// under the write lock, cannot interleave with this record.
		s.mu.RLock()
		defer s.mu.RUnlock()
		if s.saAssignCheckMode != SAAssignCheckEnforce {
			return
		}
		now := time.Now().UTC()
		for {
			prev := s.saAssignCheckDiag.Load()
			next := &saAssignCheckDiagnostic{since: now, last: now}
			if prev != nil {
				next.since = prev.since
			}
			if s.saAssignCheckDiag.CompareAndSwap(prev, next) {
				return
			}
		}
	}
}

// apiServiceDisabled reports whether err says the called API is disabled for
// the project, which Google APIs signal with an ErrorInfo detail whose
// reason is SERVICE_DISABLED.
func apiServiceDisabled(err error) bool {
	st, ok := status.FromError(err)
	if !ok {
		return false
	}
	for _, d := range st.Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok && info.GetReason() == "SERVICE_DISABLED" {
			return true
		}
	}
	return false
}

// healthSummarySACheck returns the diagnostic section for the admin health
// summary, or nil when the check is not enforced or has no recorded cause.
func (s *Server) healthSummarySACheck() *HealthSummarySACheck {
	s.mu.RLock()
	mode := s.saAssignCheckMode
	s.mu.RUnlock()
	if mode != SAAssignCheckEnforce {
		return nil
	}
	d := s.saAssignCheckDiag.Load()
	if d == nil {
		return nil
	}
	return &HealthSummarySACheck{
		Status:   HealthStatusDegraded,
		Cause:    saAssignCheckDiagCause,
		Remedy:   saAssignCheckDiagRemedy,
		DocsURL:  saAssignCheckDiagDocsURL,
		Since:    d.since,
		LastSeen: d.last,
	}
}
