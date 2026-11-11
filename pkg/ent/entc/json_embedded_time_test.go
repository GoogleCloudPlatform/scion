// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package entc

import (
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
)

// jsonEmbeddedTimeAllowlist lists every time.Time that sits inside a
// JSON-typed (or other non-scalar) ent field, keyed by its path, with the
// reason it is safe.
//
// UTCTimeHook normalises only top-level time.Time fields; it cannot see a time
// embedded in a JSON value, which keeps whatever offset its writer gave it and
// is encoded with that offset. Every such time must therefore be converted to
// UTC by its writer. Adding a new embedded time.Time fails
// TestJSONEmbeddedTimesAreAllowlisted until the new path is normalised on
// every write and listed here.
var jsonEmbeddedTimeAllowlist = map[string]string{
	// Normalised on every write by entadapter's utcExposedPorts (CreateAgent
	// and UpdateAgentExposedPorts are the only writers of exposed_ports).
	"Agent.ExposedPorts[].ExposedAt": "normalised in entadapter utcExposedPorts",

	// access_policies.conditions has no writer: the policy API returns 410
	// and the legacy policy store was removed. Any new writer must convert
	// these to UTC before calling SetConditions.
	"AccessPolicy.Conditions.ValidFrom":  "no write path; a new writer must normalise",
	"AccessPolicy.Conditions.ValidUntil": "no write path; a new writer must normalise",
}

var timeType = reflect.TypeOf(time.Time{})

// entEntityTypes returns every generated ent entity struct type, found via
// the Get method of each *XxxClient field of ent.Client, keyed by entity name.
func entEntityTypes(t *testing.T) map[string]reflect.Type {
	t.Helper()
	out := map[string]reflect.Type{}
	ct := reflect.TypeOf(ent.Client{})
	for i := 0; i < ct.NumField(); i++ {
		f := ct.Field(i)
		if !f.IsExported() || f.Type.Kind() != reflect.Pointer || !strings.HasSuffix(f.Type.Elem().Name(), "Client") {
			continue
		}
		get, ok := f.Type.MethodByName("Get")
		if !ok {
			continue
		}
		// Method type includes the receiver: (recv, ctx, id) (*Entity, error).
		if get.Type.NumOut() != 2 || get.Type.Out(0).Kind() != reflect.Pointer {
			t.Fatalf("%s.Get has an unexpected signature: %s", f.Name, get.Type)
		}
		et := get.Type.Out(0).Elem()
		if et.Kind() != reflect.Struct {
			t.Fatalf("%s.Get returns a non-struct entity: %s", f.Name, et)
		}
		out[et.Name()] = et
	}
	return out
}

// embeddedTimePaths returns the paths of every time.Time nested inside a
// non-scalar field of entity type et. Top-level time.Time and *time.Time
// fields are scalar columns, covered by UTCTimeHook, and are skipped, as are
// unexported fields and the generated Edges struct (which only points at
// other entities, each walked on its own).
func embeddedTimePaths(entity string, et reflect.Type) []string {
	var paths []string
	for i := 0; i < et.NumField(); i++ {
		f := et.Field(i)
		if !f.IsExported() || f.Name == "Edges" {
			continue
		}
		ft := f.Type
		if ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if ft == timeType {
			continue
		}
		walkForTimes(ft, entity+"."+f.Name, map[reflect.Type]bool{}, &paths)
	}
	return paths
}

// isTimeType reports whether t is time.Time or a struct type defined from it
// (e.g. "type Stamp time.Time"), which carries the same wall clock and
// location and so the same offset hazard.
func isTimeType(t reflect.Type) bool {
	return t == timeType || (t.Kind() == reflect.Struct && t.ConvertibleTo(timeType))
}

