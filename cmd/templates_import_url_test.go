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
	"encoding/json"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeTemplateImportURLService struct {
	hubclient.TemplateService
	lastImportReq *hubclient.ImportResourcesRequest
	importResp    *hubclient.ImportResourcesResponse
	importErr     error
}

func (f *fakeTemplateImportURLService) ImportResources(_ context.Context, req *hubclient.ImportResourcesRequest) (*hubclient.ImportResourcesResponse, error) {
	f.lastImportReq = req
	return f.importResp, f.importErr
}

func TestExecuteTemplatesImportURL_Success(t *testing.T) {
	svc := &fakeTemplateImportURLService{
		importResp: &hubclient.ImportResourcesResponse{
			Kind:     "template",
			Imported: []string{"alpha", "beta"},
			Count:    2,
		},
	}

	out := captureStdout(t, func() {
		err := executeTemplatesImportURL(context.Background(), svc, "project", "proj-1", "https://github.com/org/repo/tree/main/.scion/templates", []string{"alpha", "beta"})
		require.NoError(t, err)
	})

	require.NotNil(t, svc.lastImportReq)
	assert.Equal(t, "template", svc.lastImportReq.Kind)
	assert.Equal(t, "project", svc.lastImportReq.Scope)
	assert.Equal(t, "proj-1", svc.lastImportReq.ScopeID)
	assert.Equal(t, []string{"alpha", "beta"}, svc.lastImportReq.Names)
	assert.Contains(t, out, "Imported: alpha")
	assert.Contains(t, out, "Imported: beta")
	assert.Contains(t, out, "Imported 2 template(s)")
}

func TestExecuteTemplatesImportURL_PartialFailureTextAndJSON(t *testing.T) {
	svc := &fakeTemplateImportURLService{
		importResp: &hubclient.ImportResourcesResponse{
			Kind:     "template",
			Imported: []string{"good"},
			Count:    1,
			Failed: []hubclient.ResourceImportFailure{
				{Name: "bad", Reason: "template exists; agents can only create new templates"},
			},
		},
	}

	t.Run("text", func(t *testing.T) {
		prev := outputFormat
		outputFormat = ""
		t.Cleanup(func() { outputFormat = prev })

		var runErr error
		out := captureStdout(t, func() {
			runErr = executeTemplatesImportURL(context.Background(), svc, "project", "proj-1", "https://github.com/org/repo/tree/main/.scion/templates", nil)
		})
		require.Error(t, runErr)
		assert.Contains(t, out, "Imported: good")
		assert.Contains(t, out, "bad: template exists; agents can only create new templates")
		assert.Contains(t, out, "1 failed")
	})

	t.Run("json", func(t *testing.T) {
		prev := outputFormat
		outputFormat = "json"
		t.Cleanup(func() { outputFormat = prev })

		var runErr error
		out := captureStdout(t, func() {
			runErr = executeTemplatesImportURL(context.Background(), svc, "project", "proj-1", "https://github.com/org/repo/tree/main/.scion/templates", nil)
		})
		require.Error(t, runErr)

		var got hubclient.ImportResourcesResponse
		require.NoError(t, json.Unmarshal([]byte(out), &got))
		assert.Equal(t, "template", got.Kind)
		assert.Equal(t, []string{"good"}, got.Imported)
		assert.Equal(t, 1, got.Count)
		require.Len(t, got.Failed, 1)
		assert.Equal(t, "bad", got.Failed[0].Name)
		assert.Equal(t, "template exists; agents can only create new templates", got.Failed[0].Reason)
	})
}

func TestAnnotateImportURLError(t *testing.T) {
	err401 := &apiclient.APIError{StatusCode: http.StatusUnauthorized, Code: "unauthorized", Message: "Authentication required"}
	got := annotateImportURLError(err401, "global", true)
	assert.Contains(t, got.Error(), "global catalog writes need a human session or hub token")

	err403 := &apiclient.APIError{StatusCode: http.StatusForbidden, Code: "forbidden", Message: "denied"}
	got = annotateImportURLError(err403, "project", false)
	assert.Contains(t, got.Error(), `check your permissions for scope "project"`)
}
