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

import "testing"

func TestTemplateSlugAcceptsOneComponent(t *testing.T) {
	for _, v := range []string{
		"default",
		"code-reviewer",
		"Mixed_Case.v2",
		"3f2a9c1e-7b4d-4e8a-9f10-2c3d4e5f6a7b",
	} {
		if !IsTemplateSlug(v) {
			t.Errorf("IsTemplateSlug(%q) = false, want true", v)
		}
	}
	for _, v := range []string{
		"",
		".",
		"..",
		"a/b",
		"../x",
		"./x",
		"/abs/dir",
		`a\b`,
		`..\x`,
		`C:\x`,
		":local:/x",
		":gcs:bucket/x",
		":s3,env_auth:b",
		"https://example.com/t",
		"http://example.com/t",
		"git://host/repo",
		"x\x00y",
		"\xff\xfe",
	} {
		if IsTemplateSlug(v) {
			t.Errorf("IsTemplateSlug(%q) = true, want false", v)
		}
	}
}
