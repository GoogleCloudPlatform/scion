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

package hubclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
)

// newBrokerAttachmentTestClient returns a hub client for srv signing as a
// broker, with a plugin name.
func newBrokerAttachmentTestClient(t *testing.T, srv *httptest.Server) *client {
	t.Helper()
	c, err := New(srv.URL, WithHMACAuth("broker-1", []byte("0123456789abcdef0123456789abcdef")), WithPluginName("tg-plugin"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c.(*client)
}

func TestCheckBrokerAttachments_RequestAndAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/broker/inbound/attachments/check" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get(apiclient.HeaderBrokerID) != "broker-1" || r.Header.Get(apiclient.HeaderSignature) == "" {
			t.Errorf("request is not HMAC-signed: %v", r.Header)
		}
		if got := r.Header.Get("X-Scion-Plugin-Name"); got != "tg-plugin" {
			t.Errorf("plugin name = %q", got)
		}
		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode: %v", err)
		}
		if body["sender"] != "user:a@example.com" || body["project_id"] != "p1" || body["surface"] != "telegram" {
			t.Errorf("body = %v", body)
		}
		if targets, _ := body["targets"].([]interface{}); len(targets) != 2 || targets[0] != "one" || targets[1] != "two" {
			t.Errorf("targets = %v", body["targets"])
		}
		_, _ = io.WriteString(w, `{"authorized":true,"uploads":"artifact","delivery_max_bytes":1000}`)
	}))
	defer srv.Close()
	c := newBrokerAttachmentTestClient(t, srv)
	got, err := c.CheckBrokerAttachments(context.Background(), "user:a@example.com", "p1", []string{"one", "two"}, "telegram")
	if err != nil {
		t.Fatalf("CheckBrokerAttachments: %v", err)
	}
	if !got.Authorized || got.Uploads != AttachmentUploadsArtifact || got.DeliveryMaxBytes == nil || *got.DeliveryMaxBytes != 1000 {
		t.Fatalf("answer = %+v", got)
	}
}

// Only a 404 that is not the hub's JSON error envelope means the route is
// absent; a 404 carrying the envelope and any other failure is an error.
func TestCheckBrokerAttachments_RouteAbsentOnlyOnBare404(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		wantAbsent bool
	}{
		{name: "bare 404", status: 404, body: "404 page not found\n", wantAbsent: true},
		{name: "empty 404", status: 404, body: "", wantAbsent: true},
		{name: "json 404 without code", status: 404, body: `{"error":{}}`, wantAbsent: true},
		{name: "enveloped 404", status: 404, body: `{"error":{"code":"not_found","message":"x"}}`},
		{name: "server error", status: 500, body: "boom"},
		{name: "bad request", status: 400, body: `{"error":{"code":"validation_error","message":"x"}}`},
		{name: "forbidden", status: 403, body: "nope"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}))
			defer srv.Close()
			c, err := New(srv.URL, WithHMACAuth("broker-1", []byte("0123456789abcdef0123456789abcdef")), WithRetry(0, 0))
			if err != nil {
				t.Fatal(err)
			}
			_, err = c.(*client).CheckBrokerAttachments(context.Background(), "user:a@example.com", "p1", nil, "telegram")
			if err == nil {
				t.Fatal("want an error")
			}
			if got := errors.Is(err, ErrBrokerAttachmentsUnsupported); got != tt.wantAbsent {
				t.Fatalf("route absent = %v, want %v (err %v)", got, tt.wantAbsent, err)
			}
		})
	}
}

