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

// Package urlredacttest holds the one table every URL redactor in the tree
// is checked against, so that they all follow the user info rule of
// pkg/util/urlredact, and the redactors that do not follow it yet are
// listed rather than forgotten. It is imported by tests only.
package urlredacttest

import (
	"strings"
	"testing"
)

// The redactors on the table, one column each. A column is named after its
// package and function; a function that takes a text and the URL it
// quotes (an endpoint scrubber) is run on Text(in) with in as the URL, and
// a function that takes a text alone is run on Text(in).
const (
	URLRedactRedactUserInfo   = "urlredact.RedactUserInfo"
	URLRedactRedactUserInfoIn = "urlredact.RedactUserInfoIn"
	ConfigRedactURL           = "config.RedactURL"
	ConfigRedactURLsIn        = "config.RedactURLsIn"
	APIClientRedactURL        = "apiclient.redactURLCredentials"
	LoggingSanitizeText       = "logging.SanitizeText"
	CmdRedactEndpointIn       = "cmd.redactEndpointIn"

	TunnelClientRedactEndpoint         = "tunnelclient.RedactEndpoint"
	TunnelClientScrubEndpoint          = "tunnelclient.scrubEndpoint"
	RuntimeBrokerRedactCloneURL        = "runtimebroker.redactCloneURL"
	RuntimeBrokerSanitizeCloneErrorTxt = "runtimebroker.sanitizeCloneErrorText"
	ProvisionRedactCloneURL            = "provision.redactCloneURL"
	ProvisionSanitizeCloneOutput       = "provision.sanitizeCloneOutput"
	RemoteFetchRedactURL               = "remotefetch.redactURL"
	CmdDisplaySourceURL                = "cmd.displaySourceURL"
	HubSkillsDiscoverBase              = "hub.skillsDiscoverBase"
)

// Columns lists every column: its name, the name of its constant here, and
// the test file that runs it (relative to the repository root).
// pkg/util/urlredact's TestTableColumnsHaveRunners checks that each file
// calls Check for its column.
var Columns = []struct{ Name, Const, Runner string }{
	{URLRedactRedactUserInfo, "URLRedactRedactUserInfo", "pkg/util/urlredact/urlredact_test.go"},
	{URLRedactRedactUserInfoIn, "URLRedactRedactUserInfoIn", "pkg/util/urlredact/urlredact_test.go"},
	{ConfigRedactURL, "ConfigRedactURL", "pkg/config/url_redact_table_test.go"},
	{ConfigRedactURLsIn, "ConfigRedactURLsIn", "pkg/config/url_redact_table_test.go"},
	{APIClientRedactURL, "APIClientRedactURL", "pkg/apiclient/redact_table_test.go"},
	{LoggingSanitizeText, "LoggingSanitizeText", "pkg/util/logging/sanitize_text_url_table_test.go"},
	{CmdRedactEndpointIn, "CmdRedactEndpointIn", "cmd/hub_redact_table_test.go"},
	{TunnelClientRedactEndpoint, "TunnelClientRedactEndpoint", "pkg/tunnelclient/endpoint_url_table_test.go"},
	{TunnelClientScrubEndpoint, "TunnelClientScrubEndpoint", "pkg/tunnelclient/endpoint_url_table_test.go"},
	{RuntimeBrokerRedactCloneURL, "RuntimeBrokerRedactCloneURL", "pkg/runtimebroker/clone_url_table_test.go"},
	{RuntimeBrokerSanitizeCloneErrorTxt, "RuntimeBrokerSanitizeCloneErrorTxt", "pkg/runtimebroker/clone_url_table_test.go"},
	{ProvisionRedactCloneURL, "ProvisionRedactCloneURL", "pkg/provision/clone_url_table_test.go"},
	{ProvisionSanitizeCloneOutput, "ProvisionSanitizeCloneOutput", "pkg/provision/clone_url_table_test.go"},
	{RemoteFetchRedactURL, "RemoteFetchRedactURL", "pkg/artifacts/remotefetch/redact_url_table_test.go"},
	{CmdDisplaySourceURL, "CmdDisplaySourceURL", "cmd/templates_update_url_table_test.go"},
	// A byte-identical copy of the inline code in
	// pkg/hub/handlers_skills_discover.go, so the table stays light. It does
	// not use the shared rule, but it shows no row's secret (it accepts only
	// the host github.com, then drops the user info), so it is checked like a
	// migrated column: any future leak fails its test. It shows no leak
	// because of an incidental functional gate (the host must equal
	// "github.com"), not because the code redacts; if that gate is widened
	// (an allowlist, a pattern, other forges), this column must be measured
	// again. Only the span is pinned: the stubs that stand in for the rest of
	// the handler (a 400 that does not echo the URL) are not, so a change to
	// what the handler does after the span does not show here.
	{HubSkillsDiscoverBase, "HubSkillsDiscoverBase", "pkg/util/urlredact/hub_skills_discover_copy_test.go"},
}

