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
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func cspLimiterRequest(remoteAddr string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/csp-reports", nil)
	r.RemoteAddr = remoteAddr
	return r
}

// TestCSPReportLimiterPerClientIP: one client IP gets its burst, then is
// refused, while another IP is still served.
func TestCSPReportLimiterPerClientIP(t *testing.T) {
	l := newCSPReportLimiter(nil)
	now := time.Unix(1_700_000_000, 0)
	l.perIP.nowFunc = func() time.Time { return now }
	l.global.nowFunc = func() time.Time { return now }

	for i := 0; i < cspReportBurst; i++ {
		if ok, _ := l.Allow(cspLimiterRequest("192.0.2.1:1234")); !ok {
			t.Fatalf("request %d within the burst was refused", i+1)
		}
	}
	ok, retry := l.Allow(cspLimiterRequest("192.0.2.1:1234"))
	if ok {
		t.Fatal("request past the per-IP burst was allowed")
	}
	if retry < 1 {
		t.Errorf("retry after = %d, want at least 1", retry)
	}
	// Positive control: another client is unaffected.
	if ok, _ := l.Allow(cspLimiterRequest("192.0.2.2:1234")); !ok {
		t.Fatal("another client IP was refused")
	}
	// And the first client is served again once its bucket refills.
	now = now.Add(time.Duration(float64(time.Second) / cspReportRatePerSecond))
	if ok, _ := l.Allow(cspLimiterRequest("192.0.2.1:1234")); !ok {
		t.Fatal("the first client was still refused after the refill interval")
	}
}

// TestCSPReportLimiterHubWide: the hub-wide record budget gives its burst
// and then refuses, whatever the clients; client requests are charged to
// the per-client buckets only.
func TestCSPReportLimiterHubWide(t *testing.T) {
	l := newCSPReportLimiter(nil)
	now := time.Unix(1_700_000_000, 0)
	l.perIP.nowFunc = func() time.Time { return now }
	l.global.nowFunc = func() time.Time { return now }

	for i := 0; i < cspReportGlobalBurst; i++ {
		if ok, _ := l.AllowRecord(); !ok {
			t.Fatalf("record %d within the hub-wide burst was refused", i+1)
		}
	}
	ok, retry := l.AllowRecord()
	if ok {
		t.Fatal("a record past the hub-wide burst was allowed")
	}
	if retry < 1 {
		t.Errorf("retry after = %d, want at least 1", retry)
	}
	// Positive control: a client request still passes its own bucket.
	if ok, _ := l.Allow(cspLimiterRequest("192.0.2.200:80")); !ok {
		t.Fatal("a client request was refused by the exhausted record budget")
	}
	// And the record budget refills at its rate.
	now = now.Add(time.Duration(float64(time.Second) / cspReportGlobalRatePerSecond))
	if ok, _ := l.AllowRecord(); !ok {
		t.Fatal("the record budget did not refill")
	}
}

// TestCSPReportLimiterGroupsIPv6By64: the addresses of one IPv6 /64 share
// a bucket, so one host cannot get a fresh bucket per address; another
// /64 has its own.
func TestCSPReportLimiterGroupsIPv6By64(t *testing.T) {
	l := newCSPReportLimiter(nil)
	now := time.Unix(1_700_000_000, 0)
	l.perIP.nowFunc = func() time.Time { return now }
	l.global.nowFunc = func() time.Time { return now }

	for i := 0; i < cspReportBurst; i++ {
		addr := fmt.Sprintf("[2001:db8:1:2::%x]:443", i+1)
		if ok, _ := l.Allow(cspLimiterRequest(addr)); !ok {
			t.Fatalf("request %d within the burst was refused", i+1)
		}
	}
	if ok, _ := l.Allow(cspLimiterRequest("[2001:db8:1:2:ffff::1]:443")); ok {
		t.Fatal("another address in the same /64 got a fresh bucket")
	}
	// Positive control: an address in another /64 is served.
	if ok, _ := l.Allow(cspLimiterRequest("[2001:db8:1:3::1]:443")); !ok {
		t.Fatal("an address in another /64 was refused")
	}
	if got := l.perIP.Len(); got != 2 {
		t.Errorf("tracked %d buckets, want 2 (one per /64)", got)
	}
}

// TestCSPReportHandlerRawSamplesFollowsSetting checks that the report
// handler logs raw samples only when server.hub.csp_report_raw_samples is
// on: off by default, on when the setting is.
func TestCSPReportHandlerRawSamplesFollowsSetting(t *testing.T) {
	l := newCSPReportLimiter(nil)
	if newCSPReportHandler(l, false).RawSamples {
		t.Fatal("RawSamples is on with the setting off")
	}
	if !newCSPReportHandler(l, true).RawSamples {
		t.Fatal("RawSamples is off with the setting on")
	}
}
