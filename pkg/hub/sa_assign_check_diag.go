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
	"sort"
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

// saAssignCheckDiagnostic is the admin-only record that the assignment
// check cannot run on this process. since is when it was first recorded;
// a repeat keeps it.
type saAssignCheckDiagnostic struct {
	since time.Time
}

// saAssignCheckName is the check this process writes to its hub-instance
// registry row while the assignment check cannot run (value degraded), so
// every replica's health summary sees it through that row.
const saAssignCheckName = "sa_assign_check"

// HealthSummarySACheck is the admin health summary section reporting that
// the service account assignment check cannot run on at least one live hub
// instance because of the hub's own access. It is built from the
// instances' registry rows (check saAssignCheckName), so every replica
// returns the same section. Present only while that is the case.
type HealthSummarySACheck struct {
	Status  string `json:"status"`
	Cause   string `json:"cause"`
	Remedy  string `json:"remedy"`
	DocsURL string `json:"docs_url"`
	// Instances lists the labels of the live instances that report it,
	// sorted.
	Instances []string `json:"instances"`
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
			next := &saAssignCheckDiagnostic{since: now}
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

// saAssignCheckCannotRun reports whether this process has recorded that
// the assignment check cannot run while it is enforced. The registry tick
// writes it to this instance's row as check saAssignCheckName.
func (s *Server) saAssignCheckCannotRun() bool {
	s.mu.RLock()
	mode := s.saAssignCheckMode
	s.mu.RUnlock()
	return mode == SAAssignCheckEnforce && s.saAssignCheckDiag.Load() != nil
}

// healthSummarySACheck builds the summary's service account check section
// from the live instances of the hub-instance registry: present when at
// least one live instance reports check saAssignCheckName as not healthy,
// nil otherwise (or when the registry could not be read).
func healthSummarySACheck(list *HealthSummaryHubInstances) *HealthSummarySACheck {
	if list == nil {
		return nil
	}
	var labels []string
	for _, it := range list.Items {
		if it.State != HubInstanceStateLive {
			continue
		}
		if v, ok := it.Checks[saAssignCheckName]; ok && v != HealthStatusHealthy {
			labels = append(labels, hubInstanceDisplayLabel(it.Label, it.ID))
		}
	}
	if len(labels) == 0 {
		return nil
	}
	sort.Strings(labels)
	return &HealthSummarySACheck{
		Status:    HealthStatusDegraded,
		Cause:     saAssignCheckDiagCause,
		Remedy:    saAssignCheckDiagRemedy,
		DocsURL:   saAssignCheckDiagDocsURL,
		Instances: labels,
	}
}
