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
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestViewHidesLink: artifact view leaves the link out in agent mode
// unless --show-link is given, and always prints it in human mode.
func TestViewHidesLink(t *testing.T) {
	for _, tc := range []struct {
		mode     CLIMode
		showLink bool
		hide     bool
	}{
		{ModeAgent, false, true},
		{ModeAgent, true, false},
		{ModeHuman, false, false},
		{ModeHuman, true, false},
	} {
		assert.Equal(t, tc.hide, viewHidesLink(tc.mode, tc.showLink), "mode=%s show-link=%v", tc.mode, tc.showLink)
	}
}

// TestArtifactViewShowLinkFlag: view has a --show-link flag, off by
// default.
func TestArtifactViewShowLinkFlag(t *testing.T) {
	f := artifactViewCmd.Flags().Lookup("show-link")
	if assert.NotNil(t, f) {
		assert.Equal(t, "false", f.DefValue)
	}
}

// TestPrintViewResultSanitizesLink: a printed link still goes through
// safeText, so control and format characters in it are replaced.
func TestPrintViewResultSanitizesLink(t *testing.T) {
	var out bytes.Buffer
	printViewResult(&out, viewResult{
		Ref:       "scion://artifact/a@1",
		URL:       "https://hub.example/view/x\x1b[2J\u202e/index.html",
		ExpiresAt: time.Date(2026, 1, 2, 3, 4, 0, 0, time.UTC),
	})
	first := strings.SplitN(out.String(), "\n", 2)[0]
	assert.Equal(t, "https://hub.example/view/x\ufffd[2J\ufffd/index.html", first)
}

// TestPrintViewResultHiddenLink: with the link hidden, the output names
// the version and the expiry, says how to print the link, and holds no
// part of the link, even when the result still carries one.
func TestPrintViewResultHiddenLink(t *testing.T) {
	var out bytes.Buffer
	printViewResult(&out, viewResult{
		Ref:        "scion://artifact/a@1",
		URL:        "https://hub.example/api/v1/artifacts/view/cap/index.html",
		LinkHidden: true,
		ExpiresAt:  time.Date(2026, 1, 2, 3, 4, 0, 0, time.UTC),
	})
	assert.NotContains(t, out.String(), "hub.example")
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if assert.Len(t, lines, 2, out.String()) {
		assert.True(t, strings.HasPrefix(lines[0], "View of scion://artifact/a@1, valid until "), lines[0])
		assert.Equal(t, "The link is not printed in agent mode; run with --show-link to print it.", lines[1])
	}
}
