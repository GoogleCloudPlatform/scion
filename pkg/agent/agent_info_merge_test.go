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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

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
