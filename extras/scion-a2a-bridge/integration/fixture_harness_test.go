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

package integration_test

import (
	"encoding/json"
	"os"
	"slices"
	"testing"
)

type fixtureSource struct {
	Title string `json:"title"`
	URL   string `json:"url"`
}

type identityFixtureFile struct {
	Schema  string            `json:"schema"`
	Comment string            `json:"comment"`
	Sources []fixtureSource   `json:"sources"`
	Entries []identityFixture `json:"entries"`
}

type identityFixture struct {
	Category       string   `json:"category"`
	PrincipalKind  string   `json:"principal_kind"`
	PrincipalLabel string   `json:"principal_label"`
	Issuer         string   `json:"issuer,omitempty"`
	Subject        string   `json:"subject,omitempty"`
	Email          string   `json:"email,omitempty"`
	Audience       string   `json:"audience,omitempty"`
	Claims         []string `json:"claims,omitempty"`
	Expected       string   `json:"expected"`
}

type protocolFixtureFile struct {
	Schema  string            `json:"schema"`
	Comment string            `json:"comment"`
	Sources []fixtureSource   `json:"sources"`
	Entries []protocolFixture `json:"entries"`
}

type protocolFixture struct {
	Category        string          `json:"category"`
	ProtocolVersion string          `json:"protocol_version"`
	Binding         string          `json:"binding"`
	Method          string          `json:"method"`
	Path            string          `json:"path"`
	Envelope        json.RawMessage `json:"envelope"`
}

type stableAgentFixture struct {
	Schema              string          `json:"schema"`
	Comment             string          `json:"comment"`
	Sources             []fixtureSource `json:"sources"`
	ProjectID           string          `json:"project_id"`
	AgentID             string          `json:"agent_id"`
	TaskID              string          `json:"task_id"`
	ContextID           string          `json:"context_id"`
	InitialReplica      string          `json:"initial_replica"`
	DelayedEventReplica string          `json:"delayed_event_replica"`
	Capabilities        []string        `json:"capabilities"`
}

func loadIdentityFixtures(t *testing.T, path string) identityFixtureFile {
	t.Helper()
	var fixtures identityFixtureFile
	loadFixtureFile(t, path, &fixtures)
	if fixtures.Schema == "" || fixtures.Comment == "" || len(fixtures.Sources) == 0 {
		t.Fatal("identity fixture is missing schema comments or sources")
	}
	return fixtures
}

func loadProtocolFixtures(t *testing.T, path string) protocolFixtureFile {
	t.Helper()
	var fixtures protocolFixtureFile
	loadFixtureFile(t, path, &fixtures)
	if fixtures.Schema == "" || fixtures.Comment == "" || len(fixtures.Sources) == 0 {
		t.Fatal("protocol fixture is missing schema comments or sources")
	}
	return fixtures
}

func loadAgentFixture(t *testing.T, path string) stableAgentFixture {
	t.Helper()
	var fixture stableAgentFixture
	loadFixtureFile(t, path, &fixture)
	if fixture.Schema == "" || fixture.Comment == "" || len(fixture.Sources) == 0 {
		t.Fatal("stable agent fixture is missing schema comments or sources")
	}
	return fixture
}

func loadFixtureFile(t *testing.T, path string, target any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, target); err != nil {
		t.Fatal(err)
	}
}

func identityCategories(fixtures identityFixtureFile) []string {
	categories := make([]string, 0, len(fixtures.Entries))
	for _, fixture := range fixtures.Entries {
		categories = append(categories, fixture.Category)
	}
	return categories
}

func protocolCategories(fixtures protocolFixtureFile) []string {
	categories := make([]string, 0, len(fixtures.Entries))
	for _, fixture := range fixtures.Entries {
		categories = append(categories, fixture.Category)
	}
	return categories
}

func assertExactCategories(t *testing.T, got, want []string) {
	t.Helper()
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("categories = %v; want %v", got, want)
	}
}

type acceptanceScaffold struct {
	Schema string            `json:"schema"`
	Layers []acceptanceLayer `json:"layers"`
}

type acceptanceLayer struct {
	Name             string               `json:"name"`
	Status           string               `json:"status"`
	Passing          bool                 `json:"passing"`
	Dependencies     []string             `json:"dependencies"`
	ExternalLiveOnly []string             `json:"external_live_only,omitempty"`
	Sublayers        []acceptanceSublayer `json:"sublayers,omitempty"`
}

type acceptanceSublayer struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Passing bool   `json:"passing"`
}
