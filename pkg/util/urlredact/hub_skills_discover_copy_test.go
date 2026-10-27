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

package urlredact

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/util/urlredact/urlredacttest"
)

// The skills discovery handler (pkg/hub/handlers_skills_discover.go)
// removes the user info of the source URL inline, not in a function, and
// pkg/hub is too heavy to link into a light table test. hubSkillsDiscoverBase
// runs a copy of that code: the lines between hubSkillsSpanStart and
// hubSkillsSpanEnd below are byte-identical to the handler's
// (TestHubSkillsDiscoverCopyIdentical). It returns the base URL the handler
// logs and echoes, or hubSkillsRejected where the handler answers 400
// without echoing the URL.

const (
	hubSkillsSpanStart = "\tu, parseErr := url.Parse(req.SourceURL)\n"
	hubSkillsSpanEnd   = "\tbase := u.String()\n"
	hubSkillsRejected  = "(rejected: 400, URL not echoed)"
)

func hubSkillsDiscoverBase(sourceURL string) (out string) {
	req := struct{ SourceURL string }{SourceURL: sourceURL}
	var w http.ResponseWriter
	const ErrCodeInvalidRequest = "invalid_request"
	writeError := func(_ http.ResponseWriter, _ int, _, _ string, _ map[string]interface{}) {
		out = hubSkillsRejected
	}
	u, parseErr := url.Parse(req.SourceURL)
	if parseErr != nil || u.Scheme != "https" || !strings.EqualFold(u.Host, "github.com") {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
			"sourceUrl must be an https://github.com/... URL", nil)
		return
	}

	// Canonicalize before anything downstream sees the URL:
	//
	//   - Lowercase the host. The gate above is case-insensitive (hostnames are),
	//     but config.DetectRemoteType matches "github.com" exactly, so a
	//     "https://GitHub.com/..." URL would otherwise sail past this check and
	//     die later with a generic fetch error.
	//   - Split off a ?token=SECRET_NAME suffix. A skill URI may name the secret
	//     used to resolve it, but discoverResourceDirs builds child URLs by plain
	//     concatenation ("<sourceURL>/<child>"), which would bury the query
	//     mid-path and make every child unnormalizable. It is re-attached per
	//     skill below. It is also irrelevant to the fetch itself, which
	//     authenticates from project credentials, so the fetch gets the bare URL.
	//     Only the supported query parameter (token) is kept; every other
	//     parameter is dropped here, so nothing else the caller put in the
	//     query is copied onto the returned URIs.
	//   - Drop any #fragment, for the same concatenation reason: ".../skills#notes"
	//     would yield child URLs like ".../skills#notes/alpha-skill".
	//   - Drop any userinfo. "https://x-access-token:SECRET@github.com/..." parses
	//     with Host == "github.com", and base is both logged and echoed in the
	//     "no skills found at <base>" message — so leaving it in would write a
	//     pasted credential to the hub log and hand it back to the client.
	//     Discovery authenticates from project credentials, never from the URL.
	tokenName, hasToken := api.SkillURITokenParam(u.RawQuery)
	if hasToken && !api.IsValidSkillTokenSecretName(tokenName) {
		// Input hygiene, not the redaction boundary: a secret name and a pasted
		// secret cannot be told apart from the string, so this check only
		// rejects values that cannot be a secret name. The value is never
		// echoed. What keeps the query out of the logs is the per-skill
		// handling below, which must not be relaxed on the strength of this
		// check.
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
			"the token query parameter in sourceUrl must name a secret "+
				"(uppercase letters, digits and underscores, starting with a letter)", nil)
		return
	}
	tokenSuffix := ""
	if hasToken {
		tokenSuffix = "?token=" + tokenName
	}
	u.Host = "github.com"
	u.User = nil
	u.RawQuery = ""
	// A bare trailing '?' (".../skills?" or ".../skills?#notes") parses with an
	// empty RawQuery but ForceQuery set, and u.String() would still write the
	// '?': base and every child URL would carry it.
	u.ForceQuery = false
	u.Fragment = ""
	base := u.String()
	_ = tokenSuffix
	return base
}

// hubSkillsSpan returns the text of src from the line hubSkillsSpanStart to
// the line hubSkillsSpanEnd, both included; each must occur once.
func hubSkillsSpan(t *testing.T, name, src string) string {
	t.Helper()
	// Line endings are normalised first: the anchors end in "\n", and a
	// CRLF checkout (Windows) gives both files "\r\n" endings.
	src = strings.ReplaceAll(src, "\r\n", "\n")
	if strings.Count(src, hubSkillsSpanStart) != 1 || strings.Count(src, hubSkillsSpanEnd) != 1 {
		t.Fatalf("%s: span anchors found %d and %d times, want 1 each", name, strings.Count(src, hubSkillsSpanStart), strings.Count(src, hubSkillsSpanEnd))
	}
	i := strings.Index(src, hubSkillsSpanStart)
	j := strings.Index(src, hubSkillsSpanEnd)
	if j < i {
		t.Fatalf("%s: span end before its start", name)
	}
	return src[i : j+len(hubSkillsSpanEnd)]
}

// The copy run by the table is the handler's code, byte for byte.
func TestHubSkillsDiscoverCopyIdentical(t *testing.T) {
	hub, err := os.ReadFile(filepath.Join("..", "..", "hub", "handlers_skills_discover.go"))
	if err != nil {
		t.Fatal(err)
	}
	cp, err := os.ReadFile("hub_skills_discover_copy_test.go")
	if err != nil {
		t.Fatal(err)
	}
	if hubSkillsSpan(t, "pkg/hub", string(hub)) != hubSkillsSpan(t, "copy", string(cp)) {
		t.Error("hubSkillsDiscoverBase's copy differs from pkg/hub/handlers_skills_discover.go; copy the span again and update the table")
	}
}

// The handler's user info removal on the table every URL redactor shares.
func TestTable_HubSkillsDiscover(t *testing.T) {
	urlredacttest.Check(t, urlredacttest.HubSkillsDiscoverBase, hubSkillsDiscoverBase)
}
