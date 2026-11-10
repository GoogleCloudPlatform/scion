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

package agent

import (
	"context"
	"log/slog"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// Display-only platform skill entries (ptone/scion#5431). The Hub seeds an
// optional scion-platform:// entry per platform skill; provisioning installs
// those skills itself, so the entries must not reach skill resolution and
// must not log an unsupported_scheme warning on every start.

const (
	// platformEntryURI is a seeded display-only platform entry.
	platformEntryURI = "scion-platform://alpha"
	// unsupportedSchemeURI is a user-defined ref with a scheme no resolver
	// serves.
	unsupportedSchemeURI = "ftp://example.org/skills/s"
)

// provisionWithRoutingResolver provisions an agent whose template declares
// the good skill (required) and ref (optional), resolving through a
// RoutingSkillResolver whose Hub fallback serves the good skill, and
// returns the captured log records.
func provisionWithRoutingResolver(t *testing.T, ref string) []slog.Record {
	t.Helper()
	content := []byte("# Good Skill")
	srv := newInstallFailServer(t, content)
	projectScionDir := writeRefLogTemplate(t, ref, true)

	hub := &mockResolver{resolved: installFailSkills(srv.URL, content, false)[:1]}
	ctx := ContextWithSkillResolver(context.Background(), NewRoutingSkillResolver(hub))
	records := captureSlog(t)

	if _, _, _, err := ProvisionAgent(ctx, "platform-entry-agent", "ref-log-tpl", "", "", projectScionDir, "", "", "", ""); err != nil {
		t.Fatalf("ProvisionAgent must succeed when only an optional skill is unresolvable, got: %v", err)
	}
	return *records
}

// skipWarnings returns the codes of the Warn records that report an
// optional skill as skipped by resolution ("missing" for one missing from
// the resolver response). The skill reference itself is logged redacted,
// so records are told apart by message and code, not by reference.
func skipWarnings(records []slog.Record) []string {
	var codes []string
	for _, r := range records {
		if r.Level != slog.LevelWarn {
			continue
		}
		switch r.Message {
		case "optional skill could not be resolved; skipped":
			code := ""
			r.Attrs(func(a slog.Attr) bool {
				if a.Key == "code" {
					code = a.Value.String()
					return false
				}
				return true
			})
			codes = append(codes, code)
		case "optional skill missing from resolver response; skipped":
			codes = append(codes, "missing")
		}
	}
	return codes
}

func TestProvision_PlatformSkillEntryNotResolved(t *testing.T) {
	records := provisionWithRoutingResolver(t, platformEntryURI)
	if codes := skipWarnings(records); len(codes) > 0 {
		t.Errorf("display-only platform entry logged skip warnings with codes %q, want none", codes)
	}
}

func TestProvision_UnsupportedSchemeOptionalSkillStillWarns(t *testing.T) {
	records := provisionWithRoutingResolver(t, unsupportedSchemeURI)
	codes := skipWarnings(records)
	if len(codes) != 1 || codes[0] != "unsupported_scheme" {
		t.Errorf("optional skill with an unsupported scheme: skip warning codes = %q, want [\"unsupported_scheme\"]", codes)
	}
}

func TestResolvableSkillRefs(t *testing.T) {
	in := []api.SkillReference{
		{URI: installFailGoodURI},
		{URI: platformEntryURI, Optional: true},
		{URI: "SCION-PLATFORM://beta", Optional: true},
		{URI: "scion-platform://required"},
		{URI: unsupportedSchemeURI, Optional: true},
	}
	got := resolvableSkillRefs(in)
	want := []string{installFailGoodURI, "scion-platform://required", unsupportedSchemeURI}
	if len(got) != len(want) {
		t.Fatalf("resolvableSkillRefs returned %d refs %v, want %v", len(got), got, want)
	}
	for i, ref := range got {
		if ref.URI != want[i] {
			t.Errorf("ref %d = %q, want %q", i, ref.URI, want[i])
		}
	}
	if len(in) != 5 || in[1].URI != platformEntryURI {
		t.Errorf("resolvableSkillRefs modified its input: %v", in)
	}
}
