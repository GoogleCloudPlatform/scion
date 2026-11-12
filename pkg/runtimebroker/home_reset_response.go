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

package runtimebroker

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// homeResetErrorWriter wraps the response of a create request with
// resetHome (ptone/scion#7757) so that every error answer, whatever path
// wrote it and whatever its status, says whether the agent's home was
// moved aside: details[api.BrokerErrDetailHomeReset] is
// api.BrokerHomeResetApplied once markApplied was called, else
// api.BrokerHomeResetIntact. Only Manager.Reprovision moves the home, and
// it marks every failure after the move with agent.ErrHomeResetApplied, so
// the one markApplied call there makes the broker the single source of
// truth. Success answers pass through unchanged; an error answer is
// buffered until finish, which must run before the handler returns.
type homeResetErrorWriter struct {
	http.ResponseWriter
	applied   bool
	unknown   bool
	status    int
	buffering bool
	buf       bytes.Buffer
}

func newHomeResetErrorWriter(w http.ResponseWriter) *homeResetErrorWriter {
	return &homeResetErrorWriter{ResponseWriter: w}
}

// markApplied records that the home was moved aside. Safe on nil.
func (w *homeResetErrorWriter) markApplied() {
	if w != nil {
		w.applied = true
	}
}

// markUnknown records that this answer cannot say whether the home was
// moved (another attempt of the same request is still running): the
// detail is left out, so the hub treats the answer as a reset. Safe on nil.
func (w *homeResetErrorWriter) markUnknown() {
	if w != nil {
		w.unknown = true
	}
}

func (w *homeResetErrorWriter) WriteHeader(code int) {
	if code >= http.StatusBadRequest {
		w.buffering, w.status = true, code
		return
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *homeResetErrorWriter) Write(b []byte) (int, error) {
	if w.buffering {
		return w.buf.Write(b)
	}
	return w.ResponseWriter.Write(b)
}

// finish writes a buffered error answer with the home reset detail added.
// A body that is not the broker's JSON error envelope is written as it was
// (the hub then treats the answer as ambiguous, that is, as a reset).
func (w *homeResetErrorWriter) finish() {
	if !w.buffering {
		return
	}
	w.buffering = false
	body := w.buf.Bytes()
	var resp ErrorResponse
	if err := json.Unmarshal(body, &resp); err == nil && !w.unknown {
		if resp.Error.Details == nil {
			resp.Error.Details = map[string]interface{}{}
		}
		state := api.BrokerHomeResetIntact
		if w.applied {
			state = api.BrokerHomeResetApplied
		}
		resp.Error.Details[api.BrokerErrDetailHomeReset] = state
		if out, err := json.Marshal(resp); err == nil {
			body = append(out, '\n')
		}
	}
	w.ResponseWriter.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.ResponseWriter.WriteHeader(w.status)
	_, _ = w.ResponseWriter.Write(body)
}
