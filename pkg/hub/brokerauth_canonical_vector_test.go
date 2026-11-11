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

package hub

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
)

// Golden canonical-string vectors. The same literals are pinned in
// pkg/apiclient/hmac_body_coverage_test.go; both packages must produce
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

func TestBrokerAuthCanonicalString_GoldenVectors(t *testing.T) {
	sized := func() *http.Request {
		req := httptest.NewRequest(http.MethodPost, "http://broker.test/api/v1/agents/a1/start?b=2&a=1", strings.NewReader(goldenBody))
		req.Header.Set(HeaderSignedHeaders, "X-Scion-On-Behalf-Of")
		req.Header.Set(HeaderOnBehalfOf, "user-1")
		return req
	}
	bodyless := func() *http.Request {
		return httptest.NewRequest(http.MethodDelete, "http://broker.test/api/v1/agents/a1", nil)
	}
	cases := []struct {
		name          string
		req           func() *http.Request
		wantCanonical string
		wantSignature string
	}{
		{"sized POST", sized, goldenSizedCanonical, goldenSizedSignature},
		{"bodyless DELETE", bodyless, goldenBodylessCanonical, goldenBodylessSignature},
	}
	svc := &BrokerAuthService{}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := svc.buildCanonicalString(c.req(), goldenTimestamp, goldenNonce)
			if string(got) != c.wantCanonical {
				t.Fatalf("hub canonical string:\n got %q\nwant %q", got, c.wantCanonical)
			}
			if shared := apiclient.BuildCanonicalString(c.req(), goldenTimestamp, goldenNonce); string(shared) != string(got) {
				t.Fatalf("hub and apiclient canonical strings differ:\n hub %q\n api %q", got, shared)
			}
			sig := base64.StdEncoding.EncodeToString(computeHMAC([]byte(goldenSecret), got))
			if sig != c.wantSignature {
				t.Fatalf("signature: got %q want %q", sig, c.wantSignature)
			}
		})
	}
}
