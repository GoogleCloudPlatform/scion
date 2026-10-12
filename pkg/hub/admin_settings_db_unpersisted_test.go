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

package hub

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// fillNonZero sets v to a non-zero value: strings "x", numbers 1, bools true,
// one-element slices and maps, and every exported field of a struct.
func fillNonZero(v reflect.Value, depth int) {
	if depth > 6 || !v.CanSet() {
		return
	}
	switch v.Kind() {
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		fillNonZero(v.Elem(), depth+1)
	case reflect.String:
		v.SetString("x")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(1)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(1)
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 1, 1)
		fillNonZero(s.Index(0), depth+1)
		v.Set(s)
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		k := reflect.New(v.Type().Key()).Elem()
		fillNonZero(k, depth+1)
		e := reflect.New(v.Type().Elem()).Elem()
		fillNonZero(e, depth+1)
		m.SetMapIndex(k, e)
		v.Set(m)
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).IsExported() {
				fillNonZero(v.Field(i), depth+1)
			}
		}
	}
}

func jsonName(f reflect.StructField) string {
	tag := f.Tag.Get("json")
	name, _, _ := strings.Cut(tag, ",")
	if tag == "-" {
		return ""
	}
	if name == "" {
		return f.Name
	}
	return name
}

// TestDBUnpersistedRequestPaths_CoverUnmappedFields keeps
// dbUnpersistedRequestPaths in step with the request types: every request
// field that extractKoanfKeysFromRequest maps to no koanf key must be listed
// (otherwise the DB-backed PUT would drop it and still report "saved"), and
// no mapped field may be listed. server.hub and server.auth are checked per
// field because they are mapped field by field.
func TestDBUnpersistedRequestPaths_CoverUnmappedFields(t *testing.T) {
	listed := map[string]bool{}
	for _, p := range dbUnpersistedRequestPaths {
		listed[strings.Join(p, ".")] = true
	}

	type probe struct {
		path string
		set  func(*ServerConfigUpdateRequest)
	}
	var probes []probe

	reqT := reflect.TypeOf(ServerConfigUpdateRequest{})
	for i := 0; i < reqT.NumField(); i++ {
		f := reqT.Field(i)
		name := jsonName(f)
		if name == "" || name == "server" {
			continue
		}
		idx := i
		probes = append(probes, probe{name, func(r *ServerConfigUpdateRequest) {
			fillNonZero(reflect.ValueOf(r).Elem().Field(idx), 0)
		}})
	}

	srvT := reflect.TypeOf(config.V1ServerConfig{})
	for i := 0; i < srvT.NumField(); i++ {
		f := srvT.Field(i)
		name := jsonName(f)
		if name == "" {
			continue
		}
		idx := i
		switch name {
		case "hub", "auth":
			sub := f.Type.Elem()
			for j := 0; j < sub.NumField(); j++ {
				sf := sub.Field(j)
				sname := jsonName(sf)
				if sname == "" {
					continue
				}
				jdx := j
				probes = append(probes, probe{"server." + name + "." + sname, func(r *ServerConfigUpdateRequest) {
					r.Server = &config.V1ServerConfig{}
					pv := reflect.ValueOf(r.Server).Elem().Field(idx)
					pv.Set(reflect.New(sub))
					fillNonZero(pv.Elem().Field(jdx), 0)
				}})
			}
		default:
			probes = append(probes, probe{"server." + name, func(r *ServerConfigUpdateRequest) {
				r.Server = &config.V1ServerConfig{}
				fillNonZero(reflect.ValueOf(r.Server).Elem().Field(idx), 0)
			}})
		}
	}

	var missing, wrong []string
	for _, p := range probes {
		var req ServerConfigUpdateRequest
		p.set(&req)
		mapped := len(extractKoanfKeysFromRequest(&req)) > 0
		switch {
		case !mapped && !listed[p.path]:
			missing = append(missing, p.path)
		case mapped && listed[p.path]:
			wrong = append(wrong, p.path)
		}
	}
	sort.Strings(missing)
	sort.Strings(wrong)
	if len(missing) > 0 {
		t.Errorf("request fields mapped to no koanf key and missing from dbUnpersistedRequestPaths "+
			"(the DB-backed PUT would drop them silently): %v", missing)
	}
	if len(wrong) > 0 {
		t.Errorf("dbUnpersistedRequestPaths lists fields that extractKoanfKeysFromRequest maps: %v", wrong)
	}
}

func TestUnknownJSONPaths(t *testing.T) {
	body := `{
		"default_template": "x",
		"Default_Model": "case-insensitive match is known",
		"server.hub.auto_suspend_stalled": true,
		"server": {"hub": {"port": 1, "bogus": 2}},
		"profiles": {"p.q": {"runtime": "docker", "nope": 1}},
		"expected_revisions": {"access": 1}
	}`
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &top); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range unknownJSONPaths(top, reflect.TypeOf(ServerConfigUpdateDBRequest{}), nil) {
		got = append(got, strings.Join(p.path, "|"))
	}
	sort.Strings(got)
	want := []string{"profiles|p.q|nope", "server.hub.auto_suspend_stalled", "server|hub|bogus"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("unknownJSONPaths = %v, want %v", got, want)
	}
}

func TestIsEmptySettingsBody(t *testing.T) {
	for body, want := range map[string]bool{
		`{}`:                                  true,
		`{"expected_revisions":{"access":1}}`: true,
		`{"quotas":{}}`:                       false,
		`not json`:                            false,
	} {
		if got := isEmptySettingsBody([]byte(body)); got != want {
			t.Errorf("isEmptySettingsBody(%s) = %v, want %v", body, got, want)
		}
	}
}
