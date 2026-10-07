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

package artifacts

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

var testViewKey = []byte("0123456789abcdef0123456789abcdef")

var htmlSite = bundle{
	"index.html":    []byte(`<html><link rel=stylesheet href="css/s.css"><img src="img/a.png"></html>`),
	"img/a.png":     testPNG,
	"css/s.css":     []byte("body{}"),
	"docs/page.htm": []byte("<p>page</p>"),
}

func (f *fixture) mintView(p *principal, id string, seq int) (*ViewResponse, int) {
	f.t.Helper()
	rec := f.do(p, http.MethodPost, fmt.Sprintf("/api/v1/artifacts/%s/versions/%d/view", id, seq), nil, nil)
	if rec.Code != http.StatusOK {
		return nil, rec.Code
	}
	v := decodeInto[ViewResponse](f.t, rec)
	return &v, rec.Code
}

func TestViewServesTheBundle(t *testing.T) {
	f := newFixture(t, true) // object storage: views still stream
	f.svc.SetViewKey(testViewKey)
	pub := f.publishBundle(agentA, "/api/v1/artifacts", htmlSite.manifest("index.html"), htmlSite)
	id := pub.Artifact.ID

	view, code := f.mintView(&agentB, id, 1)
	if code != http.StatusOK {
		t.Fatalf("mint: %d", code)
	}
	if !strings.HasPrefix(view.URL, RouteView) || !strings.HasSuffix(view.URL, "/index.html") || view.RemoteImages {
		t.Fatalf("view = %+v", view)
	}
	if d := time.Until(view.ExpiresAt); d <= 0 || d > ViewTTL {
		t.Errorf("expires in %v", d)
	}
	base := strings.TrimSuffix(view.URL, "index.html")
	// No principal: the capability alone serves every file of the version,
	// including through relative paths.
	for p, body := range htmlSite {
		rec := f.do(nil, http.MethodGet, base+p, nil, nil)
		if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), body) {
			t.Fatalf("GET %s: %d", p, rec.Code)
		}
		h := rec.Header()
		for k, want := range map[string]string{
			"Content-Security-Policy": viewCSP,
			"X-Content-Type-Options":  "nosniff",
			"Referrer-Policy":         "no-referrer",
			"Cache-Control":           "private, no-store",
			"Content-Disposition":     "inline",
		} {
			if got := h.Get(k); got != want {
				t.Errorf("%s: %s = %q, want %q", p, k, got, want)
			}
		}
		if h.Get("Location") != "" {
			t.Errorf("%s: view redirected", p)
		}
	}
	if ct := f.do(nil, http.MethodGet, base+"index.html", nil, nil).Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("entry content type %q", ct)
	}
	for _, p := range []string{"missing.png", "../index.html", "img/../index.html", "_remote/" + sha([]byte("x"))} {
		if rec := f.do(nil, http.MethodGet, base+p, nil, nil); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s: %d, want 404", p, rec.Code)
		}
	}
	if rec := f.do(nil, http.MethodPost, base+"index.html", nil, nil); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST on a view: %d", rec.Code)
	}
}

func TestViewCapabilityChecks(t *testing.T) {
	f := newFixture(t, false)
	f.svc.SetViewKey(testViewKey)
	pub := f.publishBundle(agentA, "/api/v1/artifacts", htmlSite.manifest("index.html"), htmlSite)
	id := pub.Artifact.ID
	other := f.publishBundle(agentA, "/api/v1/artifacts", htmlSite.manifest("index.html"), htmlSite)
	exp := time.Now().Add(ViewTTL)
	good := mintViewCapability(testViewKey, id, 1, exp)
	get := func(capability string) int {
		return f.do(nil, http.MethodGet, RouteView+capability+"/index.html", nil, nil).Code
	}
	if c := get(good); c != http.StatusOK {
		t.Fatalf("good capability: %d", c)
	}
	sig := good[strings.LastIndexByte(good, '.')+1:]
	for name, capability := range map[string]string{
		"other key":            mintViewCapability([]byte("another key of thirty-two bytes!"), id, 1, exp),
		"expired":              mintViewCapability(testViewKey, id, 1, time.Now().Add(-time.Second)),
		"too far out":          mintViewCapability(testViewKey, id, 1, time.Now().Add(ViewTTL+2*viewMaxSkew)),
		"missing version":      mintViewCapability(testViewKey, id, 2, exp),
		"other artifact id":    strings.Replace(good, id, other.Artifact.ID, 1),
		"other seq":            id + ".2." + fmt.Sprint(exp.Unix()) + "." + sig,
		"other expiry":         id + ".1." + fmt.Sprint(exp.Unix()+1) + "." + sig,
		"truncated signature":  good[:len(good)-2],
		"no signature":         id + ".1." + fmt.Sprint(exp.Unix()),
		"extra part":           good + ".x",
		"leading zero expiry":  id + ".1.0" + fmt.Sprint(exp.Unix()) + "." + sig,
		"uppercase id":         strings.ToUpper(id) + good[len(id):],
		"empty":                "",
		"garbage":              "not-a-capability",
		"padded base64":        good + "=",
		"standard base64 char": good[:len(good)-1] + "+",
	} {
		if c := get(capability); c != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", name, c)
		}
	}
	// Without a key nothing is served or minted.
	f.svc.SetViewKey(nil)
	if c := get(good); c != http.StatusNotFound {
		t.Errorf("no key: %d", c)
	}
	if _, code := f.mintView(&agentA, id, 1); code != http.StatusServiceUnavailable {
		t.Errorf("mint without a key: %d", code)
	}
}

