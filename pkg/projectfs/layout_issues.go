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

package projectfs

import (
	"fmt"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// IssueView selects how much of a LayoutIssue a reader may see.
type IssueView int

const (
	// ViewPublic names the differing or invalid fields only, never their
	// values. It is the view of every response a non-admin or
	// unauthenticated caller can read, and the default.
	ViewPublic IssueView = iota
	// ViewAdmin adds the values. Use it only in a response the handler has
	// authorized for a hub admin.
	ViewAdmin
	// ViewLog is the full form for process logs.
	ViewLog
)

// LayoutIssuesRemediation is the fixed remediation that accompanies the
// public view of storage issues.
const LayoutIssuesRemediation = "ask a hub admin; the hub log has the details"

// LayoutIssue is one way a storage layout is invalid or differs between the
// hub and a broker: the field, a rule or reason that carries no value, and
// the values involved. Build issues with the projectfs constructors
// (Layout.DescriptorIssues, NewFieldIssue, NewMismatchIssue); render them
// only with RenderLayoutIssues or LayoutIssueDetails.
type LayoutIssue struct {
	// Field names the setting or descriptor field, for example
	// "nfs server" or "server.project_storage.nfs.shares[0].id".
	Field string
	// Rule is the value-free reason, for example "differs" or
	// "broker reports no usable share".
	Rule string
	// HubValue and BrokerValue are the values compared (a mismatch), or
	// HubValue alone the invalid value (an invalid field). Either may be
	// empty.
	HubValue    string
	BrokerValue string

	invalid bool
	// detail is the full line with values (ViewAdmin, ViewLog).
	detail string
}

// NewFieldIssue is an invalid field: field, the rule it breaks, and the
// configured value (rendered only for admins and logs).
func NewFieldIssue(field, rule, value string) LayoutIssue {
	detail := rule
	if field != "" && !strings.HasPrefix(rule, field) {
		detail = field + ": " + rule
	}
	if value != "" {
		detail += fmt.Sprintf(" (got %q)", value)
	}
	return LayoutIssue{Field: field, Rule: rule, HubValue: value, invalid: true, detail: detail}
}

// NewMismatchIssue is a field whose value differs between two sides,
// labelled a and b in the admin and log views (for example the source and
// target broker of a move).
func NewMismatchIssue(field, a, aValue, b, bValue string) LayoutIssue {
	return LayoutIssue{Field: field, Rule: "differs", HubValue: aValue, BrokerValue: bValue,
		detail: fmt.Sprintf("%s: %s %q, %s %q", field, a, aValue, b, bValue)}
}

// mismatch is a hub/broker mismatch whose admin line is
// "<field>: hub <hub>, broker <broker>" with the values already formatted.
func mismatch(field, hub, broker, hubValue, brokerValue string) LayoutIssue {
	return LayoutIssue{Field: field, Rule: "differs", HubValue: hubValue, BrokerValue: brokerValue,
		detail: fmt.Sprintf("%s: hub %s, broker %s", field, hub, broker)}
}

// reasonIssue is an issue with a fixed reason and no values.
func reasonIssue(field, reason, line string) LayoutIssue {
	return LayoutIssue{Field: field, Rule: reason, detail: line}
}

// RenderLayoutIssues renders issues for view, one line each. ViewPublic
// gives "<field>: invalid" for an invalid field and "<field>: <rule>"
// otherwise (rules carry no values); ViewAdmin and ViewLog give the full
// line with values. It is the only place layout values become text.
func RenderLayoutIssues(items []LayoutIssue, view IssueView) []string {
	if len(items) == 0 {
		return nil
	}
	out := make([]string, len(items))
	for i, it := range items {
		switch {
		case view == ViewAdmin || view == ViewLog:
			if it.detail != "" {
				out[i] = it.detail
			} else {
				out[i] = it.Field + ": " + it.Rule
			}
		case it.invalid:
			out[i] = it.Field + ": invalid"
		default:
			out[i] = it.Field + ": " + it.Rule
		}
	}
	return out
}

// LayoutIssueFields returns the distinct field names of issues, in order.
func LayoutIssueFields(items []LayoutIssue) []string {
	var out []string
	seen := map[string]bool{}
	for _, it := range items {
		if !seen[it.Field] {
			seen[it.Field] = true
			out = append(out, it.Field)
		}
	}
	return out
}

// LayoutIssueSummary is the one-line form of issues for view, as a refusal
// message embeds it: the rendered lines joined with "; ", and for
// ViewPublic the fixed remediation after them.
func LayoutIssueSummary(items []LayoutIssue, view IssueView) string {
	s := strings.Join(RenderLayoutIssues(items, view), "; ")
	if view == ViewPublic && s != "" {
		s += "; " + LayoutIssuesRemediation
	}
	return s
}

// LayoutIssueDetails is the structured form of issues for an error
// response's details: "fields" (the field names), "issues" (the rendered
// lines for view) and, for ViewPublic, "remediation".
func LayoutIssueDetails(items []LayoutIssue, view IssueView) map[string]interface{} {
	d := map[string]interface{}{
		"fields": LayoutIssueFields(items),
		"issues": RenderLayoutIssues(items, view),
	}
	if view == ViewPublic {
		d["remediation"] = LayoutIssuesRemediation
	}
	return d
}

// ExportIssues compares the NFS exports two brokers report (a move's source
// a and target b, labelled with the broker names aName and bName in the
// admin and log views): the backend, server, export (trailing slashes
// ignored) and subpath root. Nil when both are on the same export settings.
func ExportIssues(aName string, a *api.BrokerWorkspaceStorage, bName string, b *api.BrokerWorkspaceStorage) []LayoutIssue {
	backend := func(d *api.BrokerWorkspaceStorage) string {
		if d == nil {
			return ""
		}
		return d.Backend
	}
	if backend(a) != api.WorkspaceStorageBackendNFS || backend(b) != api.WorkspaceStorageBackendNFS || a.NFS == nil || b.NFS == nil {
		if backend(a) != backend(b) {
			return []LayoutIssue{NewMismatchIssue("backend", aName, backend(a), bName, backend(b))}
		}
		return []LayoutIssue{reasonIssue("nfs", "a broker reports no usable NFS share", "nfs: a broker reports no usable NFS share")}
	}
	var out []LayoutIssue
	if a.NFS.Server != b.NFS.Server {
		out = append(out, NewMismatchIssue("nfs server", aName, a.NFS.Server, bName, b.NFS.Server))
	}
	if api.NormalizeExportPath(a.NFS.Export) != api.NormalizeExportPath(b.NFS.Export) {
		out = append(out, NewMismatchIssue("nfs export", aName, a.NFS.Export, bName, b.NFS.Export))
	}
	if a.NFS.SubPathRoot != b.NFS.SubPathRoot {
		out = append(out, NewMismatchIssue("subpath_root", aName, a.NFS.SubPathRoot, bName, b.NFS.SubPathRoot))
	}
	if len(out) == 0 && !api.SameWorkspaceExport(a, b) {
		out = append(out, reasonIssue("nfs", "a broker reports an incomplete NFS share", "nfs: a broker reports an incomplete NFS share"))
	}
	return out
}

// ExportIDIssue is two brokers' differing export identity markers.
func ExportIDIssue(aName, aID, bName, bID string) LayoutIssue {
	return NewMismatchIssue("export identity marker", aName, aID, bName, bID)
}
