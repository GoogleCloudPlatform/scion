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

package authzop

import (
	"fmt"
	"strings"
)

// RenderMarkdown produces a reviewer-facing Markdown view of the operation
// catalog. The output is deterministic for a given input: operations are
// rendered in the order they appear in the slice.
func RenderMarkdown(specs []OperationSpec) string {
	var b strings.Builder

	b.WriteString("# Authorization Operation Catalog\n\n")
	b.WriteString("*Generated from Go-native OperationSpec definitions. Do not edit manually.*\n\n")
	fmt.Fprintf(&b, "**Operations:** %d\n\n", len(specs))

	// Table of contents.
	b.WriteString("## Table of Contents\n\n")
	for _, s := range specs {
		fmt.Fprintf(&b, "- [%s](#%s) — %s\n", s.ID, anchorID(s.ID), s.Description)
	}
	b.WriteString("\n---\n\n")

	// Per-operation sections.
	for _, s := range specs {
		renderSpec(&b, &s)
	}

	return b.String()
}

func renderSpec(b *strings.Builder, s *OperationSpec) {
	fmt.Fprintf(b, "## %s\n\n", s.ID)
	fmt.Fprintf(b, "**Domain:** %s\n\n", s.Domain)
	fmt.Fprintf(b, "**Description:** %s\n\n", s.Description)

	// Entry points.
	if len(s.EntryPoints) > 0 {
		b.WriteString("### Entry Points\n\n")
		b.WriteString("| Kind | Method | Pattern |\n")
		b.WriteString("|------|--------|---------|\n")
		for _, ep := range s.EntryPoints {
			method := ep.Method
			if method == "" {
				method = "—"
			}
			fmt.Fprintf(b, "| %s | %s | `%s` |\n", escapeTableCell(string(ep.Kind)), escapeTableCell(method), escapeTableCell(ep.Pattern))
		}
		b.WriteString("\n")
	}

	// Principals.
	if len(s.Principals) > 0 {
		b.WriteString("**Principals:** ")
		kinds := make([]string, len(s.Principals))
		for i, p := range s.Principals {
			kinds[i] = "`" + string(p) + "`"
		}
		b.WriteString(strings.Join(kinds, ", "))
		b.WriteString("\n\n")
	}

	// Credentials.
	if len(s.Credentials) > 0 {
		b.WriteString("**Credentials:** ")
		creds := make([]string, len(s.Credentials))
		for i, c := range s.Credentials {
			creds[i] = "`" + string(c) + "`"
		}
		b.WriteString(strings.Join(creds, ", "))
		b.WriteString("\n\n")
	}

	// Base permission and resolver.
	if s.BasePermission != "" {
		fmt.Fprintf(b, "**Base Permission:** `%s`\n\n", s.BasePermission)
	}
	if s.ResourceResolver != "" {
		fmt.Fprintf(b, "**Resource Resolver:** %s\n\n", s.ResourceResolver)
	}

	// Effects.
	if len(s.Effects) > 0 {
		b.WriteString("**Effects:** ")
		effs := make([]string, len(s.Effects))
		for i, e := range s.Effects {
			effs[i] = "`" + string(e) + "`"
		}
		b.WriteString(strings.Join(effs, ", "))
		b.WriteString("\n\n")
	}

	// Delegation.
	if s.DelegationKind != DelegationNone {
		b.WriteString("### Delegation\n\n")
		fmt.Fprintf(b, "- **Kind:** `%s`\n", s.DelegationKind)
		if s.DelegationDescription != "" {
			fmt.Fprintf(b, "- %s\n", s.DelegationDescription)
		}
		b.WriteString("\n")
	}

	// Authority evaluation.
	if s.AuthorityEval != AuthorityEvalNone {
		fmt.Fprintf(b, "**Authority Evaluation:** `%s`\n\n", s.AuthorityEval)
	}

	// Governance.
	if s.Governance != nil {
		b.WriteString("### Governance\n\n")
		fmt.Fprintf(b, "- **Kind:** %s\n", s.Governance.Kind)
		if s.Governance.Description != "" {
			fmt.Fprintf(b, "- %s\n", s.Governance.Description)
		}
		if s.Governance.DomainCallback != "" {
			fmt.Fprintf(b, "- **Callback:** `%s`\n", s.Governance.DomainCallback)
		}
		b.WriteString("\n")
	}

	// Invariants.
	if len(s.Invariants) > 0 {
		b.WriteString("### Invariants\n\n")
		b.WriteString("| ID | Kind | Description | Fail-Closed |\n")
		b.WriteString("|----|------|-------------|-------------|\n")
		for _, inv := range s.Invariants {
			fc := "No"
			if inv.FailClosed {
				fc = "Yes"
			}
			fmt.Fprintf(b, "| %s | %s | %s | %s |\n",
				escapeTableCell(inv.ID), escapeTableCell(string(inv.Kind)),
				escapeTableCell(inv.Description), fc)
		}
		b.WriteString("\n")
	}

	// Audit.
	if s.AuditObligation != nil {
		b.WriteString("### Audit\n\n")
		fmt.Fprintf(b, "- **Event Type:** `%s`\n", s.AuditObligation.EventType)
		if len(s.AuditObligation.ContextFields) > 0 {
			fmt.Fprintf(b, "- **Context Fields:** %s\n", strings.Join(s.AuditObligation.ContextFields, ", "))
		}
		if len(s.AuditObligation.BeforeFields) > 0 {
			fmt.Fprintf(b, "- **Before Fields:** %s\n", strings.Join(s.AuditObligation.BeforeFields, ", "))
		}
		if len(s.AuditObligation.AfterFields) > 0 {
			fmt.Fprintf(b, "- **After Fields:** %s\n", strings.Join(s.AuditObligation.AfterFields, ", "))
		}
		if s.AuditObligation.Atomic {
			b.WriteString("- **Atomic:** Yes\n")
		} else {
			b.WriteString("- **Atomic:** No\n")
			if s.AuditObligation.NonAtomicJustification != "" {
				fmt.Fprintf(b, "- **Non-Atomic Justification:** %s\n", s.AuditObligation.NonAtomicJustification)
			}
		}
		b.WriteString("\n")
	}

	// External effect policy.
	if s.ExternalPolicy != nil {
		b.WriteString("### External Effect Policy\n\n")
		fmt.Fprintf(b, "- **Delivery:** `%s`\n", s.ExternalPolicy.DeliveryMode)
		fmt.Fprintf(b, "- **Failure Mode:** `%s`\n", s.ExternalPolicy.FailureMode)
		fmt.Fprintf(b, "- **Idempotency:** %s\n", s.ExternalPolicy.IdempotencyKey)
		fmt.Fprintf(b, "- **Retry:** %s\n", s.ExternalPolicy.RetryPolicy)
		if s.ExternalPolicy.Compensation != "" {
			fmt.Fprintf(b, "- **Compensation:** %s\n", s.ExternalPolicy.Compensation)
		}
		if s.ExternalPolicy.AuthBeforeEmit {
			b.WriteString("- **Auth Before Emit:** Yes\n")
		}
		b.WriteString("\n")
	}

	// Denial codes.
	if len(s.DenialCodes) > 0 {
		b.WriteString("**Denial Codes:** ")
		codes := make([]string, len(s.DenialCodes))
		for i, dc := range s.DenialCodes {
			codes[i] = "`" + string(dc) + "`"
		}
		b.WriteString(strings.Join(codes, ", "))
		b.WriteString("\n\n")
	}

	// Test refs.
	if len(s.TestRefs) > 0 {
		b.WriteString("### Tests\n\n")
		for _, tr := range s.TestRefs {
			fmt.Fprintf(b, "- `%s:%s`\n", tr.Package, tr.Function)
		}
		b.WriteString("\n")
	}

	// Exemptions.
	if len(s.Exemptions) > 0 {
		b.WriteString("### Exemptions\n\n")
		for _, ex := range s.Exemptions {
			fmt.Fprintf(b, "- **%s:** %s", ex.Kind, ex.Reason)
			if ex.Scope != "" {
				fmt.Fprintf(b, " (scope: %s)", ex.Scope)
			}
			if len(ex.Waives) > 0 {
				waives := make([]string, len(ex.Waives))
				for i, w := range ex.Waives {
					waives[i] = "`" + string(w) + "`"
				}
				fmt.Fprintf(b, " — waives: %s", strings.Join(waives, ", "))
			}
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	b.WriteString("---\n\n")
}

// anchorID converts an operation ID to a GitHub-compatible Markdown anchor.
// Operation IDs are validated as lowercase-only, so no case conversion needed.
func anchorID(id OperationID) string {
	return strings.ReplaceAll(string(id), ".", "")
}

// escapeTableCell escapes pipe characters in a Markdown table cell value.
func escapeTableCell(s string) string {
	return strings.ReplaceAll(s, "|", "\\|")
}
