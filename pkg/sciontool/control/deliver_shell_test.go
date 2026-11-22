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

package control

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDeliverEnabled_RefusesShellHarness(t *testing.T) {
	getenv := func(k string) string {
		switch k {
		case EnvHubExperiments:
			return ExperimentConduitDeliver
		case "SCION_HARNESS":
			return "shell"
		default:
			return ""
		}
	}
	if DeliverEnabled(getenv, true) {
		t.Fatal("DeliverEnabled must return false for SCION_HARNESS=shell")
	}
}

func TestDeliverRoute_DisabledRefuses(t *testing.T) {
	route := newDeliverRoute(DeliverOptions{Disabled: true})
	req := httptest.NewRequest(http.MethodPost, "/"+RouteDeliverMessage, bytes.NewBufferString(`{}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	route.serve(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body: %s)", rec.Code, rec.Body.String())
	}
}
