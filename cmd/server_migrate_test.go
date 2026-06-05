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

package cmd

import "testing"

func TestParseSQLiteSourceDSN(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		wantDSN  string
		wantPath string
		wantErr  bool
	}{
		{
			name:     "absolute sqlite url",
			in:       "sqlite:///var/lib/scion/hub.db",
			wantDSN:  "file:/var/lib/scion/hub.db?cache=shared",
			wantPath: "/var/lib/scion/hub.db",
		},
		{
			name:     "relative sqlite url",
			in:       "sqlite://data/hub.db",
			wantDSN:  "file:data/hub.db?cache=shared",
			wantPath: "data/hub.db",
		},
		{
			name:     "sqlite single-colon form",
			in:       "sqlite:/tmp/hub.db",
			wantDSN:  "file:/tmp/hub.db?cache=shared",
			wantPath: "/tmp/hub.db",
		},
		{
			name:     "file url with query",
			in:       "file:/tmp/hub.db?cache=shared",
			wantDSN:  "file:/tmp/hub.db?cache=shared",
			wantPath: "/tmp/hub.db",
		},
		{
			name:     "file url with triple slashes",
			in:       "file:///tmp/hub.db",
			wantDSN:  "file:/tmp/hub.db?cache=shared",
			wantPath: "/tmp/hub.db",
		},
		{
			name:     "bare path",
			in:       "/tmp/hub.db",
			wantDSN:  "file:/tmp/hub.db?cache=shared",
			wantPath: "/tmp/hub.db",
		},
		{
			name:    "empty",
			in:      "",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dsn, path, err := parseSQLiteSourceDSN(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got dsn=%q path=%q", dsn, path)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if dsn != tt.wantDSN {
				t.Errorf("dsn = %q, want %q", dsn, tt.wantDSN)
			}
			if path != tt.wantPath {
				t.Errorf("path = %q, want %q", path, tt.wantPath)
			}
		})
	}
}

func TestParsePostgresDestDSN(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{name: "url form", in: "postgres://u:p@host:5432/db?sslmode=require"},
		{name: "postgresql scheme", in: "postgresql://u:p@host:5432/db"},
		{name: "keyword form", in: "host=h port=5432 user=u password=p dbname=db sslmode=require"},
		{name: "empty", in: "", wantErr: true},
		{name: "not postgres", in: "sqlite:///tmp/hub.db", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parsePostgresDestDSN(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.in {
				t.Errorf("dsn = %q, want unchanged %q", got, tt.in)
			}
		})
	}
}

func TestServerMigrateCmdRegistered(t *testing.T) {
	for _, c := range serverCmd.Commands() {
		if c.Name() == "migrate" {
			return
		}
	}
	t.Fatal("migrate subcommand not registered under server")
}
