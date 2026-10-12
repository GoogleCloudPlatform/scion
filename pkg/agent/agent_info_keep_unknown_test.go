// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package agent

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// sciontoolAgentInfo is agent-info.json as sciontool's StatusHandler leaves
// it after a Notification-set waiting_for_input on a tool: toolName and
// activitySource are keys StatusHandler really writes and api.AgentInfo has
// no field for; futureField stands in for a key a newer writer adds
// (ptone/scion#4591).
const sciontoolAgentInfo = `{
  "name": "keep-agent",
  "phase": "running",
  "activity": "waiting_for_input",
  "activitySource": "notification",
  "toolName": "Bash",
  "futureField": {"nested": ["a", "<b>"], "n": 1}
}`

// assertUnknownAgentInfoKeysKept checks that data still carries the unknown
// keys of sciontoolAgentInfo with their original values.
func assertUnknownAgentInfoKeysKept(t *testing.T, data []byte) map[string]json.RawMessage {
	t.Helper()
	var got map[string]json.RawMessage
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("json.Unmarshal(updated) error = %v\n%s", err, data)
	}
	want := map[string]string{
		"activitySource": `"notification"`,
		"toolName":       `"Bash"`,
		"futureField":    `{"nested":["a","<b>"],"n":1}`,
	}
	for k, w := range want {
		v, ok := got[k]
		if !ok {
			t.Errorf("key %q was dropped by the update:\n%s", k, data)
			continue
		}
		var c bytes.Buffer
		if err := json.Compact(&c, v); err != nil {
			t.Fatalf("json.Compact(%s) error = %v", k, err)
		}
		if c.String() != w {
			t.Errorf("key %q = %s, want %s", k, c.String(), w)
		}
	}
	return got
}

