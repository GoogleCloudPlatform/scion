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
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/keyring"
)

// Phase 4 of ptone/scion#1530: view capabilities signed by a versioned
// signing ring. Synthetic fixtures only.

const (
	vkLegacySecret = "test-only-view-legacy-secret"
	vkGen1Secret   = "test-only-view-generation-1-0123456789abcdef"
	vkGen2Secret   = "test-only-view-generation-2-0123456789abcdef"
)

// vkPurpose stands in for the hub's view purpose (any deterministic
// derivation works for these tests).
var vkPurpose = keyring.NewPurpose("test_artifact_view", func(secret []byte) []byte {
	sum := sha256.Sum256(append([]byte("test-view:"), secret...))
	return sum[:]
})

var vkT0 = time.Now().Truncate(time.Second)

type vkEntry struct {
	id, secret string
	// signUntil/verifyUntil are offsets from vkT0; zero means no deadline.
	signUntil, verifyUntil time.Duration
}

// vkRing parses a version-1 keyring file with the given signing entries.
func vkRing(t *testing.T, active, emit string, legacyReads []string, entries ...vkEntry) *keyring.SigningRing {
	t.Helper()
	keys := make([]any, 0, len(entries))
	for _, e := range entries {
		m := map[string]any{"id": e.id, "secret": e.secret, "sign_until": nil, "verify_until": nil}
		if e.signUntil != 0 {
			m["sign_until"] = vkT0.Add(e.signUntil).UTC().Format(time.RFC3339)
			m["verify_until"] = vkT0.Add(e.verifyUntil).UTC().Format(time.RFC3339)
		}
		keys = append(keys, m)
	}
	doc := map[string]any{
		"schema": 1, "revision": "test-rev",
		"signing": map[string]any{"active": active, "emit": emit, "keys": keys, "legacy_read_ids": legacyReads},
		"encryption": map[string]any{
			"active": "legacy", "write_format": "v1",
			"keys":               []any{map[string]any{"id": "legacy", "key_b64": base64.StdEncoding.EncodeToString(keyring.DeriveLegacyEncryptionKey(vkLegacySecret))}},
			"legacy_v1_read_ids": []any{"legacy"},
		},
	}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	k, err := keyring.Parse(data)
	if err != nil {
		t.Fatalf("parse keyring: %v", err)
	}
	return k.Signing()
}

type vkClock struct{ t time.Time }

func (c *vkClock) now() time.Time { return c.t }

// vkRollout returns two replicas' keys mid-rollout: A has gen1 active, B
// has gen2 active and keeps gen1 as the outgoing generation (sign_until
// T0+1h, verify_until T0+48h). Both emit versioned, and both read legacy
// (ID-less) capabilities with the imported legacy entry only.
func vkRollout(t *testing.T, clk *vkClock) (a, b ViewKeys) {
	legacy := vkEntry{id: "legacy", secret: vkLegacySecret}
	ra := vkRing(t, "gen1", "versioned", []string{"legacy"}, legacy,
		vkEntry{id: "gen1", secret: vkGen1Secret}, vkEntry{id: "gen2", secret: vkGen2Secret})
	rb := vkRing(t, "gen2", "versioned", []string{"legacy"}, legacy,
		vkEntry{id: "gen1", secret: vkGen1Secret, signUntil: time.Hour, verifyUntil: 48 * time.Hour},
		vkEntry{id: "gen2", secret: vkGen2Secret})
	return ViewKeys{Ring: ra, Purpose: vkPurpose, Now: clk.now}, ViewKeys{Ring: rb, Purpose: vkPurpose, Now: clk.now}
}

