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

package envelope

import (
	"reflect"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// wire sends f through the protobuf encoding, as a session does.
func wire(t *testing.T, f *conduitv1.Frame) *conduitv1.Frame {
	t.Helper()
	b, err := proto.Marshal(f)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out conduitv1.Frame
	if err := proto.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return &out
}

func allBytes() []byte {
	b := make([]byte, 256)
	for i := range b {
		b[i] = byte(i)
	}
	return b
}

func TestRequestRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		req  *wsprotocol.RequestEnvelope
	}{
		{"get without body", wsprotocol.NewRequestEnvelope("r1", "GET", "/api/v1/agents/a1/logs", "tail=100&projectId=p1", map[string]string{"X-Scion-Broker-ID": "b1"}, nil)},
		{"post with json body", wsprotocol.NewRequestEnvelope("r2", "POST", "/api/v1/agents", "", map[string]string{"Content-Type": "application/json"}, []byte(`{"name":"a"}`))},
		{"binary body", wsprotocol.NewRequestEnvelope("r3", "PUT", "/api/v1/blob", "", nil, allBytes())},
		{"multi-value header and escaped query", wsprotocol.NewRequestEnvelope("r4", "DELETE", "/api/v1/agents/a%2Fb", "q=a+b&x=%26&x=2",
			map[string]string{"Accept": "application/json, text/plain", "Traceparent": "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"}, nil)},
		{"empty method path and body", wsprotocol.NewRequestEnvelope("", "", "", "", map[string]string{}, []byte{})},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rpc := RequestToRPC(tc.req)
			f := wire(t, &conduitv1.Frame{Body: &conduitv1.Frame_RpcRequest{RpcRequest: rpc}})
			got := RPCToRequest(f.GetRpcRequest())
			want := *tc.req
			want.Headers = cloneHeaders(want.Headers)
			want.Body = cloneBody(want.Body)
			if !reflect.DeepEqual(got, &want) {
				t.Fatalf("round trip changed the request:\n got  %+v\n want %+v", got, &want)
			}
			// The reverse direction (RPC -> envelope -> RPC) is the
			// identity too.
			back := RequestToRPC(got)
			if !proto.Equal(back, f.GetRpcRequest()) {
				t.Fatalf("reverse round trip changed the RpcRequest:\n got  %v\n want %v", back, f.GetRpcRequest())
			}
		})
	}
}

func TestResponseRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		resp *wsprotocol.ResponseEnvelope
	}{
		{"ok json", wsprotocol.NewResponseEnvelope("r1", 200, map[string]string{"Content-Type": "application/json"}, []byte(`{"ok":true}`))},
		{"no content", wsprotocol.NewResponseEnvelope("r2", 204, nil, nil)},
		{"error with retry-after", wsprotocol.NewResponseEnvelope("r3", 503, map[string]string{"Retry-After": "5", "Vary": "Accept, Origin"}, []byte("unavailable"))},
		{"binary body", wsprotocol.NewResponseEnvelope("r4", 200, nil, allBytes())},
		{"empty header map and body", wsprotocol.NewResponseEnvelope("r5", 413, map[string]string{}, []byte{})},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rpc := ResponseToRPC(tc.resp)
			f := wire(t, &conduitv1.Frame{Body: &conduitv1.Frame_RpcResponse{RpcResponse: rpc}})
			got := RPCToResponse(f.GetRpcResponse())
			want := *tc.resp
			want.Headers = cloneHeaders(want.Headers)
			want.Body = cloneBody(want.Body)
			if !reflect.DeepEqual(got, &want) {
				t.Fatalf("round trip changed the response:\n got  %+v\n want %+v", got, &want)
			}
			back := ResponseToRPC(got)
			if !proto.Equal(back, f.GetRpcResponse()) {
				t.Fatalf("reverse round trip changed the RpcResponse:\n got  %v\n want %v", back, f.GetRpcResponse())
			}
		})
	}
}

// TestUTF8Normalisation checks that every string field gets UTF-8
// normalisation for parity with the JSON envelope, in both directions, so
// the frame always encodes.
func TestUTF8Normalisation(t *testing.T) {
	const bad = "caf\xe9"
	const fixed = "caf\uFFFD"
	t.Run("request path and fields", func(t *testing.T) {
		req := wsprotocol.NewRequestEnvelope("id\xff", "GE\xffT", "/api/v1/\xff", "q=\xff", map[string]string{"X-Bad\xff": bad}, nil)
		rpc := RequestToRPC(req)
		f := wire(t, &conduitv1.Frame{Body: &conduitv1.Frame_RpcRequest{RpcRequest: rpc}})
		got := f.GetRpcRequest()
		if got.GetPath() != "/api/v1/\uFFFD" || got.GetMethod() != "GE\uFFFDT" || got.GetQuery() != "q=\uFFFD" || got.GetRequestId() != "id\uFFFD" {
			t.Fatalf("request fields not normalised: %v", got)
		}
		if got.GetHeaders()["X-Bad\uFFFD"] != fixed {
			t.Fatalf("request headers not normalised: %v", got.GetHeaders())
		}
		back := RPCToRequest(&conduitv1.RpcRequest{Path: "/\xff", Headers: map[string]string{"A": bad}})
		if back.Path != "/\uFFFD" || back.Headers["A"] != fixed {
			t.Fatalf("RPCToRequest not normalised: %+v", back)
		}
	})
	t.Run("response header value", func(t *testing.T) {
		resp := wsprotocol.NewResponseEnvelope("r\xff", 200, map[string]string{"Content-Disposition": bad}, []byte("caf\xe9 body bytes stay"))
		f := wire(t, &conduitv1.Frame{Body: &conduitv1.Frame_RpcResponse{RpcResponse: ResponseToRPC(resp)}})
		got := f.GetRpcResponse()
		if got.GetHeaders()["Content-Disposition"] != fixed || got.GetRequestId() != "r\uFFFD" {
			t.Fatalf("response not normalised: %v", got)
		}
		if string(got.GetBody()) != "caf\xe9 body bytes stay" {
			t.Fatalf("body bytes must pass unchanged, got %q", got.GetBody())
		}
		back := RPCToResponse(&conduitv1.RpcResponse{Status: 200, Headers: map[string]string{"B": bad}})
		if back.Headers["B"] != fixed {
			t.Fatalf("RPCToResponse not normalised: %+v", back)
		}
	})
}

// TestConversionsCopy checks that a converted value shares no header map
// or body with its source, so a caller mutating one cannot change the
// other.
func TestConversionsCopy(t *testing.T) {
	req := wsprotocol.NewRequestEnvelope("r", "POST", "/p", "", map[string]string{"A": "1"}, []byte("body"))
	rpc := RequestToRPC(req)
	req.Headers["A"] = "2"
	req.Body[0] = 'X'
	if rpc.Headers["A"] != "1" || string(rpc.Body) != "body" {
		t.Fatalf("RpcRequest shares state with its envelope: %v", rpc)
	}
	resp := RPCToResponse(&conduitv1.RpcResponse{Status: 200, Headers: map[string]string{"B": "1"}, Body: []byte("ok")})
	if resp.Type != wsprotocol.TypeResponse || resp.StatusCode != 200 {
		t.Fatalf("unexpected response envelope %+v", resp)
	}
}
