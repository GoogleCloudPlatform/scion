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
// one row, so the gap stays visible. Every redactor on the table uses the
// shared rule now (the skills discover column is checked like a migrated
// one, see Columns), so the list is empty; a redactor added to the table
// before it is moved goes here.
var KnownUnmigrated = map[string]bool{}

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

// SecretFragment returns the first 4-byte part of secret found in out, or
// "": the check Check makes, for tests of a redactor beyond the table.
func SecretFragment(out, secret string) string { return fragmentIn(out, secret) }

// Shape is a URL whose credential a redactor must not show any part of.
// Every value is made up.
type Shape struct {
	Name, URL, Secret string
}

// Shapes are the URL shapes a redactor that shows a URL by its host and
// path (urlredact.HostAndPath) must show no part of the credential of, for
// the per-package tests that run a redactor in its own setting (an error
// that quotes a URL built from the input, git's output). They hold a
// delimiter in the password, a second '@', a password read as a port, a
// token read as a host, a URL nested in a query, and a token in the query
// or fragment of a URL with an '@' in its path.
var Shapes = []Shape{
	{"slash in password", "https://alice:Zq/7mWx2@git.example.com/acme/repo.git", "Zq/7mWx2"},
	{"question mark in password", "https://alice:Zq?7mWx2@git.example.com/acme/repo.git", "Zq?7mWx2"},
	{"hash in password", "https://alice:Zq#7mWx2@git.example.com/acme/repo.git", "Zq#7mWx2"},
	{"second '@' in password", "https://alice:Zq@7mWx2@git.example.com/acme/repo.git", "Zq@7mWx2"},
	{"'@' then '/' in password", "https://alice:Zq@7m/Wx2@git.example.com/acme/repo.git", "Zq@7m/Wx2"},
	{"'@' then '?' in password", "https://alice:Zq@7m?Wx2@git.example.com/acme/repo.git", "Zq@7m?Wx2"},
	{"'@' then a short '/' piece", "https://alice:Zq@7m/Wx@git.example.com/acme/repo.git", "Zq@7m/Wx"},
	{"'@' then a short '?' piece", "https://alice:Zq@7m?Wx@git.example.com/acme/repo.git", "Zq@7m?Wx"},
	{"password read as a port and a path", "https://u:8461/Wx2@git.example.com/acme/repo.git", "8461/Wx2"},
	{"password read as a port, then '?'", "https://u:8461?Wx2@git.example.com/acme/repo.git", "8461?Wx2"},
	{"token read as a host", "https://tKn7Qz/x@git.example.com/acme/repo.git", "tKn7Qz/x"},
	{"token read as a host, then '#'", "https://tKn7Qz#x@git.example.com/acme/repo.git", "tKn7Qz#x"},
	{"URL with user info in a query value", "https://git.example.com/acme/repo.git?next=https://bob:Zq7mWx2@app.example.com/", "Zq7mWx2"},
	{"'@' in the path, token in the query", "https://git.example.com/acme/r@x.git?access_token=Zq7mWx2Pk", "Zq7mWx2Pk"},
	{"'@' in the path, token in the fragment", "https://git.example.com/acme/r@x.git#Zq7mWx2Pk", "Zq7mWx2Pk"},
}

// KeptURL is a URL a host and path redactor shows by its host and path,
// without its credential. Every value is made up.
type KeptURL struct {
	Name, URL, Secret, Want string
}

// KeptURLs are ordinary URLs and token-bearing clone URLs: a host and path
// redactor keeps their host and path and shows no part of the credential.
var KeptURLs = []KeptURL{
	{"no user info", "https://git.example.com/acme/repo.git", "", "https://git.example.com/acme/repo.git"},
	{"token as password", "https://x-access-token:ghs_Zq7mWx2Pk@github.com/acme/repo.git", "ghs_Zq7mWx2Pk", "https://github.com/acme/repo.git"},
	{"token as user name", "https://ghp_Zq7mWx2Pk@github.com/acme/repo.git", "ghp_Zq7mWx2Pk", "https://github.com/acme/repo.git"},
	{"token, query and fragment", "https://oauth2:glpat-Zq7mWx2@gitlab.example.com/group/sub/repo.git?ref=Pk3Lm9#Rt5Vx2", "glpat-Zq7mWx2", "https://gitlab.example.com/group/sub/repo.git"},
	{"second '@' in password", "https://alice:Zq@7mWx2@git.example.com/acme/repo.git", "Zq@7mWx2", "https://git.example.com/acme/repo.git"},
}

// GitEcho returns raw as git prints it in an error: with its user info
// removed up to the first '@' of the authority, as git cuts it, and a
// trailing slash.
func GitEcho(raw string) string {
	scheme, rest, ok := strings.Cut(raw, "://")
	if !ok {
		return raw
	}
	authority := rest
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		authority = rest[:i]
	}
	if i := strings.IndexByte(authority, '@'); i >= 0 {
		rest = rest[i+1:]
	}
	return scheme + "://" + rest + "/"
}

// Echoes returns raw as a tool may print it reformatted, so that no
// rendering of raw itself matches: as git prints it (GitEcho), with a path
// added before its query or fragment ("/info/refs", as git requests it),
// and with its query and fragment cut off.
func Echoes(raw string) []string {
	base, tail := raw, ""
	if i := strings.IndexAny(raw, "?#"); i >= 0 {
		base, tail = raw[:i], raw[i:]
	}
	return []string{GitEcho(raw), base + "/info/refs" + tail, base + "/"}
}