// TestViewCapabilityGoldens pins both wire forms to formulas written out
// independently of the code under test: the legacy form is byte-identical
// to the pre-ring capability, and the versioned form signs
// "artifact-view-v2\n" + kid + "\n" + id/seq/exp/linkID with an empty
// linkID kept for a session capability.
func TestViewCapabilityGoldens(t *testing.T) {
	key := []byte("test-only-golden-view-key-32byte")
	id := "11111111-2222-4333-8444-555555555555"
	link := "66666666-7777-4888-8999-aaaaaaaaaaaa"
	exp := time.Unix(1893456000, 0)
	mac := func(msg string) string {
		m := hmac.New(sha256.New, key)
		m.Write([]byte(msg))
		return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
	}
	legacy := LegacyViewKeys(key)
	got, err := mintView(legacy, id, 3, exp, "")
	if err != nil {
		t.Fatal(err)
	}
	if want := id + ".3.1893456000." + mac("artifact-view\n"+id+"\n3\n1893456000"); got != want {
		t.Errorf("legacy session form:\n got %s\nwant %s", got, want)
	}
	got, err = mintView(legacy, id, 3, exp, link)
	if err != nil {
		t.Fatal(err)
	}
	if want := id + ".3.1893456000." + link + "." + mac("artifact-view-link\n"+id+"\n3\n1893456000\n"+link); got != want {
		t.Errorf("legacy link form:\n got %s\nwant %s", got, want)
	}
	if want := "v2.gen7." + id + ".3.1893456000." + mac("artifact-view-v2\ngen7\n"+id+"\n3\n1893456000\n"); mintViewCapabilityV2(key, "gen7", id, 3, exp, "") != want {
		t.Errorf("v2 session form:\n got %s\nwant %s", mintViewCapabilityV2(key, "gen7", id, 3, exp, ""), want)
	}
	if want := "v2.gen7." + id + ".3.1893456000." + link + "." + mac("artifact-view-v2\ngen7\n"+id+"\n3\n1893456000\n"+link); mintViewCapabilityV2(key, "gen7", id, 3, exp, link) != want {
		t.Errorf("v2 link form:\n got %s\nwant %s", mintViewCapabilityV2(key, "gen7", id, 3, exp, link), want)
	}
}

// TestViewKeys_EmitSelectsForm: emit legacy signs the legacy form with the
// active generation; emit versioned signs v2 carrying the active ID.
func TestViewKeys_EmitSelectsForm(t *testing.T) {
	id := "11111111-2222-4333-8444-555555555555"
	exp := time.Now().Add(ViewTTL).Truncate(time.Second)
	legacyEntry := vkEntry{id: "legacy", secret: vkLegacySecret}
	leg := ViewKeys{Ring: vkRing(t, "legacy", "legacy", []string{"legacy"}, legacyEntry), Purpose: vkPurpose}
	c, err := mintView(leg, id, 1, exp, "")
	if err != nil || strings.HasPrefix(c, "v2.") {
		t.Fatalf("emit legacy: %q %v", c, err)
	}
	if _, err := parseViewCapability(leg, c, time.Now()); err != nil {
		t.Errorf("emit legacy capability refused: %v", err)
	}
	ver := ViewKeys{Ring: vkRing(t, "legacy", "versioned", []string{"legacy"}, legacyEntry), Purpose: vkPurpose}
	c, err = mintView(ver, id, 1, exp, "")
	if err != nil || !strings.HasPrefix(c, "v2.legacy.") {
		t.Fatalf("emit versioned: %q %v", c, err)
	}
	// Readers accept both forms whatever their emit setting.
	if _, err := parseViewCapability(leg, c, time.Now()); err != nil {
		t.Errorf("v2 capability refused by an emit-legacy reader: %v", err)
	}
}

