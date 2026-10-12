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
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/GoogleCloudPlatform/scion/pkg/hubprofile"
)

// errHubProfileGateFailed is the exit error of 'scion hub status' when a
// profile is selected and a gate check fails.
var errHubProfileGateFailed = errors.New("hub status: the hub profile checks failed (see above)")

// runHubProfileStatus prints the profile part of 'scion hub status' and
// exits non-zero when the request sent to the target did not carry the
// profile's bearer, or its transport principal is not the profile's IAP
// service account. Tokens are decoded locally and never printed.
func runHubProfileStatus(cmd *cobra.Command) error {
	s, err := openHubProfileSession(hubProfileAs != "")
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()
	r := s.Status(ctx)
	out := cmd.OutOrStdout()
	if isJSONOutput() {
		if err := writeIndentedJSON(out, r); err != nil {
			return err
		}
		if !r.OK {
			return &jsonReportedError{msg: errHubProfileGateFailed.Error()}
		}
		return nil
	}
	printHubProfileStatus(out, r)
	if !r.OK {
		return errHubProfileGateFailed
	}
	return nil
}

func printHubProfileStatus(out io.Writer, r *hubprofile.StatusReport) {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(out, format, a...) }
	p("Hub Profile Status\n")
	p("==================\n")
	p("Profile:         %s\n", r.Profile)
	p("Endpoint:        %s\n", r.Endpoint)
	if r.Run != "" {
		p("Run:             %s\n", r.Run)
	}
	p("Bearer source:   %s\n", r.BearerSource)
	id := r.Identity
	p("Identity:        %s\n", valueOrNone(id.Email))
	p("  Kind:          %s\n", valueOrNone(id.Kind))
	p("  Tier:          %s\n", valueOrNone(id.Tier))
	if id.IdentityID != "" {
		p("  ID:            %s\n", id.IdentityID)
	}
	if id.DelegationID != "" {
		p("  Delegation:    %s\n", id.DelegationID)
	}
	if id.ExpiresAt != nil {
		p("  Expires:       %s\n", id.ExpiresAt.UTC().Format(time.RFC3339))
	}
	if id.TokenExpiresAt != nil {
		p("  Token expires: %s\n", id.TokenExpiresAt.UTC().Format(time.RFC3339))
	}
	p("Transport:       %s\n", r.Transport.Mode)
	if r.Transport.PrincipalEmail != "" || r.Transport.Audience != "" {
		p("  Principal:     %s\n", valueOrNone(r.Transport.PrincipalEmail))
		p("  Audience:      %s\n", valueOrNone(r.Transport.Audience))
	}
	if r.IssuerAudience != "" {
		p("Issuer audience: %s\n", r.IssuerAudience)
	}
	p("Checks:\n")
	for _, c := range r.Checks {
		mark := "ok  "
		if !c.OK {
			mark = "FAIL"
		}
		p("  [%s] %s: %s\n", mark, c.Name, strings.TrimSpace(c.Detail))
	}
	if r.OK {
		p("Result:          ok\n")
	} else {
		p("Result:          FAILED\n")
	}
}

// bearerSourceLabel names where the CLI's bearer comes from without a
// profile: OAuth, agent-token (the scion-token file), env
// (SCION_AUTH_TOKEN or SCION_HUB_TOKEN), dev, transport or none.
func bearerSourceLabel(info authInfo) string {
	switch info.MethodType {
	case "oauth":
		return "OAuth"
	case "agent_token":
		if strings.HasSuffix(info.Source, " env") {
			return "env"
		}
		return "agent-token"
	case "bearer":
		return "env"
	case "devauth":
		return "dev"
	case "proxy-auth":
		return "transport"
	default:
		return "none"
	}
}