func TestUploadBrokerAttachments_WireFormat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/broker/inbound/attachments" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.ContentLength <= 0 {
			t.Errorf("a signed upload must declare its length, got %d", r.ContentLength)
		}
		if r.Header.Get(apiclient.HeaderSignature) == "" || r.Header.Get("X-Scion-Plugin-Name") != "tg-plugin" {
			t.Errorf("headers = %v", r.Header)
		}
		mr, err := r.MultipartReader()
		if err != nil {
			t.Errorf("multipart: %v", err)
			return
		}
		var got []string
		for {
			p, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Errorf("part: %v", err)
				return
			}
			b, _ := io.ReadAll(p)
			if p.FileName() != "" {
				got = append(got, p.FormName()+"["+p.FileName()+"]="+string(b))
			} else {
				got = append(got, p.FormName()+"="+string(b))
			}
		}
		want := []string{
			"sender=user:a@example.com", "project_id=p1", "surface=telegram", "audience=group",
			"targets=one", "targets=two", "file[a.txt]=hello", "file[b.txt]=world",
		}
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("parts =\n %v\nwant\n %v", got, want)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"attachments":[{"id":"1","name":"a.txt","mime":"text/plain","size":5},{"id":"2","name":"b.txt","mime":"text/plain","size":5}],"failures":[]}`)
	}))
	defer srv.Close()
	c := newBrokerAttachmentTestClient(t, srv)
	res, err := c.UploadBrokerAttachments(context.Background(), "user:a@example.com", "p1", []string{"one", "two"}, "telegram", "group",
		[]NamedReader{{Name: "a.txt", Reader: strings.NewReader("hello")}, {Name: "b.txt", Reader: strings.NewReader("world")}})
	if err != nil {
		t.Fatalf("UploadBrokerAttachments: %v", err)
	}
	if len(res.Attachments) != 2 || res.Attachments[1].ID != "2" {
		t.Fatalf("result = %+v", res)
	}
}

func TestUploadBrokerAttachments_RefusalsAndLimit(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"code":"message_denied","message":"Message delivery denied"}}`)
	}))
	defer srv.Close()
	c := newBrokerAttachmentTestClient(t, srv)
	files := []NamedReader{{Name: "a.txt", Reader: strings.NewReader("hello")}}
	_, err := c.UploadBrokerAttachments(context.Background(), "user:a@example.com", "p1", nil, "telegram", "direct", files)
	var apiErr *apiclient.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusForbidden || apiErr.Code != "message_denied" {
		t.Fatalf("err = %v", err)
	}

	big := []NamedReader{{Name: "big.bin", Reader: strings.NewReader(strings.Repeat("x", int(apiclient.MaxSignedRequestBodyBytes)))}}
	before := hits.Load()
	_, err = c.UploadBrokerAttachments(context.Background(), "user:a@example.com", "p1", nil, "telegram", "direct", big)
	if !errors.Is(err, errSignedBodyTooLarge) {
		t.Fatalf("err = %v, want the signed-body limit", err)
	}
	if hits.Load() != before {
		t.Fatal("an over-limit upload was sent")
	}
	if _, err := c.UploadBrokerAttachments(context.Background(), "user:a@example.com", "p1", nil, "telegram", "direct", nil); err == nil {
		t.Fatal("an upload without files must fail")
	}
}

func TestUploadAttachmentReaders_StreamsFiles(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/chat/attachments" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		mr, err := r.MultipartReader()
		if err != nil {
			t.Errorf("multipart: %v", err)
			return
		}
		var names []string
		for {
			p, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Errorf("part: %v", err)
				return
			}
			if p.FormName() != "files" {
				t.Errorf("part name = %q", p.FormName())
			}
			names = append(names, p.FileName())
		}
		if strings.Join(names, ",") != "x.txt,y.txt" {
			t.Errorf("files = %v", names)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"attachments":[{"id":"1","name":"x.txt","size":1},{"id":"2","name":"y.txt","size":1}]}`)
	}))
	defer srv.Close()
	c, err := New(srv.URL, WithBearerToken("tok"))
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.(*client).UploadAttachmentReaders(context.Background(),
		[]NamedReader{{Name: "x.txt", Reader: strings.NewReader("1")}, {Name: "y.txt", Reader: strings.NewReader("2")}})
	if err != nil {
		t.Fatalf("UploadAttachmentReaders: %v", err)
	}
	if len(res.Attachments) != 2 {
		t.Fatalf("result = %+v", res)
	}
	if _, err := c.(*client).UploadAttachmentReaders(context.Background(), nil); err == nil {
		t.Fatal("an upload without files must fail")
	}
	if _, err := c.(*client).UploadAttachmentReaders(context.Background(), []NamedReader{{Name: "", Reader: strings.NewReader("1")}}); err == nil {
		t.Fatal("a file without a name must fail")
	}
}