// TestViewKeys_CrossInstanceDuringGrace: through the real service routes
// over one store, a session view and a link-bound view minted under either
// replica's active generation are served by the other replica's keys.
func TestViewKeys_CrossInstanceDuringGrace(t *testing.T) {
	clk := &vkClock{t: vkT0}
	a, b := vkRollout(t, clk)
	f := newFixture(t, false)
	f.svc.limiter = newRateLimiter(time.Now, 1000, 1000, 100000, 100000, 1000)
	pub := f.publishBundle(agentA, "/api/v1/artifacts", htmlSite.manifest("index.html"), htmlSite)
	id := f.publish(userU, "doc.md", []byte("# shared"), "scope=project-1").Artifact.ID

	for _, tc := range []struct {
		name         string
		mint, server ViewKeys
		kid          string
	}{
		{"A to B", a, b, "gen1"},
		{"B to A", b, a, "gen2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f.svc.SetViewKeys(tc.mint)
			v, code := f.mintView(&agentA, pub.Artifact.ID, 1)
			if code != http.StatusOK {
				t.Fatalf("mint: %d", code)
			}
			if !strings.HasPrefix(v.URL, RouteView+"v2."+tc.kid+".") {
				t.Fatalf("minted %q, want a v2 capability of %s", v.URL, tc.kid)
			}
			tok, _ := f.mustMintLink(userU, id, "")
			loc := f.shared(RouteShared+tok, "").Header().Get("Location")
			if !strings.HasPrefix(loc, RouteView+"v2."+tc.kid+".") {
				t.Fatalf("link redirect %q, want a v2 capability of %s", loc, tc.kid)
			}
			f.svc.SetViewKeys(tc.server)
			if rec := f.do(nil, http.MethodGet, v.URL, nil, nil); rec.Code != http.StatusOK {
				t.Errorf("session view on the other replica: %d", rec.Code)
			}
			if rec := f.shared(loc, ""); rec.Code != http.StatusOK {
				t.Errorf("link view on the other replica: %d", rec.Code)
			}
		})
	}
}

// TestViewKeys_LegacyCapabilityUpgrade: a capability minted by a pre-ring
// (legacy single-key) replica is served by a ring replica whose legacy
// reader is the same imported material, and refused by one without it.
func TestViewKeys_LegacyCapabilityUpgrade(t *testing.T) {
	f := newFixture(t, false)
	pub := f.publishBundle(agentA, "/api/v1/artifacts", htmlSite.manifest("index.html"), htmlSite)
	legacyRing := vkRing(t, "legacy", "legacy", []string{"legacy"}, vkEntry{id: "legacy", secret: vkLegacySecret})
	f.svc.SetViewKeys(ViewKeys{Ring: legacyRing, Purpose: vkPurpose})
	v, code := f.mintView(&agentA, pub.Artifact.ID, 1)
	if code != http.StatusOK || strings.HasPrefix(v.URL, RouteView+"v2.") {
		t.Fatalf("legacy mint: %d %+v", code, v)
	}
	clk := &vkClock{t: vkT0}
	_, b := vkRollout(t, clk)
	f.svc.SetViewKeys(b)
	if rec := f.do(nil, http.MethodGet, v.URL, nil, nil); rec.Code != http.StatusOK {
		t.Errorf("legacy capability on a ring replica: %d", rec.Code)
	}
	noLegacy := ViewKeys{Ring: vkRing(t, "gen2", "versioned", []string{"gen2"}, vkEntry{id: "gen2", secret: vkGen2Secret}), Purpose: vkPurpose}
	f.svc.SetViewKeys(noLegacy)
	if rec := f.do(nil, http.MethodGet, v.URL, nil, nil); rec.Code != http.StatusNotFound {
		t.Errorf("legacy capability without a legacy reader for its key: %d, want 404", rec.Code)
	}
}