func walkForTimes(t reflect.Type, path string, onStack map[reflect.Type]bool, paths *[]string) {
	if isTimeType(t) {
		*paths = append(*paths, path)
		return
	}
	// Guard every composite kind, not only structs: a recursive slice, map
	// or pointer type (e.g. "type rec []rec") never passes through a struct.
	if onStack[t] {
		return // recursive type; already being walked further up
	}
	onStack[t] = true
	defer delete(onStack, t)
	switch t.Kind() {
	case reflect.Pointer:
		walkForTimes(t.Elem(), path, onStack, paths)
	case reflect.Slice, reflect.Array:
		walkForTimes(t.Elem(), path+"[]", onStack, paths)
	case reflect.Map:
		walkForTimes(t.Key(), path+"{key}", onStack, paths)
		walkForTimes(t.Elem(), path+"{}", onStack, paths)
	case reflect.Struct:
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() && !f.Anonymous {
				continue // encoding/json ignores unexported fields
			}
			walkForTimes(f.Type, path+"."+f.Name, onStack, paths)
		}
	}
	// Interface-typed values (e.g. map[string]any) are opaque to reflection
	// on the type; times decoded into them come back as strings, not
	// time.Time, so they cannot carry a stored offset as a time.Time.
}

// TestJSONEmbeddedTimesAreAllowlisted fails when any ent entity gains a
// time.Time inside a JSON-typed field that is not on the allowlist, and when
// an allowlist entry no longer matches a real field.
func TestJSONEmbeddedTimesAreAllowlisted(t *testing.T) {
	entities := entEntityTypes(t)
	if len(entities) < 10 {
		t.Fatalf("found only %d ent entity types; enumeration via ent.Client is broken", len(entities))
	}

	found := map[string]bool{}
	for name, et := range entities {
		for _, p := range embeddedTimePaths(name, et) {
			found[p] = true
		}
	}

	var unlisted, stale []string
	for p := range found {
		if _, ok := jsonEmbeddedTimeAllowlist[p]; !ok {
			unlisted = append(unlisted, p)
		}
	}
	for p := range jsonEmbeddedTimeAllowlist {
		if !found[p] {
			stale = append(stale, p)
		}
	}
	sort.Strings(unlisted)
	sort.Strings(stale)
	for _, p := range unlisted {
		t.Errorf("time.Time embedded in a JSON ent field is not normalised to UTC: %s. "+
			"Convert it with .UTC() on every write, then add it to jsonEmbeddedTimeAllowlist.", p)
	}
	for _, p := range stale {
		t.Errorf("jsonEmbeddedTimeAllowlist entry %q matches no ent field; remove it", p)
	}
}

// TestEmbeddedTimePathsDetectsNewField checks that the walker reports a
// time.Time nested at any depth inside a JSON-shaped field, and skips scalar
// time columns.
func TestEmbeddedTimePathsDetectsNewField(t *testing.T) {
	type inner struct {
		At    time.Time
		AtPtr *time.Time
		Name  string
	}
	type stamp time.Time
	type recSlice []recSlice
	type recMap map[string]recMap
	type recPtr *recPtr
	type recursive struct {
		When time.Time
		Next *recursive
	}
	type fakeEntity struct {
		ID        int
		Created   time.Time  // scalar column: skipped
		DeletedAt *time.Time // nillable scalar column: skipped
		Payload   *inner
		List      []inner
		ByName    map[string]inner
		Chain     recursive
		Stamps    []stamp
		RecSlice  recSlice // recursive non-struct types: must terminate, report nothing
		RecMap    recMap
		RecPtr    recPtr
		Edges     struct{ Other *inner }
		hidden    inner //nolint:unused // proves unexported fields are skipped
	}

	got := embeddedTimePaths("Fake", reflect.TypeOf(fakeEntity{}))
	sort.Strings(got)
	want := []string{
		"Fake.ByName{}.At",
		"Fake.ByName{}.AtPtr",
		"Fake.Chain.When",
		"Fake.List[].At",
		"Fake.List[].AtPtr",
		"Fake.Payload.At",
		"Fake.Payload.AtPtr",
		"Fake.Stamps[]",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("embeddedTimePaths = %v, want %v", got, want)
	}
}
