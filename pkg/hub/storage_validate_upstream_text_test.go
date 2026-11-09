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

	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/stretchr/testify/assert"
)

// The storage validation handlers answer a failed ValidateStorage with the
// fixed "validation failed" and the same status (502) and code
// (runtime_error) as before. The error, which can carry the object path and
// the wrapped backend error, is logged once, rendered (ptone/scion#7498).
//
// These tests must not run in parallel: they swap the default logger.

// validateFaultStorage fails every object read and existence check with
// errUpstreamMarker, so ValidateStorage returns an error wrapping it.
type validateFaultStorage struct {
	storage.Storage
}

func (validateFaultStorage) GetObject(context.Context, string) (*storage.Object, error) {
	return nil, errUpstreamMarker
}

func (validateFaultStorage) Exists(context.Context, string) (bool, error) {
	return false, errUpstreamMarker
}

func TestTemplateValidate_StorageErrorFixedText(t *testing.T) {
	srv, s, stor := testTemplateFileServer(t)
	tmpl := createTestTemplate(t, s, stor, map[string]string{"CLAUDE.md": "# Agent"})
	srv.SetStorage(validateFaultStorage{srv.GetStorage()})
	logs := captureUpstreamLog(t)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/templates/"+tmpl.ID+"/validate", nil)

	assertObjectStoreFailureFixed(t, rec, logs, "template validation failed")
	assert.Contains(t, rec.Body.String(), "validation failed")
	assert.NotContains(t, rec.Body.String(), "checking object")
	assert.NotContains(t, rec.Body.String(), "CLAUDE.md")
}

func TestHarnessConfigValidate_StorageErrorFixedText(t *testing.T) {
	srv, _, hc, _ := localStorageHarnessConfig(t, []string{"config.yaml"}, []string{"config.yaml"})
	srv.SetStorage(validateFaultStorage{srv.GetStorage()})
	logs := captureUpstreamLog(t)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/harness-configs/"+hc.ID+"/validate", nil)

	assertObjectStoreFailureFixed(t, rec, logs, "harness-config validation failed")
	assert.Contains(t, rec.Body.String(), "validation failed")
	assert.NotContains(t, rec.Body.String(), "checking object")
	assert.NotContains(t, rec.Body.String(), "config.yaml")
}