// TestViewKeys_UnknownGenerationAndPartialFields: every malformed or
// unknown-generation versioned capability gets the uniform 404.
func TestViewKeys_UnknownGenerationAndPartialFields(t *testing.T) {
	clk := &vkClock{t: vkT0}
	a, b := vkRollout(t, clk)
	f := newFixture(t, false)
	pub := f.publishBundle(agentA, "/api/v1/artifacts", htmlSite.manifest("index.html"), htmlSite)
	id := pub.Artifact.ID
	exp := time.Now().Add(ViewTTL).Truncate(time.Second)
	gen1, err := a.Ring.Active(vkPurpose, clk.now())
	if err != nil {
		t.Fatal(err)
	}
	good := mintViewCapabilityV2(gen1.Bytes(), "gen1", id, 1, exp, "")
	f.svc.SetViewKeys(b)
	get := func(c string) int { return f.do(nil, http.MethodGet, RouteView+c+"/index.html", nil, nil).Code }
	if c := get(good); c != http.StatusOK {
		t.Fatalf("good v2 capability: %d", c)
	}
	body := strings.TrimPrefix(good, "v2.gen1.")
	parts := strings.Split(body, ".")
	sig := parts[len(parts)-1]
	legacyForm := strings.Join(parts, ".")
	for name, c := range map[string]string{
		"unknown kid (signed by gen1 key)": mintViewCapabilityV2(gen1.Bytes(), "gen9", id, 1, exp, ""),
		"kid swapped to another known gen": "v2.gen2." + body,
		"kid stripped to the legacy form":  legacyForm,
		"prefix only":                      "v2.",
		"prefix and kid only":              "v2.gen1",
		"empty kid":                        "v2.." + body,
		"kid with bad characters":          "v2.gen!1." + body,
		"kid too long":                     "v2." + strings.Repeat("k", 33) + "." + body,
		"missing field":                    "v2.gen1." + parts[0] + "." + parts[1] + "." + sig,
		"extra field":                      good + ".x",
		"uppercase version marker":         "V2.gen1." + body,
		"legacy form signed under the v1 domain carrying a kid": "v2.gen1." + mintViewCapability(gen1.Bytes(), id, 1, exp),
	} {
		if code := get(c); code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", name, code)
		}
	}
	// A versioned session capability cannot be made link-bound and back.
	link := "66666666-7777-4888-8999-aaaaaaaaaaaa"
	linked := mintViewCapabilityV2(gen1.Bytes(), "gen1", id, 1, exp, link)
	lp := strings.Split(strings.TrimPrefix(linked, "v2.gen1."), ".")
	asSession := "v2.gen1." + strings.Join([]string{lp[0], lp[1], lp[2], lp[4]}, ".")
	if _, err := parseViewCapability(b, asSession, time.Now()); err == nil {
		t.Errorf("a v2 link capability without its link id was accepted")
	}
	asLink := "v2.gen1." + strings.Join([]string{parts[0], parts[1], parts[2], link, sig}, ".")
	if _, err := parseViewCapability(b, asLink, time.Now()); err == nil {
		t.Errorf("a v2 session capability with a link id was accepted")
	}
}

// TestViewKeys_RetirementBoundary: a gen1 capability is served by B until
// gen1's verify_until and refused from that instant; a legacy capability
// follows its legacy reader's deadline the same way.
func TestViewKeys_RetirementBoundary(t *testing.T) {
	clk := &vkClock{t: vkT0}
	a, b := vkRollout(t, clk)
	f := newFixture(t, false)
	pub := f.publishBundle(agentA, "/api/v1/artifacts", htmlSite.manifest("index.html"), htmlSite)
	f.svc.SetViewKeys(a)
	v, code := f.mintView(&agentA, pub.Artifact.ID, 1)
	if code != http.StatusOK {
		t.Fatalf("mint: %d", code)
	}
	f.svc.SetViewKeys(b)
	clk.t = vkT0.Add(48*time.Hour - time.Second)
	if rec := f.do(nil, http.MethodGet, v.URL, nil, nil); rec.Code != http.StatusOK {
		t.Errorf("one second before verify_until: %d", rec.Code)
	}
	clk.t = vkT0.Add(48 * time.Hour)
	if rec := f.do(nil, http.MethodGet, v.URL, nil, nil); rec.Code != http.StatusNotFound {
		t.Errorf("at verify_until: %d, want 404", rec.Code)
	}

	// Legacy (ID-less) capability, legacy reader with a deadline.
	clk.t = vkT0
	legacyOnly := ViewKeys{Ring: vkRing(t, "legacy", "legacy", []string{"legacy"}, vkEntry{id: "legacy", secret: vkLegacySecret}), Purpose: vkPurpose}
	f.svc.SetViewKeys(legacyOnly)
	lv, code := f.mintView(&agentA, pub.Artifact.ID, 1)
	if code != http.StatusOK {
		t.Fatalf("legacy mint: %d", code)
	}
	retiring := ViewKeys{Ring: vkRing(t, "gen2", "versioned", []string{"legacy"},
		vkEntry{id: "legacy", secret: vkLegacySecret, signUntil: time.Hour, verifyUntil: 2 * time.Hour},
		vkEntry{id: "gen2", secret: vkGen2Secret}), Purpose: vkPurpose, Now: clk.now}
	f.svc.SetViewKeys(retiring)
	clk.t = vkT0.Add(2*time.Hour - time.Second)
	if rec := f.do(nil, http.MethodGet, lv.URL, nil, nil); rec.Code != http.StatusOK {
		t.Errorf("legacy capability before its reader's verify_until: %d", rec.Code)
	}
	clk.t = vkT0.Add(2 * time.Hour)
	if rec := f.do(nil, http.MethodGet, lv.URL, nil, nil); rec.Code != http.StatusNotFound {
		t.Errorf("legacy capability at its reader's verify_until: %d, want 404", rec.Code)
	}
}

