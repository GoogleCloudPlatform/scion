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

package hubprofile

import (
	"github.com/GoogleCloudPlatform/scion/pkg/hubprofile/hubprofiletest"
)

const (
	testIssuerSA    = hubprofiletest.IssuerSA
	testUATSA       = hubprofiletest.UATSA
	testIAPAudience = hubprofiletest.IAPAudience
	testIssuerAud   = hubprofiletest.IssuerAudience
	testDelegation  = hubprofiletest.Delegation
	testAccessToken = hubprofiletest.AccessToken
)

var jwtOf = hubprofiletest.JWT

// testDeps returns session dependencies wired to the fakes; env entries
// override the fakes' assign-mode environment.
func testDeps(g *hubprofiletest.GCP, clk *hubprofiletest.Clock, env map[string]string) Deps {
	full := g.Env()
	for k, v := range env {
		full[k] = v
	}
	return Deps{
		Getenv:            func(k string) string { return full[k] },
		IAMCredentialsURL: g.IAM.URL,
		Now:               clk.Now,
	}
}

// testProfile returns a valid profile for target.
func testProfile(target *hubprofiletest.Target, transport string) *Profile {
	p := &Profile{Name: "t1", Endpoint: target.Srv.URL, Transport: transport, IssuerServiceAccount: testIssuerSA, IssuerDelegations: map[string]string{TierMember: testDelegation}}
	if transport == TransportIAP {
		p.IAPAudience = testIAPAudience
		p.UATServiceAccount = testUATSA
	}
	return p
}
