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

package apiclient

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Golden canonical-string vectors. The same literals are pinned in
// pkg/hub/brokerauth_canonical_vector_test.go; both packages must produce
// these exact bytes and signatures. Sized and bodyless requests hash the
// same as before the body-coverage change, so these also pin wire
// compatibility with existing signers and verifiers.
const (
	goldenSecret    = "golden-vector-secret-0123456789ab"
	goldenTimestamp = "1700000000"
	goldenNonce     = "Z29sZGVuLW5vbmNlLTAx"
	goldenBody      = `{"name":"golden","n":1}`

	goldenSizedCanonical = "POST\n/api/v1/agents/a1/start\nb=2&a=1\n1700000000\nZ29sZGVuLW5vbmNlLTAx\n" +
		"x-scion-on-behalf-of:user-1\n" +
		"mJ+SuJt5I9IUZkAwhDi1QE66tuYPjI9gYahv1xxdhiw="
	goldenSizedSignature = "RETk5WaPpTSpIU4D5uiG/kff98FjF3dR5i53TnFCW+Y="

	goldenBodylessCanonical = "DELETE\n/api/v1/agents/a1\n\n1700000000\nZ29sZGVuLW5vbmNlLTAx\n"
	goldenBodylessSignature = "s38O3S1hlVkXBQuQJo88oj9Zjk2v4188Kpb9SlI27KA="
)

func goldenSizedRequest() *http.Request {
	req := httptest.NewRequest(http.MethodPost, "http://broker.test/api/v1/agents/a1/start?b=2&a=1", strings.NewReader(goldenBody))
	req.Header.Set(HeaderSignedHeaders, "X-Scion-On-Behalf-Of")
	req.Header.Set("X-Scion-On-Behalf-Of", "user-1")
	return req
}

func goldenBodylessRequest() *http.Request {
	return httptest.NewRequest(http.MethodDelete, "http://broker.test/api/v1/agents/a1", nil)
}

func TestBuildCanonicalString_GoldenVectors(t *testing.T) {
	cases := []struct {
		name          string
		req           *http.Request
		wantCanonical string
		wantSignature string
	}{
		{"sized POST", goldenSizedRequest(), goldenSizedCanonical, goldenSizedSignature},
		{"bodyless DELETE", goldenBodylessRequest(), goldenBodylessCanonical, goldenBodylessSignature},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := BuildCanonicalString(c.req, goldenTimestamp, goldenNonce)
			if string(got) != c.wantCanonical {
				t.Fatalf("canonical string:\n got %q\nwant %q", got, c.wantCanonical)
			}
			sig := base64.StdEncoding.EncodeToString(ComputeHMAC([]byte(goldenSecret), got))
			if sig != c.wantSignature {
				t.Fatalf("signature: got %q want %q", sig, c.wantSignature)
			}
		})
	}
}

// unsizedReader hides the length of its data from net/http, like a
// streaming body.
type unsizedReader struct{ r io.Reader }

func (u *unsizedReader) Read(p []byte) (int, error) { return u.r.Read(p) }

// errReader fails every read.
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }

// verify reports whether req carries a valid signature under secret, using
// the canonical string the verifiers build.
func verify(t *testing.T, req *http.Request, secret []byte) bool {
	t.Helper()
	sig, err := base64.StdEncoding.DecodeString(req.Header.Get(HeaderSignature))
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	canonical := BuildCanonicalString(req, req.Header.Get(HeaderTimestamp), req.Header.Get(HeaderNonce))
	return VerifyHMAC(secret, canonical, sig)
}

// asServerRequest copies the signed headers of req onto a new request with
// the given body and framing, as a verifier would see it.
func asServerRequest(req *http.Request, body io.Reader, contentLength int64, transferEncoding []string) *http.Request {
	out := httptest.NewRequest(req.Method, req.URL.String(), nil)
	out.Header = req.Header.Clone()
	if body != nil {
		out.Body = io.NopCloser(body)
	}
	out.ContentLength = contentLength
	out.TransferEncoding = transferEncoding
	return out
}

func TestBuildCanonicalString_HashesBodyWhateverContentLength(t *testing.T) {
	sized := BuildCanonicalString(goldenSizedRequest(), goldenTimestamp, goldenNonce)
	for _, cl := range []int64{-1, 0} {
		t.Run("ContentLength "+strconv.FormatInt(cl, 10), func(t *testing.T) {
			req := goldenSizedRequest()
			req.Body = io.NopCloser(&unsizedReader{strings.NewReader(goldenBody)})
			req.ContentLength = cl
			got := BuildCanonicalString(req, goldenTimestamp, goldenNonce)
			if !bytes.Equal(got, sized) {
				t.Fatalf("an unsized body must hash like the sized one:\n got %q\nwant %q", got, sized)
			}
			restored, _ := io.ReadAll(req.Body)
			if string(restored) != goldenBody {
				t.Fatalf("body not restored: %q", restored)
			}
		})
	}
}