// TestViewKeys_IssuanceClosed: once the active generation reaches its
// sign_until, neither the mint route nor a share link signs anything (503),
// existing capabilities still verify, and the one warning names no key
// material.
func TestViewKeys_IssuanceClosed(t *testing.T) {
	clk := &vkClock{t: vkT0}
	closing := ViewKeys{Ring: vkRing(t, "gen1", "versioned", []string{"legacy"},
		vkEntry{id: "legacy", secret: vkLegacySecret},
		vkEntry{id: "gen1", secret: vkGen1Secret, signUntil: time.Hour, verifyUntil: 48 * time.Hour}),
		Purpose: vkPurpose, Now: clk.now}
	f := newFixture(t, false)
	f.svc.limiter = newRateLimiter(time.Now, 1000, 1000, 100000, 100000, 1000)
	f.svc.SetViewKeys(closing)
	pub := f.publishBundle(agentA, "/api/v1/artifacts", htmlSite.manifest("index.html"), htmlSite)
	id := f.publish(userU, "doc.md", []byte("# shared"), "scope=project-1").Artifact.ID
	v, code := f.mintView(&agentA, pub.Artifact.ID, 1)
	if code != http.StatusOK {
		t.Fatalf("mint before sign_until: %d", code)
	}
	tok, _ := f.mustMintLink(userU, id, "")

	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })

	clk.t = vkT0.Add(time.Hour)
	if _, code := f.mintView(&agentA, pub.Artifact.ID, 1); code != http.StatusServiceUnavailable {
		t.Errorf("mint at sign_until: %d, want 503", code)
	}
	if rec := f.shared(RouteShared+tok, ""); rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Location") != "" {
		t.Errorf("share link at sign_until: %d (Location %q), want 503 without a capability", rec.Code, rec.Header().Get("Location"))
	}
	if rec := f.do(nil, http.MethodGet, v.URL, nil, nil); rec.Code != http.StatusOK {
		t.Errorf("an earlier capability after sign_until: %d", rec.Code)
	}
	out := logs.String()
	if strings.Count(out, "view capability not issued") != 1 {
		t.Errorf("want exactly one issuance-closed warning, got:\n%s", out)
	}
	for _, s := range []string{vkGen1Secret, vkLegacySecret, "gen1"} {
		if strings.Contains(out, s) {
			t.Errorf("log names %q", s)
		}
	}
}

// TestViewKeys_NotConfigured: zero ViewKeys and a ring with no usable
// active generation sign and accept nothing.
func TestViewKeys_NotConfigured(t *testing.T) {
	id := "11111111-2222-4333-8444-555555555555"
	exp := time.Now().Add(ViewTTL)
	for name, k := range map[string]ViewKeys{
		"zero":      {},
		"empty key": LegacyViewKeys(nil),
		"zero ring": {Ring: &keyring.SigningRing{}, Purpose: vkPurpose},
		"nil ring":  {Ring: nil, Purpose: vkPurpose},
	} {
		if k.configured() {
			t.Errorf("%s: configured", name)
		}
		if _, err := mintView(k, id, 1, exp, ""); err == nil {
			t.Errorf("%s: minted", name)
		}
		c := mintViewCapability([]byte("test-only-any-key"), id, 1, exp)
		if _, err := parseViewCapability(k, c, time.Now()); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
