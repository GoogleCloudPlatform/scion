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

// Package envelope maps the control channel's wsprotocol request and
// response envelopes onto conduit RpcRequest and RpcResponse frames, one
// to one: method, path, query, headers and body for requests; status,
// headers and body for responses. The hub and the Runtime Broker both use
// it, so a request tunnelled over a conduit session reaches the broker's
// HTTP handlers exactly as it would over the control channel.
//
// Both shapes carry one string per header name. Empty header maps and
// empty bodies map to nil, as they do through the control channel's JSON
// encoding (omitempty). Every string (request id, method, path, query,
// header names and values) gets UTF-8 normalisation for parity with the
// JSON envelope: invalid bytes become U+FFFD.
package envelope

import (
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// RequestToRPC converts a request envelope to an RpcRequest.
func RequestToRPC(req *wsprotocol.RequestEnvelope) *conduitv1.RpcRequest {
	return &conduitv1.RpcRequest{
		RequestId: validUTF8(req.RequestID),
		Method:    validUTF8(req.Method),
		Path:      validUTF8(req.Path),
		Query:     validUTF8(req.Query),
		Headers:   cloneHeaders(req.Headers),
		Body:      cloneBody(req.Body),
	}
}

// RPCToRequest converts an RpcRequest to a request envelope.
func RPCToRequest(req *conduitv1.RpcRequest) *wsprotocol.RequestEnvelope {
	return wsprotocol.NewRequestEnvelope(validUTF8(req.GetRequestId()), validUTF8(req.GetMethod()), validUTF8(req.GetPath()), validUTF8(req.GetQuery()),
		cloneHeaders(req.GetHeaders()), cloneBody(req.GetBody()))
}

// ResponseToRPC converts a response envelope to an RpcResponse.
func ResponseToRPC(resp *wsprotocol.ResponseEnvelope) *conduitv1.RpcResponse {
	return &conduitv1.RpcResponse{
		RequestId: validUTF8(resp.RequestID),
		Status:    int32(resp.StatusCode),
		Headers:   cloneHeaders(resp.Headers),
		Body:      cloneBody(resp.Body),
	}
}

// RPCToResponse converts an RpcResponse to a response envelope.
func RPCToResponse(resp *conduitv1.RpcResponse) *wsprotocol.ResponseEnvelope {
	return wsprotocol.NewResponseEnvelope(validUTF8(resp.GetRequestId()), int(resp.GetStatus()),
		cloneHeaders(resp.GetHeaders()), cloneBody(resp.GetBody()))
}

// validUTF8 is the UTF-8 normalisation for parity with the JSON
// envelope: invalid bytes in s become U+FFFD.
func validUTF8(s string) string { return strings.ToValidUTF8(s, "\uFFFD") }

// cloneHeaders copies h with UTF-8 normalisation of every name and
// value.
func cloneHeaders(h map[string]string) map[string]string {
	if len(h) == 0 {
		return nil
	}
	out := make(map[string]string, len(h))
	for k, v := range h {
		out[validUTF8(k)] = validUTF8(v)
	}
	return out
}

func cloneBody(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	return append([]byte(nil), b...)
}
