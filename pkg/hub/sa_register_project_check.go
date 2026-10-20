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
	"context"
	"log/slog"
	"time"

	policytroubleshooterpb "cloud.google.com/go/policytroubleshooter/iam/apiv3/iampb"
	"google.golang.org/grpc/status"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// SAProjectAccessProbe reports whether the hub can run the service account
// assignment check in a GCP project: the check needs the hub's identity to
// read IAM policy there. The returned error is only classified (see
// hubIdentityRefusedIAMRead); none of its text reaches a caller.
type SAProjectAccessProbe interface {
	ProbeProjectAccess(ctx context.Context, gcpProjectID, hubSAEmail string) error
}

// ptProjectAccessProbe is the SAProjectAccessProbe backed by the same Policy
// Troubleshooter API the assignment check calls. It asks about the GCP
// project resource itself, not about a service account in it, so the answer
// does not depend on whether any account exists there. Only the call's error
// is used; the access answer in the response is ignored.
type ptProjectAccessProbe struct {
	client PTClient
}

// NewPTProjectAccessProbe returns an SAProjectAccessProbe that calls client.
func NewPTProjectAccessProbe(client PTClient) SAProjectAccessProbe {
	return &ptProjectAccessProbe{client: client}
}

// ProbeProjectAccess implements SAProjectAccessProbe.
func (p *ptProjectAccessProbe) ProbeProjectAccess(ctx context.Context, gcpProjectID, hubSAEmail string) error {
	_, err := p.client.TroubleshootIamPolicy(ctx, &policytroubleshooterpb.TroubleshootIamPolicyRequest{
		AccessTuple: &policytroubleshooterpb.AccessTuple{
			Principal:        hubSAEmail,
			FullResourceName: "//cloudresourcemanager.googleapis.com/projects/" + gcpProjectID,
			Permission:       store.PermissionActAs,
		},
	})
	return err
}

// saProjectProbeTimeout bounds the registration-time probe so a slow API
// cannot hold up a registration or verify response.
const saProjectProbeTimeout = 10 * time.Second

// SetSAProjectAccessProbe installs the probe run when a service account is
// registered or verified, and the hub's own GCP project, whose accounts are
// never probed. A nil probe or an empty project turns the probe off.
func (s *Server) SetSAProjectAccessProbe(probe SAProjectAccessProbe, hubGCPProjectID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saProjectProbe = probe
	s.saProjectProbeHubProject = hubGCPProjectID
}

// saProjectAccessWarning returns the advisory warning for a registered or
// verified service account whose GCP project differs from the hub's when
// the hub's identity is refused reading IAM policy there, or "" otherwise.
// The text is saAssignIAMReadDeniedMessage, the one the assignment check
// shows when it fails for the same reason. Only a plain permission-denied
// refusal produces it; every other outcome, including success, adds
// nothing. Accounts in the hub's own project, and every account while the
// assignment check is not enforced, are not probed at all. It only reports:
// registration, verification and the stored account are unaffected.
func (s *Server) saProjectAccessWarning(ctx context.Context, sa *store.GCPServiceAccount) string {
	s.mu.RLock()
	probe := s.saProjectProbe
	hubProject := s.saProjectProbeHubProject
	mode := s.saAssignCheckMode
	s.mu.RUnlock()
	if probe == nil || hubProject == "" || mode != SAAssignCheckEnforce || sa.ProjectID == hubProject {
		return ""
	}
	hubEmail := s.hubSAEmail()
	msg := saAssignIAMReadDeniedMessage(sa.ProjectID, hubEmail)
	if msg == "" {
		// Nothing could be shown, so make no call.
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, saProjectProbeTimeout)
	defer cancel()
	err := probe.ProbeProjectAccess(ctx, sa.ProjectID, hubEmail)
	if !hubIdentityRefusedIAMRead(err) {
		if err != nil {
			slog.Debug("service account project access probe did not complete",
				"sa_id", sa.ID, "code", status.Code(err).String())
		}
		return ""
	}
	return msg
}

// withSAProjectAccessWarning appends saProjectAccessWarning for sa to
// warnings, when there is one.
func (s *Server) withSAProjectAccessWarning(ctx context.Context, warnings []string, sa *store.GCPServiceAccount) []string {
	if w := s.saProjectAccessWarning(ctx, sa); w != "" {
		warnings = append(warnings, w)
	}
	return warnings
}
