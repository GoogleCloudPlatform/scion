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

package store

import (
	"regexp"
	"strings"
	"time"
)

// ProjectVisitorRule gives visitor access to a project to every user of a
// trusted issuer whose verified email is in Domain. The rule is not itself
// a grant: on a matching user's request the hub creates a project-visitor
// binding for that user whose SourceRuleID is the rule's ID, and removing
// the rule deletes those bindings.
type ProjectVisitorRule struct {
	ID        string `json:"id"`
	ProjectID string `json:"projectId"`
	// Issuer is the normalized URL of the trusted issuer
	// (NormalizeFederatedIssuer).
	Issuer string `json:"issuer"`
	// Domain is the lower-cased email domain (NormalizeVisitorRuleDomain).
	// It matches an email's domain exactly: no wildcard, no subdomain.
	Domain    string     `json:"domain"`
	NotBefore *time.Time `json:"notBefore,omitempty"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
	CreatedBy string     `json:"createdBy,omitempty"`
	CreatedAt time.Time  `json:"createdAt"`
}

// ActiveAt reports whether the rule's window contains t: NotBefore, when
// set, is not after t, and ExpiresAt, when set, is after t. It is the rule
// role bindings follow.
func (r *ProjectVisitorRule) ActiveAt(t time.Time) bool {
	if r == nil {
		return false
	}
	if r.NotBefore != nil && t.Before(*r.NotBefore) {
		return false
	}
	if r.ExpiresAt != nil && !t.Before(*r.ExpiresAt) {
		return false
	}
	return true
}

// visitorRuleDomainPattern is the shape of a visitor rule domain: one or
// more dot-separated labels of lower-case letters, digits and inner
// hyphens. It admits no wildcard, no "@", and no leading or trailing dot.
var visitorRuleDomainPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`)

// NormalizeVisitorRuleDomain returns domain trimmed and lower-cased, the
// form ProjectVisitorRule.Domain stores.
func NormalizeVisitorRuleDomain(domain string) string {
	return strings.ToLower(strings.TrimSpace(domain))
}

// ValidVisitorRuleDomain reports whether domain, already normalized, is a
// domain a visitor rule may name: a host name of at most 253 characters
// (visitorRuleDomainPattern). A wildcard, an email address or a leading dot
// is not.
func ValidVisitorRuleDomain(domain string) bool {
	return len(domain) <= 253 && visitorRuleDomainPattern.MatchString(domain)
}