func TestViewMintAuthorization(t *testing.T) {
	f := newFixture(t, false)
	f.svc.SetViewKey(testViewKey)
	pub := f.publishBundle(agentA, "/api/v1/artifacts", htmlSite.manifest("index.html"), htmlSite)
	id := pub.Artifact.ID
	for name, p := range map[string]*principal{"unauthenticated": nil, "other project": &agentX, "outsider": &outside} {
		if _, code := f.mintView(p, id, 1); code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", name, code)
		}
	}
	f.host.deny(agentB, "project-1", PermissionRead)
	if _, code := f.mintView(&agentB, id, 1); code != http.StatusNotFound {
		t.Errorf("credential without read: %d, want 404", code)
	}
	if _, code := f.mintView(&agentA, id, 9); code != http.StatusNotFound {
		t.Errorf("missing version: %d, want 404", code)
	}
	md := f.publish(agentA, "doc.md", []byte("# x"), "")
	if _, code := f.mintView(&agentA, md.Artifact.ID, 1); code != http.StatusBadRequest {
		t.Errorf("markdown entry: %d, want 400", code)
	}
	// A pending version has no view.
	pend := f.createPending(agentA, "/api/v1/artifacts/"+id+"/versions", htmlSite.manifest("index.html"))
	if _, code := f.mintView(&agentA, id, pend.Version.Seq); code != http.StatusNotFound {
		t.Errorf("pending version: %d, want 404", code)
	}
	// A deleted artifact's capability stops working.
	view, _ := f.mintView(&agentA, id, 1)
	if _, err := f.db.Exec("UPDATE artifact SET deleted_at = ? WHERE id = ?", time.Now().UTC().Format(sqliteTimeLayout), id); err != nil {
		t.Fatal(err)
	}
	if rec := f.do(nil, http.MethodGet, view.URL, nil, nil); rec.Code != http.StatusNotFound {
		t.Errorf("deleted artifact: %d, want 404", rec.Code)
	}
}

func TestHTMLRemoteImagesNotice(t *testing.T) {
	f := newFixture(t, false)
	f.svc.SetViewKey(testViewKey)
	ff := &fakeFetcher{}
	f.useFetcher(ff)
	remote := bundle{"index.html": []byte(`<p>x</p><IMG alt=a src="https://img.example/a.png">`), "a.png": testPNG}
	pub := f.publishBundle(agentA, "/api/v1/artifacts", remote.manifest("index.html"), remote)
	if len(pub.Warnings) != 1 || pub.Warnings[0] != WarnHTMLRemoteImages {
		t.Errorf("finalize warnings %q", pub.Warnings)
	}
	if len(ff.calls) != 0 {
		t.Errorf("an HTML entry fetched %v", ff.calls)
	}
	view, _ := f.mintView(&agentA, pub.Artifact.ID, 1)
	if view == nil || !view.RemoteImages {
		t.Errorf("view = %+v, want remoteImages", view)
	}
	resp := f.publish(agentA, "page.html", []byte(`<img src="http://img.example/a.png">`), "")
	if len(resp.Warnings) != 1 || resp.Warnings[0] != WarnHTMLRemoteImages {
		t.Errorf("single-file warnings %q", resp.Warnings)
	}
	local := f.publish(agentA, "local.html", []byte(`<img src="a.png"><img src="data:image/png;base64,AA">`), "")
	if len(local.Warnings) != 0 {
		t.Errorf("local images warned: %q", local.Warnings)
	}
}

func TestHTMLHasRemoteImages(t *testing.T) {
	for doc, want := range map[string]bool{
		`<img src="https://x.example/a.png">`:  true,
		`<IMG SRC=http://x.example/a.png>`:     true,
		`<img src="img/a.png">`:                false,
		`<img data-src="https://x/a.png">`:     false,
		`<p>https://x.example/a.png</p>`:       false,
		`<img src="https://x.example/a.png"`:   false,
		`<img src="ht` + "\t" + `tps://x/a">`:  true,
		`![a](https://x.example/a.png)`:        false,
		`<imgx src="https://x.example/a.png">`: false,
	} {
		if got := htmlHasRemoteImages(doc); got != want {
			t.Errorf("htmlHasRemoteImages(%q) = %v", doc, got)
		}
	}
	// Linear on tag-heavy documents.
	for _, doc := range []string{fill("<img", imageScanWindow), fill("<img src=x <", imageScanWindow), fill("<img src='a'>", imageScanWindow)} {
		start := time.Now()
		htmlHasRemoteImages(doc)
		if d := time.Since(start); d > 2*time.Second && !raceEnabled {
			t.Errorf("took %v", d)
		}
	}
}