func TestBuildCanonicalString_EmptyBodyHasNoHashLine(t *testing.T) {
	for name, body := range map[string]io.ReadCloser{
		"nil":          nil,
		"NoBody":       http.NoBody,
		"empty reader": io.NopCloser(strings.NewReader("")),
	} {
		t.Run(name, func(t *testing.T) {
			req := goldenBodylessRequest()
			req.Body = body
			req.ContentLength = 0
			got := BuildCanonicalString(req, goldenTimestamp, goldenNonce)
			if string(got) != goldenBodylessCanonical {
				t.Fatalf("got %q want %q", got, goldenBodylessCanonical)
			}
		})
	}
}

func TestBuildCanonicalString_UnreadableBodyFailsClosed(t *testing.T) {
	req := goldenBodylessRequest()
	req.Body = io.NopCloser(errReader{})
	req.ContentLength = -1
	got := string(BuildCanonicalString(req, goldenTimestamp, goldenNonce))
	if got != goldenBodylessCanonical+bodyUnreadableMarker {
		t.Fatalf("got %q", got)
	}
	// The marker is outside the base64 alphabet, so it never equals a hash.
	if _, err := base64.StdEncoding.DecodeString(bodyUnreadableMarker); err == nil {
		t.Fatalf("marker %q decodes as base64", bodyUnreadableMarker)
	}
}

func TestHMACAuth_ApplyAuth_SizesUnsizedBody(t *testing.T) {
	secret := []byte(goldenSecret)
	auth := &HMACAuth{BrokerID: "broker-1", SecretKey: secret}
	cases := []struct {
		name string
		cl   int64
		te   []string
	}{
		{"ContentLength -1", -1, nil},
		{"ContentLength 0", 0, nil},
		{"chunked", -1, []string{"chunked"}},
		{"sized but chunked", int64(len(goldenBody)), []string{"chunked"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, "http://broker.test/api/v1/agents", &unsizedReader{strings.NewReader(goldenBody)})
			if err != nil {
				t.Fatal(err)
			}
			req.ContentLength = c.cl
			req.TransferEncoding = c.te
			if err := auth.ApplyAuth(req); err != nil {
				t.Fatalf("ApplyAuth: %v", err)
			}
			if req.ContentLength != int64(len(goldenBody)) || len(req.TransferEncoding) != 0 {
				t.Fatalf("body not sized: ContentLength %d TransferEncoding %v", req.ContentLength, req.TransferEncoding)
			}
			if err := CheckSignedBodyFraming(req); err != nil {
				t.Fatalf("signed request fails the framing check: %v", err)
			}
			if req.GetBody == nil {
				t.Fatal("GetBody not set")
			}
			again, _ := req.GetBody()
			if b, _ := io.ReadAll(again); string(b) != goldenBody {
				t.Fatalf("GetBody returned %q", b)
			}
			// The verifier sees the sized body and the signature covers it.
			if !verify(t, req, secret) {
				t.Fatal("signed request does not verify")
			}
			swapped := asServerRequest(req, strings.NewReader(strings.ToUpper(goldenBody)), int64(len(goldenBody)), nil)
			if verify(t, swapped, secret) {
				t.Fatal("a swapped body verifies")
			}
		})
	}
}

func TestHMACAuth_ApplyAuth_EmptyUnsizedBodyBecomesNoBody(t *testing.T) {
	auth := &HMACAuth{BrokerID: "broker-1", SecretKey: []byte(goldenSecret)}
	req, err := http.NewRequest(http.MethodPost, "http://broker.test/api/v1/agents/a1/stop", &unsizedReader{strings.NewReader("")})
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = -1
	if err := auth.ApplyAuth(req); err != nil {
		t.Fatalf("ApplyAuth: %v", err)
	}
	if req.Body != http.NoBody || req.ContentLength != 0 {
		t.Fatalf("got Body %T ContentLength %d", req.Body, req.ContentLength)
	}
	if !verify(t, req, []byte(goldenSecret)) {
		t.Fatal("does not verify")
	}
}

