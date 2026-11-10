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

package config

import "errors"

// ErrInvalidResourceName is returned when a template or harness-config name
// that is looked up on disk, or used to build a directory, is not exactly
// one local path component (IsTemplateSlug). Its text never contains the
// name.
var ErrInvalidResourceName = errors.New("name must be a single path component")

// CheckResourceName returns ErrInvalidResourceName unless name is a template
// or harness-config name that is exactly one local path component
// (IsTemplateSlug). Callers check it before name is joined under a templates
// or harness-configs directory.
func CheckResourceName(name string) error {
	if !IsTemplateSlug(name) {
		return ErrInvalidResourceName
	}
	return nil
}

// checkLookupName is CheckResourceName for a template or harness-config
// lookup on disk by name. The empty name is not refused: it keeps its
// earlier meaning there (no template is named, or nothing is found), and a
// lookup never joins it into a path outside the templates or harness-configs
// directory. Callers that build a directory from a name (the CLI pulls) use
// CheckResourceName, which refuses it.
func checkLookupName(name string) error {
	if name == "" {
		return nil
	}
	return CheckResourceName(name)
}
