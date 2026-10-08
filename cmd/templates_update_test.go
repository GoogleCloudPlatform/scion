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

package cmd

import (
	"context"
	"errors"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeTemplateUpdateService implements the TemplateService calls used by
// templates update and records reimport requests.
type fakeTemplateUpdateService struct {
	hubclient.TemplateService
	templates   []hubclient.Template
	listOpts    []hubclient.ListTemplatesOptions
	reimported  []string
	overrides   []string
	reimportErr map[string]error
}

func (f *fakeTemplateUpdateService) List(_ context.Context, opts *hubclient.ListTemplatesOptions) (*hubclient.ListTemplatesResponse, error) {
	f.listOpts = append(f.listOpts, *opts)
	var out []hubclient.Template
	for _, t := range f.templates {
		if opts.Name != "" && t.Name != opts.Name && t.Slug != opts.Name {
			continue
		}
		if opts.Scope != "" && t.Scope != opts.Scope {
			continue
		}
		out = append(out, t)
	}
	return &hubclient.ListTemplatesResponse{Templates: out}, nil
}

func (f *fakeTemplateUpdateService) Reimport(_ context.Context, id, sourceURL string) (*hubclient.ReimportTemplateResponse, error) {
	f.reimported = append(f.reimported, id)
	f.overrides = append(f.overrides, sourceURL)
	if err := f.reimportErr[id]; err != nil {
		return nil, err
	}
	return &hubclient.ReimportTemplateResponse{Templates: []string{id}, Count: 1}, nil
}

const ghSource = "https://github.com/acme/repo/tree/main/.scion/templates/my-template"

func TestUpdateSingleTemplate(t *testing.T) {
	t.Run("reimports from stored source", func(t *testing.T) {
		svc := &fakeTemplateUpdateService{templates: []hubclient.Template{
			{ID: "t1", Name: "my-template", Slug: "my-template", Scope: "global", SourceURL: ghSource},
		}}
		require.NoError(t, updateSingleTemplate(context.Background(), svc, "my-template", "", ""))
		assert.Equal(t, []string{"t1"}, svc.reimported)
		assert.Equal(t, []string{""}, svc.overrides)
		assert.Equal(t, "active", svc.listOpts[0].Status)
	})

	t.Run("passes url override", func(t *testing.T) {
		svc := &fakeTemplateUpdateService{templates: []hubclient.Template{
			{ID: "t1", Name: "my-template", Scope: "global"},
		}}
		require.NoError(t, updateSingleTemplate(context.Background(), svc, "my-template", ghSource, ""))
		assert.Equal(t, []string{ghSource}, svc.overrides)
	})

	t.Run("not found", func(t *testing.T) {
		svc := &fakeTemplateUpdateService{}
		err := updateSingleTemplate(context.Background(), svc, "missing", "", "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not found")
		assert.Empty(t, svc.reimported)
	})

	t.Run("no stored source", func(t *testing.T) {
		svc := &fakeTemplateUpdateService{templates: []hubclient.Template{
			{ID: "t1", Name: "my-template", Scope: "global"},
		}}
		err := updateSingleTemplate(context.Background(), svc, "my-template", "", "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no stored source URL")
		assert.Empty(t, svc.reimported)
	})

	t.Run("built-in template is refused", func(t *testing.T) {
		svc := &fakeTemplateUpdateService{templates: []hubclient.Template{
			{ID: "t1", Name: "default", Scope: "global", SourceURL: "builtin://scion/1.0/template/default"},
		}}
		err := updateSingleTemplate(context.Background(), svc, "default", "", "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cannot be refreshed")
		assert.Empty(t, svc.reimported)
	})

	t.Run("ambiguous across scopes needs --scope", func(t *testing.T) {
		svc := &fakeTemplateUpdateService{templates: []hubclient.Template{
			{ID: "g", Name: "my-template", Scope: "global", SourceURL: ghSource},
			{ID: "p", Name: "my-template", Scope: "project", SourceURL: ghSource},
		}}
		err := updateSingleTemplate(context.Background(), svc, "my-template", "", "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--scope")
		assert.Empty(t, svc.reimported)

		require.NoError(t, updateSingleTemplate(context.Background(), svc, "my-template", "", "project"))
		assert.Equal(t, []string{"p"}, svc.reimported)
	})

	t.Run("hub error is returned", func(t *testing.T) {
		svc := &fakeTemplateUpdateService{
			templates:   []hubclient.Template{{ID: "t1", Name: "my-template", Scope: "global", SourceURL: ghSource}},
			reimportErr: map[string]error{"t1": errors.New("unsupported_source")},
		}
		err := updateSingleTemplate(context.Background(), svc, "my-template", "", "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "reimport failed")
	})
}

func TestUpdateAllTemplates(t *testing.T) {
	svc := &fakeTemplateUpdateService{
		templates: []hubclient.Template{
			{ID: "a", Name: "a", Scope: "global", SourceURL: ghSource},
			{ID: "builtin", Name: "default", Scope: "global", SourceURL: "builtin://scion/1.0/template/default"},
			{ID: "none", Name: "local", Scope: "project"},
			{ID: "b", Name: "b", Scope: "project", SourceURL: ghSource},
			{ID: "bad", Name: "bad", Scope: "user", SourceURL: ghSource},
		},
		reimportErr: map[string]error{"bad": errors.New("boom")},
	}
	err := updateAllTemplates(context.Background(), svc, "")
	require.Error(t, err, "a failed reimport makes the command fail")
	assert.Equal(t, []string{"a", "b", "bad"}, svc.reimported, "templates without an https source are skipped")
	for _, o := range svc.overrides {
		assert.Empty(t, o)
	}
}

func TestDisplaySourceURL_DropsCredentials(t *testing.T) {
	assert.Equal(t, "https://github.com/acme/repo", displaySourceURL("https://user:secret@github.com/acme/repo"))
	assert.Equal(t, ghSource, displaySourceURL(ghSource))
}

func TestTemplatesUpdate_Usage(t *testing.T) {
	for name, args := range map[string][]string{
		"no name":     {},
		"all and url": {"--all", "--url", ghSource},
		"bad scope":   {"x", "--scope", "galaxy"},
	} {
		t.Run(name, func(t *testing.T) {
			cmd := templatesUpdateCmd
			require.NoError(t, cmd.Flags().Parse(args))
			defer func() {
				_ = cmd.Flags().Set("all", "false")
				_ = cmd.Flags().Set("url", "")
				_ = cmd.Flags().Set("scope", "")
			}()
			err := runTemplatesUpdate(cmd, cmd.Flags().Args())
			require.Error(t, err)
		})
	}
}
