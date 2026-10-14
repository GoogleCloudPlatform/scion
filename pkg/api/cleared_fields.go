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

package api

import (
	"reflect"
	"strings"
)

// ClearedEnvPathPrefix prefixes a ScionConfig.ClearedFields entry that names
// one env key: "env.FOO" clears the agent's own value for FOO.
const ClearedEnvPathPrefix = "env."

// clearableConfigKeys are the top-level ScionConfig json keys a
// ClearedFields entry may name: the limits, thinking level, resources and
// system prompt. Each is a key whose inherited value a start re-derives from
// the template, so removing the agent's own value restores it. Other keys
// are left out until a test shows their inherited value comes back after a
// clear: the harness-config layer (model, image, command args, task flag,
// harness auth, kubernetes pull policy) is written into the agent's config
// at provision and not re-applied at start, so clearing those would drop
// the harness config's value instead of restoring it. Collections are not
// cleared wholesale: env is cleared per key (ClearedEnvPathPrefix), and
// removing a whole volume list could drop mounts the broker added itself,
// such as the workspace. Telemetry is left out: its TLS cert and key files
// must clear together.
var clearableConfigKeys = map[string]bool{
	"max_turns":       true,
	"max_model_calls": true,
	"max_duration":    true,
	"thinking_level":  true,
	"resources":       true,
	"system_prompt":   true,
}

// ClearableConfigPath reports whether path is a ScionConfig.ClearedFields
// entry a Runtime Broker applies: a key in clearableConfigKeys, or
// ClearedEnvPathPrefix followed by a non-empty env key.
func ClearableConfigPath(path string) bool {
	if k, ok := strings.CutPrefix(path, ClearedEnvPathPrefix); ok {
		return k != ""
	}
	return clearableConfigKeys[path]
}

// scionConfigFieldIndex indexes ScionConfig's fields by json key.
var scionConfigFieldIndex = func() map[string]int {
	m := map[string]int{}
	typ := reflect.TypeOf(ScionConfig{})
	for i := 0; i < typ.NumField(); i++ {
		name, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
		if name != "" && name != "-" {
			m[name] = i
		}
	}
	return m
}()

// ApplyClearedFields applies the cleared paths to cfg, in place. An env path
// removes that key from cfg.Env (a template's env is applied at start as a
// final fill, so it is never copied here). Any other path takes its value
// from inherit when inherit is non-nil, and is zeroed otherwise, so the
// value the start derives under cfg applies. A path ClearableConfigPath
// does not accept is left alone and returned in ignored. Applying the same
// paths again gives the same result.
func ApplyClearedFields(cfg *ScionConfig, paths []string, inherit *ScionConfig) (ignored []string) {
	if cfg == nil {
		return nil
	}
	v := reflect.ValueOf(cfg).Elem()
	for _, p := range paths {
		if !ClearableConfigPath(p) {
			ignored = append(ignored, p)
			continue
		}
		if k, ok := strings.CutPrefix(p, ClearedEnvPathPrefix); ok {
			delete(cfg.Env, k)
			continue
		}
		i, ok := scionConfigFieldIndex[p]
		if !ok {
			ignored = append(ignored, p)
			continue
		}
		f := v.Field(i)
		if inherit != nil {
			f.Set(reflect.ValueOf(inherit).Elem().Field(i))
		} else {
			f.Set(reflect.Zero(f.Type()))
		}
	}
	return ignored
}
