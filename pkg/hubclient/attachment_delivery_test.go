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
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
)

// TestDeliveryRedemption_ObjectStoreRedirect: a capability redeemed against
// the hub with the broker's credential, answered with a redirect to an
// object store, is followed without the broker credential
// (ptone/scion#4596).
func TestDeliveryRedemption_ObjectStoreRedirect(t *testing.T) {
	objects := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(apiclient.HeaderBrokerID) != "" || r.Header.Get("Authorization") != "" {
			http.Error(w, "credential sent to the object store", http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte("object bytes"))
	}))
	defer objects.Close()
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(apiclient.HeaderBrokerID) != "broker-1" {
			http.Error(w, "no broker credential", http.StatusNotFound)
			return
		}
		http.Redirect(w, r, objects.URL+"/blob?sig=x", http.StatusFound)
	}))
	defer hub.Close()
	c, err := New(hub.URL, WithHMACAuth("broker-1", []byte("secret-key-for-test-0123456789ab")))
	if err != nil {
		t.Fatal(err)
	}
	f, ok := c.(AttachmentDeliveryFetcher)
	if !ok {
		t.Fatal("the hub client redeems delivery capabilities")
	}
	rc, err := f.FetchAttachmentDelivery(context.Background(), "/api/v1/attachment-deliveries/token-1")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	defer func() { _ = rc.Close() }()
	b, _ := io.ReadAll(rc)
	if string(b) != "object bytes" {
		t.Fatalf("body = %q", b)
	}
	if _, err := f.FetchAttachmentDelivery(context.Background(), "/api/v1/other"); err == nil {
		t.Fatal("a path outside the redemption route is refused")
	}
}

// TestFetchRedirect_HTTPSOnly: an object-store redirect is followed only
// over https, unless the hub itself is reached over plain http (a local
// development hub).
func TestFetchRedirect_HTTPSOnly(t *testing.T) {
	objects := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("object bytes"))
	}))
	defer objects.Close()
	if _, err := fetchRedirect(context.Background(), objects.URL+"/blob", false); err == nil {
		t.Fatal("a plain http redirect is refused for an https hub")
	}
	rc, err := fetchRedirect(context.Background(), objects.URL+"/blob", true)
	if err != nil {
		t.Fatalf("a plain http hub follows a plain http redirect: %v", err)
	}
	_ = rc.Close()
	if _, err := fetchRedirect(context.Background(), "ftp://objects.example/blob", true); err == nil {
		t.Fatal("other schemes are refused")
	}
}

// TestFetchAttachmentDelivery_ErrorsCarryNoURL: whatever fails, the error
// names neither the capability path nor the signed object-store URL, so a
// caller may log it.
func TestFetchAttachmentDelivery_ErrorsCarryNoURL(t *testing.T) {
	const token = "TOKENSECRET"
	// A hub that is not listening: a transport error (*url.Error).
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	c, err := New(deadURL, WithHMACAuth("broker-1", []byte("secret-key-for-test-0123456789ab")))
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.(AttachmentDeliveryFetcher).FetchAttachmentDelivery(context.Background(), "/api/v1/attachment-deliveries/"+token)
	if err == nil || strings.Contains(err.Error(), token) || strings.Contains(err.Error(), deadURL) {
		t.Fatalf("transport error = %v; want an error without the capability or the URL", err)
	}
	// An object store that is not listening: a transport error naming the
	// signed URL, a bearer for its lifetime, stays out of the error.
	deadObjects := httptest.NewServer(http.NotFoundHandler())
	deadObjectsURL := deadObjects.URL
	deadObjects.Close()
	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, deadObjectsURL+"/blob?sig=SIGSECRET", http.StatusFound)
	}))
	defer redirecting.Close()
	c, err = New(redirecting.URL, WithHMACAuth("broker-1", []byte("secret-key-for-test-0123456789ab")))
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.(AttachmentDeliveryFetcher).FetchAttachmentDelivery(context.Background(), "/api/v1/attachment-deliveries/"+token)
	if err == nil || strings.Contains(err.Error(), "SIGSECRET") || strings.Contains(err.Error(), deadObjectsURL) || strings.Contains(err.Error(), token) {
		t.Fatalf("object store transport error = %v; want an error without the signed URL or the capability", err)
	}
	// An object store that refuses: the signed URL stays out of the error.
	objects := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusForbidden)
	}))
	defer objects.Close()
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, objects.URL+"/blob?sig=SIGSECRET", http.StatusFound)
	}))
	defer hub.Close()
	c, err = New(hub.URL, WithHMACAuth("broker-1", []byte("secret-key-for-test-0123456789ab")))
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.(AttachmentDeliveryFetcher).FetchAttachmentDelivery(context.Background(), "/api/v1/attachment-deliveries/"+token)
	if err == nil || strings.Contains(err.Error(), "SIGSECRET") || strings.Contains(err.Error(), token) {
		t.Fatalf("object store error = %v; want an error without the signed URL or the capability", err)
	}
}
