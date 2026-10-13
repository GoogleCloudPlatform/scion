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

package store

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// brokerCapabilitiesFieldsFixture is the list of JSON field names the hub
// sends in a runtime broker's capabilities object. The web test
// web/src/components/pages/broker-capabilities-contract.test.ts reads the
// same file and checks the web BrokerCapabilities type against it.
const brokerCapabilitiesFieldsFixture = "testdata/broker_capabilities_fields.json"

// structJSONFields returns the JSON object keys encoding/json uses for the
// struct type typ, following its tag rules: options after the first comma
// are stripped, a "-" tag skips the field, an empty name falls back to the
// Go field name, and unexported fields are not encoded. It also returns a
// problem for each field this check does not support: an embedded field, an
// unexported field with a json tag, and two fields with the same JSON name
// (encoding/json leaves both off the wire when they conflict at one depth).
func structJSONFields(typ reflect.Type) (names, problems []string) {
	goField := map[string]string{}
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		tag, hasTag := f.Tag.Lookup("json")
		if f.Anonymous {
			problems = append(problems, fmt.Sprintf("%s.%s: embedded fields are not supported by this check", typ.Name(), f.Name))
			continue
		}
		if !f.IsExported() {
			if hasTag {
				problems = append(problems, fmt.Sprintf("%s.%s: unexported field has json tag %q, which encoding/json ignores", typ.Name(), f.Name, tag))
			}
			continue
		}
		if tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if name == "" {
			name = f.Name
		}
		if prev, ok := goField[name]; ok {
			problems = append(problems, fmt.Sprintf("%s.%s and %s.%s both use JSON name %q", typ.Name(), prev, typ.Name(), f.Name, name))
			continue
		}
		goField[name] = f.Name
		names = append(names, name)
	}
	return names, problems
}

// TestStructJSONFieldsDuplicateName checks that structJSONFields reports two
// fields with the same JSON name and names both Go fields.
func TestStructJSONFieldsDuplicateName(t *testing.T) {
	type dup struct {
		First  bool `json:"same"`
		Second bool `json:"same,omitempty"`
		Other  bool `json:"other"`
	}
	names, problems := structJSONFields(reflect.TypeOf(dup{}))
	if len(problems) != 1 {
		t.Fatalf("problems = %q, want exactly one", problems)
	}
	for _, want := range []string{"dup.First", "dup.Second", `"same"`} {
		if !strings.Contains(problems[0], want) {
			t.Errorf("problem %q does not name %s", problems[0], want)
		}
	}
	if got := strings.Join(names, ","); got != "same,other" {
		t.Errorf("names = %q, want \"same,other\"", got)
	}
}

// TestBrokerCapabilitiesFieldsFixture requires the fixture to list exactly
// the JSON field names of BrokerCapabilities, sorted and without duplicates.
func TestBrokerCapabilitiesFieldsFixture(t *testing.T) {
	raw, err := os.ReadFile(brokerCapabilitiesFieldsFixture)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var fixture struct {
		Fields []string `json:"fields"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	if len(fixture.Fields) == 0 {
		t.Fatalf("%s lists no fields", brokerCapabilitiesFieldsFixture)
	}

	inFixture := make(map[string]bool, len(fixture.Fields))
	for i, name := range fixture.Fields {
		if inFixture[name] {
			t.Errorf("%s lists %q more than once", brokerCapabilitiesFieldsFixture, name)
		}
		inFixture[name] = true
		if i > 0 && fixture.Fields[i-1] >= name {
			t.Errorf("%s is not sorted: %q comes after %q", brokerCapabilitiesFieldsFixture, name, fixture.Fields[i-1])
		}
	}

	// runtimebroker.BrokerCapabilities also declares exec. The hub sends
	// store.BrokerCapabilities, which does not carry exec, so exec is not
	// part of the hub API and is not in the fixture.
	structNames, problems := structJSONFields(reflect.TypeOf(BrokerCapabilities{}))
	for _, p := range problems {
		t.Error(p)
	}
	inStruct := map[string]bool{}
	for _, name := range structNames {
		inStruct[name] = true
	}

	var missing, extra []string
	for name := range inStruct {
		if !inFixture[name] {
			missing = append(missing, name)
		}
	}
	for name := range inFixture {
		if !inStruct[name] {
			extra = append(extra, name)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 {
		t.Errorf("BrokerCapabilities JSON fields missing from %s: %v (add them, then declare or allow-list them in broker-capabilities-contract.test.ts)", brokerCapabilitiesFieldsFixture, missing)
	}
	if len(extra) > 0 {
		t.Errorf("%s lists fields BrokerCapabilities does not have: %v", brokerCapabilitiesFieldsFixture, extra)
	}
}
