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
	"strings"

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

// saAssignCheckDiagnostic marks that the assignment check cannot run on
// this process. Its presence is the whole record: the registry tick writes
// it to this instance's row as check saAssignCheckName.
type saAssignCheckDiagnostic struct{}

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
		s.saAssignCheckDiag.Store(&saAssignCheckDiagnostic{})
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

// saAssignIAMReadRole is the role the hub's identity needs in a service
// account's GCP project to run the assignment check for that account.
const saAssignIAMReadRole = "roles/iam.securityReviewer"

// hubIdentityRefusedIAMRead reports whether err, from a failed assignment
// check, is a plain permission-denied refusal of the hub's own identity.
// Only that case gets the detailed end-user text: a disabled API, a VPC
// Service Controls refusal, any other ErrorInfo reason, any precondition
// detail and every other error code return false and keep the generic
// text. err is only classified; none of its text is shown to the caller.
func hubIdentityRefusedIAMRead(err error) bool {
	if err == nil || status.Code(err) != codes.PermissionDenied {
		return false
	}
	st, ok := status.FromError(err)
	if !ok {
		return false
	}
	if strings.Contains(st.Message(), "vpcServiceControlsUniqueIdentifier") {
		return false
	}
	for _, d := range st.Details() {
		switch v := d.(type) {
		case *errdetails.ErrorInfo:
			if v.GetReason() != "IAM_PERMISSION_DENIED" {
				return false
			}
		case *errdetails.PreconditionFailure:
			return false
		}
	}
	return true
}

// saAssignIAMReadDeniedMessage is the end-user text when the assignment
// check failed because the hub's identity may not read IAM policy in the
// service account's GCP project. Its only inputs are that project ID and
// the hub's service account email; it returns "" when either is empty or
// holds characters a GCP project ID or account email does not, so the
// caller falls back to the generic text. It never names the target
// account, so it reads the same whether or not that account exists.
func saAssignIAMReadDeniedMessage(gcpProjectID, hubSAEmail string) string {
	if !saAssignSafeIdent(gcpProjectID, false) || !saAssignSafeIdent(hubSAEmail, true) {
		return ""
	}
	return "Could not verify your permission to use this GCP service account: " +
		"the Hub's service account (" + hubSAEmail + ") cannot read IAM policy in " +
		"the account's GCP project " + gcpProjectID + ". It needs the " +
		saAssignIAMReadRole + " role on that project. A GCP project admin can " +
		"grant it with: gcloud projects add-iam-policy-binding " + gcpProjectID +
		" --member=serviceAccount:" + hubSAEmail + " --role=" + saAssignIAMReadRole
}

// saAssignSafeIdent reports whether s is non-empty and uses only lowercase
// letters, digits, '-', '.', ':' (domain-scoped project IDs) and, when
// email is set, '@' and '_'. It keeps stored values that could not be a
// real project ID or account email out of the suggested command.
func saAssignSafeIdent(s string, email bool) bool {
	if s == "" || len(s) > 254 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '.', c == ':':
		case email && (c == '@' || c == '_'):
		default:
			return false
		}
	}
	return true
}
