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

package clicontextseed

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestRender(t *testing.T) {
	data, err := Render(Options{Endpoint: "HTTPS://Prod.Example.COM:443///"})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if strings.Contains(string(data), "transport") {
		t.Errorf("plain-endpoint seed must not contain transport:\n%s", data)
	}
	var doc seedDocument
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if doc.Version != Version || doc.CurrentHub != "prod" {
		t.Fatalf("doc = %+v", doc)
	}
	if h := doc.Hubs["prod"]; h == nil || h.Endpoint != "https://prod.example.com" {
		t.Fatalf("hubs[prod] = %+v", h)
	}
}

func TestRender_EmptyEndpoint(t *testing.T) {
	if _, err := Render(Options{Endpoint: "   "}); err == nil {
		t.Fatal("expected error for empty endpoint, got nil")
	}
}

func TestRender_WithTransport(t *testing.T) {
	for _, tc := range []struct {
		name         string
		mode         string
		audience     string
		wantMode     string
		wantAudience string
	}{
		{
			name:         "iap with audience",
			mode:         " iap ",
			audience:     " 12345.apps.googleusercontent.com ",
			wantMode:     "iap",
			wantAudience: "12345.apps.googleusercontent.com",
		},
		{
			name:         "cloudrun_invoker with audience",
			mode:         "cloudrun_invoker",
			audience:     "https://hub.example.com",
			wantMode:     "cloudrun_invoker",
			wantAudience: "https://hub.example.com",
		},
		{
			name:     "iap without audience",
			mode:     "iap",
			audience: "",
			wantMode: "iap",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := Render(Options{
				Endpoint: "https://hub.example.com/",
				Transport: &TransportOptions{
					Mode:     tc.mode,
					Audience: tc.audience,
				},
			})
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			var doc seedDocument
			if err := yaml.Unmarshal(data, &doc); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			h := doc.Hubs["hub"]
			if h == nil || h.Endpoint != "https://hub.example.com" {
				t.Fatalf("hubs[hub] = %+v", h)
			}
			if h.Transport == nil {
				t.Fatalf("expected transport block in:\n%s", data)
			}
			if h.Transport.Mode != tc.wantMode || h.Transport.Audience != tc.wantAudience {
				t.Errorf("transport = %+v, want {Mode:%q Audience:%q}", *h.Transport, tc.wantMode, tc.wantAudience)
			}
			if tc.wantAudience == "" && strings.Contains(string(data), "audience") {
				t.Errorf("empty audience should be omitted:\n%s", data)
			}
		})
	}
}

func TestRender_EmptyTransportOmitted(t *testing.T) {
	data, err := Render(Options{
		Endpoint:  "https://hub.example.com",
		Transport: &TransportOptions{Mode: "  ", Audience: "  "},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if strings.Contains(string(data), "transport") {
		t.Errorf("empty transport must be omitted:\n%s", data)
	}
}

func TestRender_UnsupportedTransportMode(t *testing.T) {
	if _, err := Render(Options{
		Endpoint:  "https://hub.example.com",
		Transport: &TransportOptions{Mode: "bogus"},
	}); err == nil {
		t.Fatal("expected error for unsupported transport mode, got nil")
	}
}