func TestHMACAuth_ApplyAuth_BodyReadErrorFails(t *testing.T) {
	auth := &HMACAuth{BrokerID: "broker-1", SecretKey: []byte(goldenSecret)}
	req, err := http.NewRequest(http.MethodPost, "http://broker.test/api/v1/agents", errReader{})
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.ApplyAuth(req); err == nil {
		t.Fatal("ApplyAuth signed a request whose body could not be read")
	}
	if req.Header.Get(HeaderSignature) != "" {
		t.Fatal("a signature was set")
	}
}

// A body added to a signed bodyless request changes the canonical string,
// whatever framing it arrives with, so the signature no longer verifies.
func TestSignedBodylessRequest_AddedBodyFailsVerification(t *testing.T) {
	secret := []byte(goldenSecret)
	auth := &HMACAuth{BrokerID: "broker-1", SecretKey: secret}
	for _, method := range []string{http.MethodPost, http.MethodDelete} {
		req, err := http.NewRequest(method, "http://broker.test/api/v1/agents/a1", nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := auth.ApplyAuth(req); err != nil {
			t.Fatal(err)
		}
		if !verify(t, asServerRequest(req, nil, 0, nil), secret) {
			t.Fatalf("%s: bodyless request does not verify", method)
		}
		added := `{"injected":true}`
		for _, framing := range []struct {
			name string
			cl   int64
			te   []string
		}{
			{"chunked", -1, []string{"chunked"}},
			{"unknown length", -1, nil},
			{"ContentLength 0", 0, nil},
		} {
			got := asServerRequest(req, strings.NewReader(added), framing.cl, framing.te)
			if verify(t, got, secret) {
				t.Errorf("%s with an added %s body verifies", method, framing.name)
			}
		}
	}
}

func TestCheckSignedBodyFraming(t *testing.T) {
	cases := []struct {
		name    string
		body    io.Reader
		cl      int64
		te      []string
		wantErr bool
	}{
		{"no body", nil, 0, nil, false},
		{"sized body", strings.NewReader(goldenBody), int64(len(goldenBody)), nil, false},
		{"unknown length", strings.NewReader(goldenBody), -1, nil, true},
		{"chunked", strings.NewReader(goldenBody), -1, []string{"chunked"}, true},
		{"sized but chunked", strings.NewReader(goldenBody), int64(len(goldenBody)), []string{"chunked"}, true},
		{"empty chunked", strings.NewReader(""), -1, []string{"chunked"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := asServerRequest(goldenBodylessRequest(), c.body, c.cl, c.te)
			err := CheckSignedBodyFraming(req)
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
			if err != nil && !errors.Is(err, ErrUnsizedSignedBody) {
				t.Fatalf("err = %v, want ErrUnsizedSignedBody", err)
			}
		})
	}
}

// The framing net/http gives a server for a body sent through a real
// connection: a streamed body arrives chunked and is refused, a signed
// body arrives sized.
func TestCheckSignedBodyFraming_OverHTTP(t *testing.T) {
	type seen struct {
		err  error
		body string
	}
	results := make(chan seen, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		err := CheckSignedBodyFraming(r)
		b, _ := io.ReadAll(r.Body)
		results <- seen{err, string(b)}
	}))
	defer srv.Close()
	client := srv.Client()
	client.Timeout = 10 * time.Second

	streamed, _ := http.NewRequest(http.MethodPost, srv.URL+"/x", &unsizedReader{strings.NewReader(goldenBody)})
	resp, err := client.Do(streamed)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if got := <-results; got.err == nil {
		t.Fatal("a streamed body passed the framing check")
	}

	signed, _ := http.NewRequest(http.MethodPost, srv.URL+"/x", &unsizedReader{strings.NewReader(goldenBody)})
	if err := (&HMACAuth{BrokerID: "b", SecretKey: []byte(goldenSecret)}).ApplyAuth(signed); err != nil {
		t.Fatal(err)
	}
	resp, err = client.Do(signed)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if got := <-results; got.err != nil || got.body != goldenBody {
		t.Fatalf("signed body: err %v body %q", got.err, got.body)
	}
}

// A sized, rewindable body is signed without another copy: the signer
// leaves it to BuildCanonicalString, which still hashes it.
func TestSizeRequestBody_KeepsSizedRewindableBody(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "http://broker.test/api/v1/agents", strings.NewReader(goldenBody))
	if err != nil {
		t.Fatal(err)
	}
	body, getBody := req.Body, req.GetBody
	if err := sizeRequestBody(req); err != nil {
		t.Fatal(err)
	}
	if req.Body != body || req.ContentLength != int64(len(goldenBody)) {
		t.Fatalf("sized body was replaced: Body %T ContentLength %d", req.Body, req.ContentLength)
	}
	if req.GetBody == nil || getBody == nil {
		t.Fatal("GetBody lost")
	}
	secret := []byte(goldenSecret)
	if err := (&HMACAuth{BrokerID: "broker-1", SecretKey: secret}).ApplyAuth(req); err != nil {
		t.Fatal(err)
	}
	if !verify(t, req, secret) {
		t.Fatal("does not verify")
	}
	swapped := asServerRequest(req, strings.NewReader(strings.ToUpper(goldenBody)), int64(len(goldenBody)), nil)
	if verify(t, swapped, secret) {
		t.Fatal("a swapped body verifies")
	}
}

