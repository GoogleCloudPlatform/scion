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

import "strings"

// ---------------------------------------------------------------------------
// Service-account project parsing.
//
// googleSAProject parses the GCP project ID out of a Google service-account
// email, for the external-bearer path's allowed_gcp_projects check:
// a service-account ID token is admitted only when its project is explicitly
// listed. This is deliberately narrower than isGoogleServiceAccount
// (google_credential_validator.go), which classifies IsServiceAccount for
// every *.gserviceaccount.com shape: a compute default SA or a Google-owned
// service agent IS a service account, but neither carries a project ID this
// function is willing to hand to the allowlist check, so both are rejected
// here explicitly rather than silently matching no configured project.
// ---------------------------------------------------------------------------

// googleSAProject returns the GCP project ID encoded in a Google
// service-account email, and whether one could be parsed. The input is
// lower-cased before matching, so callers need not normalize case first.
//
//	name@PROJECT.iam.gserviceaccount.com        -> PROJECT, true
//	PROJECT@appspot.gserviceaccount.com         -> PROJECT, true
//	service-N@gcp-sa-*.iam.gserviceaccount.com  -> "", false (Google-owned service agent)
//	N-compute@developer.gserviceaccount.com     -> "", false (project number only)
//	*@system.gserviceaccount.com                -> "", false
//	anything else                               -> "", false
//
// There is no wildcard support: the returned project ID is matched against
// allowed_gcp_projects by exact, case-insensitive string comparison.
func googleSAProject(email string) (projectID string, ok bool) {
	email = strings.ToLower(email)

	at := strings.LastIndex(email, "@")
	if at < 0 || at == len(email)-1 {
		return "", false
	}
	local, domain := email[:at], email[at+1:]

	switch {
	case strings.HasSuffix(domain, ".iam.gserviceaccount.com"):
		project := strings.TrimSuffix(domain, ".iam.gserviceaccount.com")
		if project == "" {
			// Bare "iam.gserviceaccount.com" with no project label.
			return "", false
		}
		// Google service agents (e.g. service-123@gcp-sa-pubsub.iam.gserviceaccount.com)
		// are Google-owned, not operator-owned projects — reject explicitly
		// rather than letting one match an operator's allowed_gcp_projects entry
		// by coincidence.
		if strings.HasPrefix(project, "gcp-sa-") {
			return "", false
		}
		return project, true

	case domain == "appspot.gserviceaccount.com":
		if local == "" {
			return "", false
		}
		return local, true

	case domain == "developer.gserviceaccount.com":
		// N-compute@developer.gserviceaccount.com carries only the project
		// NUMBER, not the project ID allowed_gcp_projects is configured with.
		// Compute default SAs are rejected rather than supported here.
		return "", false

	case domain == "system.gserviceaccount.com":
		return "", false

	default:
		return "", false
	}
}