// KnownUnmigrated are the columns whose redactor does not use the shared
// rule yet. Their current output is pinned all the same, so moving one of
// them to the shared rule fails its test until it is taken off this list
// and its column updated; and each must still show the secret of at least
// one row, so the gap stays visible.
var KnownUnmigrated = map[string]bool{
	TunnelClientRedactEndpoint:         true,
	TunnelClientScrubEndpoint:          true,
	RuntimeBrokerRedactCloneURL:        true,
	RuntimeBrokerSanitizeCloneErrorTxt: true,
	ProvisionRedactCloneURL:            true,
	ProvisionSanitizeCloneOutput:       true,
	RemoteFetchRedactURL:               true,
	CmdDisplaySourceURL:                true,
}

// Text returns the error text a text column is run on: in quoted the way
// net/http's errors quote a URL.
func Text(in string) string { return `Get "` + in + `": refused` }

// Row is one input and what each column shows for it.
type Row struct {
	Name string
	In   string
	// Secret is a made-up credential in In ("" for none). No 4-byte part of
	// it may appear in the output of a column that is neither known to be
	// unmigrated nor listed in Residual.
	Secret string
	// Residual lists the columns that may show Secret for this row, by the
	// rule itself (see urlredact.Undelimited) rather than by a bug.
	Residual []string
	// Want is the whole output of every column.
	Want map[string]string
}

// Check runs redact (which maps a row's In to its column's output; see
// Text) on every row: the output must equal the row's Want for column,
// and, unless column is KnownUnmigrated or the row lists it in Residual,
// must hold no 4-byte part of the row's Secret. A KnownUnmigrated column
// must show the secret of at least one row.
func Check(t *testing.T, column string, redact func(in string) string) {
	t.Helper()
	known := false
	for _, c := range Columns {
		known = known || c.Name == column
	}
	if !known {
		t.Fatalf("column %q is not in urlredacttest.Columns", column)
	}
	leaks := 0
	for _, r := range Rows {
		want, ok := r.Want[column]
		if !ok {
			t.Errorf("%s: no Want for column %s", r.Name, column)
			continue
		}
		got := redact(r.In)
		if got != want {
			t.Errorf("%s: %s(%q) = %q, want %q", r.Name, column, r.In, got, want)
		}
		frag := fragmentIn(got, r.Secret)
		if frag != "" {
			leaks++
		}
		if frag != "" && !KnownUnmigrated[column] && !residual(r, column) {
			t.Errorf("%s: %s(%q) = %q, shows %q of the secret", r.Name, column, r.In, got, frag)
		}
	}
	if KnownUnmigrated[column] && leaks == 0 {
		t.Errorf("%s shows no row's secret: take it off KnownUnmigrated and add the test that pins its new behaviour", column)
	}
}

func residual(r Row, column string) bool {
	for _, c := range r.Residual {
		if c == column {
			return true
		}
	}
	return false
}

// fragmentIn returns the first 4-byte part of secret found in out, or "".
func fragmentIn(out, secret string) string {
	for i := 0; i+4 <= len(secret); i++ {
		if strings.Contains(out, secret[i:i+4]) {
			return secret[i : i+4]
		}
	}
	return ""
}
