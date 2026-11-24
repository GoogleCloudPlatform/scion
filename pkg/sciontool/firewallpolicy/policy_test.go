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

package firewallpolicy

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestNormalize_Valid(t *testing.T) {
	tests := []struct {
		name     string
		domains  []string
		allowSSH bool
		want     Policy
	}{
		{
			name: "empty",
			want: Policy{},
		},
		{
			name:     "ssh only",
			allowSSH: true,
			want:     Policy{AllowSSH: true},
		},
		{
			name: "case folding, whitespace, trailing dot, dedupe, sort",
			domains: []string{
				"  PyPI.org. ",
				"files.pythonhosted.org",
				"pypi.org",
				"NEXUS.corp.example",
				"a-1.b-2.internal",
				"1.example.com",
			},
			allowSSH: true,
			want: Policy{
				AllowDomains: []string{
					"1.example.com",
					"a-1.b-2.internal",
					"files.pythonhosted.org",
					"nexus.corp.example",
					"pypi.org",
				},
				AllowSSH: true,
			},
		},
		{
			name:    "max length label (63) and max length domain (253)",
			domains: []string{strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 61)},
			want: Policy{
				AllowDomains: []string{strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 61)},
			},
		},
		{
			name:    "punycode IDN",
			domains: []string{"xn--bcher-kva.example"},
			want: Policy{
				AllowDomains: []string{"xn--bcher-kva.example"},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Normalize(tc.domains, tc.allowSSH)
			if err != nil {
				t.Fatalf("Normalize(%v, %v) unexpected error: %v", tc.domains, tc.allowSSH, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Normalize(%v, %v) = %#v, want %#v", tc.domains, tc.allowSSH, got, tc.want)
			}
		})
	}
}

func TestNormalize_RejectsInvalid(t *testing.T) {
	tooLongLabel := strings.Repeat("a", 64) + ".example.com"
	tooLongDomain := strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 62)

	cases := []struct {
		name  string
		entry string
	}{
		{"empty", ""},
		{"whitespace only", "   "},
		{"dot only", "."},
		{"double trailing dot", "example.com.."},
		{"leading dot", ".example.com"},
		{"empty interior label", "a..example.com"},
		{"wildcard", "*.example.com"},
		{"bare wildcard", "*"},
		{"ipv4 literal", "1.2.3.4"},
		{"ipv4 shorthand", "127.1"},
		{"numeric tld", "foo.123"},
		{"ipv6 literal", "2001:db8::1"},
		{"ipv6 bracketed", "[2001:db8::1]"},
		{"cidr", "10.0.0.0/8"},
		{"single label", "intranet"},
		{"label over 63", tooLongLabel},
		{"domain over 253", tooLongDomain},
		{"leading hyphen", "-a.example.com"},
		{"trailing hyphen", "a-.example.com"},
		{"leading hyphen second label", "a.-b.com"},
		{"trailing hyphen second label", "a.b-.com"},
		{"localhost", "localhost"},
		{"subdomain of localhost", "foo.localhost"},
		{"upper localhost", "API.LOCALHOST."},
		{"non-ascii", "bücher.example"},
		{"semicolon", "a.com;id"},
		{"command substitution", "$(id).example.com"},
		{"backtick", "`id`.example.com"},
		{"space", "a.com b.com"},
		{"tab", "a.com\tb.com"},
		{"newline", "a.com\nb.com"},
		{"comma inside entry", "a.com,b.com"},
		{"single quote", "a'b.example.com"},
		{"double quote", `a"b.example.com`},
		{"slash", "example.com/path"},
		{"colon port", "example.com:443"},
		{"percent", "example%20.com"},
		{"underscore", "my_host.example.com"},
		{"null byte", "a.example.com\x00"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Normalize([]string{tc.entry}, false)
			if err == nil {
				t.Fatalf("Normalize([%q]) = %#v, nil; want error", tc.entry, got)
			}
			if !got.IsEmpty() {
				t.Errorf("Normalize([%q]) returned non-empty policy %#v alongside error", tc.entry, got)
			}
		})
	}
}

func TestNormalize_MaxDomainsPerSource(t *testing.T) {
	okList := make([]string, MaxDomainsPerSource)
	for i := range okList {
		okList[i] = fmt.Sprintf("host-%02d.example.com", i)
	}
	got, err := Normalize(okList, false)
	if err != nil {
		t.Fatalf("Normalize(%d entries) unexpected error: %v", MaxDomainsPerSource, err)
	}
	if len(got.AllowDomains) != MaxDomainsPerSource {
		t.Fatalf("len(AllowDomains) = %d, want %d", len(got.AllowDomains), MaxDomainsPerSource)
	}

	// Duplicates that collapse to 64 unique entries are accepted.
	withDup := append(append([]string{}, okList...), okList[0])
	if _, err := Normalize(withDup, false); err != nil {
		t.Fatalf("Normalize(64 unique + 1 dup) unexpected error: %v", err)
	}

	// 65 unique entries are rejected.
	tooMany := append(append([]string{}, okList...), "host-64.example.com")
	if _, err := Normalize(tooMany, false); err == nil {
		t.Fatal("Normalize(65 unique entries) = nil, want error")
	}
}

