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

package portforward

import "net/http"

const (
	MessageTypeRequest  = "request"
	MessageTypeResponse = "response"
	MessageTypeError    = "error"
)

type Request struct {
	StreamID string      `json:"streamId"`
	Port     int         `json:"port"`
	Host     string      `json:"host"`
	Method   string      `json:"method"`
	Path     string      `json:"path"`
	Query    string      `json:"query,omitempty"`
	Header   http.Header `json:"header,omitempty"`
	Body     []byte      `json:"body,omitempty"`
}

type Response struct {
	StreamID string      `json:"streamId"`
	Status   int         `json:"status"`
	Header   http.Header `json:"header,omitempty"`
	Body     []byte      `json:"body,omitempty"`
	Error    string      `json:"error,omitempty"`
}

type Message struct {
	Type     string    `json:"type"`
	Request  *Request  `json:"request,omitempty"`
	Response *Response `json:"response,omitempty"`
}
