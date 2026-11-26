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

//go:build !no_sqlite

package hub

import (
	"context"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The store maps unique violations to store.ErrAlreadyExists, so a duplicate
// create must answer 409 with the handler's specific message rather than the
// generic "Resource already exists" from writeErrorFromErr.

func TestCreateSkill_DuplicateSlugConflictMessage(t *testing.T) {
	srv, s, alice, _, _ := setupSkillAuthzTest(t)
	srv.SetStorage(newMockStorage("dup-skill-storage"))
	bindGlobalCatalogAuthor(t, s, alice.ID)

	req := CreateSkillRequest{Name: "dup-skill", Scope: store.SkillScopeGlobal}
	first := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/skills", req)
	require.Equal(t, http.StatusCreated, first.Code, first.Body.String())

	second := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/skills", req)
	assert.Equal(t, http.StatusConflict, second.Code, second.Body.String())
	assert.Contains(t, second.Body.String(), "A skill with this slug already exists in the target scope")
}

func TestMultipartPublish_DraftVersionConflictMessage(t *testing.T) {
	srv, s, alice, skill := setupMultipartTest(t)

	// A draft (not published) version passes the immutability check, so the
	// publish reaches CreateSkillVersion and hits the unique (skill_id, version) key.
	require.NoError(t, s.CreateSkillVersion(context.Background(), &store.SkillVersion{
		ID:      api.NewUUID(),
		SkillID: skill.ID,
		Version: "1.0.0",
		Status:  store.SkillVersionStatusDraft,
	}))

	rec := doMultipartRequestAsUser(t, srv, alice, http.MethodPost,
		"/api/v1/skills/"+skill.ID+"/versions",
		map[string]string{"version": "1.0.0"},
		map[string][]byte{"SKILL.md": []byte("---\nname: test\n---\n# Test")},
	)
	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "version 1.0.0 already exists for this skill")
}

func TestCreateUserTemplate_DuplicateNameConflictMessage(t *testing.T) {
	srv, _, alice, _ := setupUserTemplateTest(t)

	body := CreateTemplateRequest{Name: "dup-template", Harness: "antigravity"}
	first := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/users/me/templates", body)
	require.Equal(t, http.StatusCreated, first.Code, first.Body.String())

	second := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/users/me/templates", body)
	assert.Equal(t, http.StatusConflict, second.Code, second.Body.String())
	assert.Contains(t, second.Body.String(), "A template with this name already exists in your user scope")
}