func TestPolicyEnvAndFromEnv_RoundTrip(t *testing.T) {
	t.Run("empty policy", func(t *testing.T) {
		var p Policy
		if env := p.Env(); env != nil {
			t.Fatalf("Policy{}.Env() = %v, want nil", env)
		}
		got, err := FromEnv(func(string) (string, bool) { return "", false })
		if err != nil {
			t.Fatalf("FromEnv(empty) error = %v", err)
		}
		if !reflect.DeepEqual(got, p) {
			t.Fatalf("FromEnv(empty) = %#v, want %#v", got, p)
		}
	})

	t.Run("domains and ssh", func(t *testing.T) {
		p, err := Normalize([]string{"pypi.org", "files.pythonhosted.org"}, true)
		if err != nil {
			t.Fatalf("Normalize error: %v", err)
		}
		envList := p.Env()
		wantEnv := []string{
			EnvAllowDomains + "=files.pythonhosted.org,pypi.org",
			EnvAllowSSH + "=true",
		}
		if !reflect.DeepEqual(envList, wantEnv) {
			t.Fatalf("Env() = %v, want %v", envList, wantEnv)
		}
		envMap := make(map[string]string, len(envList))
		for _, kv := range envList {
			k, v, _ := strings.Cut(kv, "=")
			envMap[k] = v
		}
		got, err := FromEnv(func(k string) (string, bool) {
			v, ok := envMap[k]
			return v, ok
		})
		if err != nil {
			t.Fatalf("FromEnv error: %v", err)
		}
		if !reflect.DeepEqual(got, p) {
			t.Fatalf("FromEnv = %#v, want %#v", got, p)
		}
	})

	t.Run("fromenv allows up to 128 unioned domains", func(t *testing.T) {
		domains := make([]string, MaxDomainsTotal)
		for i := range domains {
			domains[i] = fmt.Sprintf("host-%03d.example.com", i)
		}
		got, err := FromEnv(func(k string) (string, bool) {
			if k == EnvAllowDomains {
				return strings.Join(domains, ","), true
			}
			return "", false
		})
		if err != nil {
			t.Fatalf("FromEnv(128 domains) error: %v", err)
		}
		if len(got.AllowDomains) != MaxDomainsTotal {
			t.Fatalf("len(AllowDomains) = %d, want %d", len(got.AllowDomains), MaxDomainsTotal)
		}
	})
}

func TestFromEnv_RejectsInvalid(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
	}{
		{
			name: "ssh yes",
			env:  map[string]string{EnvAllowSSH: "yes"},
		},
		{
			name: "ssh 1",
			env:  map[string]string{EnvAllowSSH: "1"},
		},
		{
			name: "ssh false",
			env:  map[string]string{EnvAllowSSH: "false"},
		},
		{
			name: "trailing comma",
			env:  map[string]string{EnvAllowDomains: "pypi.org,"},
		},
		{
			name: "leading comma",
			env:  map[string]string{EnvAllowDomains: ",pypi.org"},
		},
		{
			name: "double comma",
			env:  map[string]string{EnvAllowDomains: "pypi.org,,example.com"},
		},
		{
			name: "metacharacter in list",
			env:  map[string]string{EnvAllowDomains: "pypi.org,evil;id.com"},
		},
		{
			name: "whitespace only domains",
			env:  map[string]string{EnvAllowDomains: "   "},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := FromEnv(func(k string) (string, bool) {
				v, ok := tc.env[k]
				return v, ok
			})
			if err == nil {
				t.Fatalf("FromEnv(%v) = nil error, want error", tc.env)
			}
		})
	}
}

func TestUnion(t *testing.T) {
	a, err := Normalize([]string{"b.example.com", "a.example.com"}, false)
	if err != nil {
		t.Fatalf("Normalize(a): %v", err)
	}
	b, err := Normalize([]string{"c.example.com", "a.example.com"}, true)
	if err != nil {
		t.Fatalf("Normalize(b): %v", err)
	}
	got, err := Union(a, b)
	if err != nil {
		t.Fatalf("Union error: %v", err)
	}
	want := Policy{
		AllowDomains: []string{"a.example.com", "b.example.com", "c.example.com"},
		AllowSSH:     true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Union = %#v, want %#v", got, want)
	}

	// Exceeding 128 unique entries fails.
	a64 := make([]string, MaxDomainsPerSource)
	b65 := make([]string, MaxDomainsPerSource+1)
	for i := range a64 {
		a64[i] = fmt.Sprintf("a-%02d.example.com", i)
	}
	for i := range b65 {
		b65[i] = fmt.Sprintf("b-%02d.example.com", i)
	}
	if _, err := Union(Policy{AllowDomains: a64}, Policy{AllowDomains: b65}); err == nil {
		t.Fatal("Union(129 unique) = nil, want error")
	}
}