// failingSizedBody returns some bytes and then a read error.
type failingSizedBody struct{ sent bool }

func (f *failingSizedBody) Read(p []byte) (int, error) {
	if !f.sent {
		f.sent = true
		return copy(p, `{"part`), nil
	}
	return 0, io.ErrUnexpectedEOF
}

// A sized, rewindable body whose read fails is not signed: the signer
// never signs the unreadable-body marker, so no request whose body read
// fails on the verifier can match a signature.
func TestHMACAuth_ApplyAuth_SizedRewindableBodyReadErrorFails(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "http://broker.test/api/v1/agents", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Body = io.NopCloser(&failingSizedBody{})
	req.ContentLength = 100
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(&failingSizedBody{}), nil }
	auth := &HMACAuth{BrokerID: "broker-1", SecretKey: []byte(goldenSecret)}
	if err := auth.ApplyAuth(req); err == nil {
		t.Fatal("ApplyAuth signed a request whose body could not be read")
	}
	if req.Header.Get(HeaderSignature) != "" {
		t.Fatal("a signature was set")
	}
}

// The verifier side still writes the marker on a read error.
func TestBuildCanonicalString_ReadErrorWritesMarkerOnly(t *testing.T) {
	req := goldenBodylessRequest()
	req.Body = io.NopCloser(&failingSizedBody{})
	req.ContentLength = 100
	got := string(BuildCanonicalString(req, goldenTimestamp, goldenNonce))
	if got != goldenBodylessCanonical+bodyUnreadableMarker {
		t.Fatalf("got %q", got)
	}
	if _, err := buildCanonicalString(goldenBodylessRequest(), goldenTimestamp, goldenNonce); err != nil {
		t.Fatalf("bodyless: %v", err)
	}

	// A real signer's signature, over a body or over no body, never
	// verifies a request whose body read fails on the verifier.
	secret := []byte(goldenSecret)
	auth := &HMACAuth{BrokerID: "broker-1", SecretKey: secret}
	for _, body := range []string{"", goldenBody} {
		var rd io.Reader
		if body != "" {
			rd = strings.NewReader(body)
		}
		signed, err := http.NewRequest(http.MethodPost, "http://broker.test/api/v1/agents", rd)
		if err != nil {
			t.Fatal(err)
		}
		if err := auth.ApplyAuth(signed); err != nil {
			t.Fatal(err)
		}
		failing := asServerRequest(signed, &failingSizedBody{}, 100, nil)
		if verify(t, failing, secret) {
			t.Fatalf("signature over body %q verifies a request whose body read fails", body)
		}
	}
}

// closeTracker records whether the caller's body was closed.
type closeTracker struct {
	io.Reader
	closed bool
}

func (c *closeTracker) Close() error { c.closed = true; return nil }

// The caller's original body is closed once the signer has buffered it,
// on success and on a read error, on the sized rewindable path too.
func TestHMACAuth_ApplyAuth_ClosesOriginalBody(t *testing.T) {
	auth := &HMACAuth{BrokerID: "broker-1", SecretKey: []byte(goldenSecret)}
	cases := []struct {
		name    string
		reader  io.Reader
		cl      int64
		getBody bool
		wantErr bool
	}{
		{"sized rewindable", strings.NewReader(goldenBody), int64(len(goldenBody)), true, false},
		{"sized rewindable read error", &failingSizedBody{}, 100, true, true},
		{"unsized", &unsizedReader{strings.NewReader(goldenBody)}, -1, false, false},
		{"unsized read error", errReader{}, -1, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, "http://broker.test/api/v1/agents", nil)
			if err != nil {
				t.Fatal(err)
			}
			orig := &closeTracker{Reader: c.reader}
			req.Body = orig
			req.ContentLength = c.cl
			if c.getBody {
				req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(goldenBody)), nil }
			}
			err = auth.ApplyAuth(req)
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
			if !orig.closed {
				t.Fatal("the original body was not closed")
			}
			if !c.wantErr {
				if b, _ := io.ReadAll(req.Body); string(b) != goldenBody {
					t.Fatalf("buffered body %q", b)
				}
			}
		})
	}
}