func writeSavedAgentInfoFile(t *testing.T, content string) (projectDir, agentName, path string) {
	t.Helper()
	projectDir = filepath.Join(t.TempDir(), config.DotScion)
	agentName = "keep-agent"
	agentHome := config.GetAgentHomePath(projectDir, agentName)
	if err := os.MkdirAll(agentHome, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	path = filepath.Join(agentHome, "agent-info.json")
	if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	return projectDir, agentName, path
}

// TestUpdateSavedAgentInfoKeepsUnknownFields: sciontool (writer A) set keys
// api.AgentInfo does not know; the broker's updateSavedAgentInfo (writer B)
// then updates other fields. A's keys survive.
func TestUpdateSavedAgentInfoKeepsUnknownFields(t *testing.T) {
	projectDir, agentName, path := writeSavedAgentInfoFile(t, sciontoolAgentInfo)

	if err := UpdateAgentConfig(agentName, projectDir, "stopped", "docker", "fast"); err != nil {
		t.Fatalf("UpdateAgentConfig() error = %v", err)
	}
	if err := SetSavedRunID(agentName, projectDir, "run-1"); err != nil {
		t.Fatalf("SetSavedRunID() error = %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	assertUnknownAgentInfoKeysKept(t, data)
	var info api.AgentInfo
	if err := json.Unmarshal(data, &info); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if info.Phase != "stopped" || info.Runtime != "docker" || info.Profile != "fast" || info.RunID != "run-1" {
		t.Errorf("known fields not updated: %+v", info)
	}
	if info.Name != "keep-agent" || info.Activity != "waiting_for_input" {
		t.Errorf("unrelated known fields changed: %+v", info)
	}
	if fi, err := os.Stat(path); err != nil {
		t.Fatalf("Stat() error = %v", err)
	} else if fi.Mode().Perm() != 0o640 {
		t.Errorf("mode = %o, want 640", fi.Mode().Perm())
	}
}

// TestUpdateSavedAgentInfoKnownOnlyOutputUnchanged: with no unknown keys, or
// an original that is JSON null, the file is exactly the plain struct
// encoding, as before ptone/scion#4591.
func TestUpdateSavedAgentInfoKnownOnlyOutputUnchanged(t *testing.T) {
	start := api.AgentInfo{Name: "keep-agent", Phase: "running", Activity: "thinking", Profile: "slow"}
	startData, err := json.MarshalIndent(start, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, content string
		base          api.AgentInfo
	}{
		{name: "known keys only", content: string(startData), base: start},
		{name: "json null", content: "null", base: api.AgentInfo{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			projectDir, agentName, path := writeSavedAgentInfoFile(t, tc.content)
			if err := UpdateAgentConfig(agentName, projectDir, "stopped", "", ""); err != nil {
				t.Fatalf("UpdateAgentConfig() error = %v", err)
			}
			want := tc.base
			want.Phase = "stopped"
			wantData, err := json.MarshalIndent(want, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, wantData) {
				t.Errorf("output differs from the plain struct encoding:\ngot:\n%s\nwant:\n%s", got, wantData)
			}
		})
	}
}

// TestPersistAgentInfoStateKeepsUnknownFields: sciontool (writer A) set keys
// api.AgentInfo does not know; the list path's persistAgentInfoState (writer
// B) then records the terminal phase. A's keys survive, and the activity it
// clears is removed.
func TestPersistAgentInfoStateKeepsUnknownFields(t *testing.T) {
	_, _, path := writeSavedAgentInfoFile(t, sciontoolAgentInfo)

	if err := persistAgentInfoState(path, string(state.PhaseStopped), ""); err != nil {
		t.Fatalf("persistAgentInfoState() error = %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	got := assertUnknownAgentInfoKeysKept(t, data)
	if _, ok := got["activity"]; ok {
		t.Errorf("cleared known key activity is still present:\n%s", data)
	}
	var info api.AgentInfo
	if err := json.Unmarshal(data, &info); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if info.Phase != string(state.PhaseStopped) || info.Name != "keep-agent" {
		t.Errorf("known fields wrong: %+v", info)
	}
	if fi, err := os.Stat(path); err != nil {
		t.Fatalf("Stat() error = %v", err)
	} else if fi.Mode().Perm() != 0o640 {
		t.Errorf("mode = %o, want 640", fi.Mode().Perm())
	}
}

// TestPersistAgentInfoStateKnownOnlyOutputUnchanged: with no unknown keys, or
// an original that is JSON null, the file is exactly the plain struct
// encoding, as before ptone/scion#4591.
func TestPersistAgentInfoStateKnownOnlyOutputUnchanged(t *testing.T) {
	start := api.AgentInfo{Name: "keep-agent", Phase: "running", Activity: "thinking", Profile: "slow"}
	startData, err := json.MarshalIndent(start, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, content string
		base          api.AgentInfo
	}{
		{name: "known keys only", content: string(startData), base: start},
		{name: "json null", content: "null", base: api.AgentInfo{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, path := writeSavedAgentInfoFile(t, tc.content)
			if err := persistAgentInfoState(path, string(state.PhaseStopped), ""); err != nil {
				t.Fatalf("persistAgentInfoState() error = %v", err)
			}
			want := tc.base
			want.Phase = string(state.PhaseStopped)
			want.Activity = ""
			wantData, err := json.MarshalIndent(want, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, wantData) {
				t.Errorf("output differs from the plain struct encoding:\ngot:\n%s\nwant:\n%s", got, wantData)
			}
		})
	}
}

// TestAgentInfoMissingOrEmptyBehaviorUnchanged: a missing file is still a
// no-op for updateSavedAgentInfo and an error for persistAgentInfoState; an
// empty file is still an error for both, and is left as is.
func TestAgentInfoMissingOrEmptyBehaviorUnchanged(t *testing.T) {
	projectDir := filepath.Join(t.TempDir(), config.DotScion)
	if err := UpdateAgentConfig("absent", projectDir, "stopped", "", ""); err != nil {
		t.Errorf("UpdateAgentConfig() missing file error = %v, want nil", err)
	}
	if _, err := os.Stat(filepath.Join(config.GetAgentHomePath(projectDir, "absent"), "agent-info.json")); !os.IsNotExist(err) {
		t.Errorf("missing file was created (stat err = %v)", err)
	}
	if err := persistAgentInfoState(filepath.Join(t.TempDir(), "agent-info.json"), "stopped", ""); err == nil {
		t.Error("persistAgentInfoState() missing file error = nil, want error")
	}

	projectDir, agentName, path := writeSavedAgentInfoFile(t, "")
	if err := UpdateAgentConfig(agentName, projectDir, "stopped", "", ""); err == nil {
		t.Error("UpdateAgentConfig() empty file error = nil, want error")
	}
	if err := persistAgentInfoState(path, "stopped", ""); err == nil {
		t.Error("persistAgentInfoState() empty file error = nil, want error")
	}
	if data, err := os.ReadFile(path); err != nil || len(data) != 0 {
		t.Errorf("empty file changed: %q, err = %v", data, err)
	}
}

func TestMarshalAgentInfoKeepUnknown(t *testing.T) {
	info := &api.AgentInfo{Name: "a", Phase: "stopped"}
	plain, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		t.Fatal(err)
	}

	t.Run("not a JSON object yields the plain encoding", func(t *testing.T) {
		for _, orig := range []string{"", "null", "{", "[1]", `"s"`, "{}"} {
			got, err := marshalAgentInfoKeepUnknown([]byte(orig), info)
			if err != nil {
				t.Fatalf("orig %q: error = %v", orig, err)
			}
			if !bytes.Equal(got, plain) {
				t.Errorf("orig %q: got\n%s\nwant\n%s", orig, got, plain)
			}
		}
	})

	t.Run("known keys, any case, are not carried over", func(t *testing.T) {
		got, err := marshalAgentInfoKeepUnknown([]byte(`{"activity":"x","Activity":"y","RUNID":"r"}`), info)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, plain) {
			t.Errorf("got\n%s\nwant\n%s", got, plain)
		}
	})

	t.Run("unknown keys follow the known ones, sorted, values unescaped", func(t *testing.T) {
		got, err := marshalAgentInfoKeepUnknown([]byte(`{"zeta":"<&>","toolName":"Bash","phase":"running"}`), info)
		if err != nil {
			t.Fatal(err)
		}
		// plain ends with "\n}"; the unknown keys are appended after its
		// last field, in sorted order.
		want := string(plain[:len(plain)-2]) + ",\n  \"toolName\": \"Bash\",\n  \"zeta\": \"<&>\"\n}"
		if string(got) != want {
			t.Errorf("got\n%s\nwant\n%s", got, want)
		}
	})
}

// TestAgentInfoKnownKeySet pins which agent-info.json keys
// marshalAgentInfoKeepUnknown treats as known: the JSON name of every
// exported api.AgentInfo field. A "-" tagged field (VanishedPodReport) and
// the sciontool-only keys toolName and activitySource are unknown, so they
// are carried over.
func TestAgentInfoKnownKeySet(t *testing.T) {
	typ := reflect.TypeOf(api.AgentInfo{})
	var want []string
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if !f.IsExported() {
			continue
		}
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if name == "" {
			name = f.Name
		}
		want = append(want, name)
	}
	if len(want) == 0 {
		t.Fatal("api.AgentInfo has no JSON fields")
	}
	// Mixed-case spellings are known too: json.Unmarshal matches field
	// names case-insensitively, so it already read them into the struct.
	for _, k := range append(want, "phase", "activity", "runId", "deletedAt", "RUNID", "Phase") {
		if !isKnownAgentInfoKey(k) {
			t.Errorf("isKnownAgentInfoKey(%q) = false, want true", k)
		}
	}
	for _, k := range []string{"VanishedPodReport", "vanishedPodReport", "toolName", "activitySource"} {
		if isKnownAgentInfoKey(k) {
			t.Errorf("isKnownAgentInfoKey(%q) = true, want false", k)
		}
	}
	// jsonFieldNames builds a fresh slice on every call, so sorting it in
	// place is safe.
	got := jsonFieldNames(typ)
	slices.Sort(got)
	wantSorted := slices.Clone(want)
	slices.Sort(wantSorted)
	if !slices.Equal(got, wantSorted) {
		var missing, extra []string
		for _, k := range wantSorted {
			if !slices.Contains(got, k) {
				missing = append(missing, k)
			}
		}
		for _, k := range got {
			if !slices.Contains(wantSorted, k) {
				extra = append(extra, k)
			}
		}
		t.Errorf("jsonFieldNames(api.AgentInfo) differs from the exported JSON field names: missing %v, extra %v\ngot  %v\nwant %v", missing, extra, got, wantSorted)
	}
}
