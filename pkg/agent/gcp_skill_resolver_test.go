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

package agent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

func TestGCPSkillResolver_HappyPath(t *testing.T) {
	skillContent := "# My Skill\nDoes things."
	skillHash := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(skillContent)))

	mux := http.NewServeMux()
	mux.HandleFunc("/skills/my-skill", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(gcpSkillResponse{
			Name:    "my-skill",
			Version: "1.0.0",
			Files: []gcpSkillFile{
				{Path: "SKILL.md", URL: "PLACEHOLDER_SKILL_URL"},
			},
		})
	})
	mux.HandleFunc("/files/SKILL.md", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(skillContent))
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	// Fix up the file URL now that we have the server address.
	origHandler := mux
	fixupMux := http.NewServeMux()
	fixupMux.HandleFunc("/skills/my-skill", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(gcpSkillResponse{
			Name:    "my-skill",
			Version: "1.0.0",
			Files: []gcpSkillFile{
				{Path: "SKILL.md", URL: server.URL + "/files/SKILL.md"},
			},
		})
	})
	fixupMux.HandleFunc("/files/SKILL.md", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(skillContent))
	})
	_ = origHandler
	server.Config.Handler = fixupMux

	resolver := &GCPSkillResolver{
		registryLookup: func(_ context.Context, name string) (*RegistryLookupResult, error) {
			if name == "team-skills" {
				return &RegistryLookupResult{
					Name:     "team-skills",
					Endpoint: server.URL + "/skills",
					Type:     "gcp",
					Status:   "active",
				}, nil
			}
			return nil, fmt.Errorf("not found")
		},
		httpClient:  server.Client(),
		tokenSource: func(context.Context) (string, error) { return "test-token", nil },
	}

	refs := []api.SkillReference{{URI: "gcp-skill://team-skills/my-skill"}}
	result, err := resolver.Resolve(context.Background(), refs, ResolveOpts{})
	if err != nil {
		t.Fatalf("Resolve() error: %v", err)
	}
	if len(result.Errors) != 0 {
		t.Fatalf("Resolve() got %d errors: %v", len(result.Errors), result.Errors)
	}
	if len(result.Resolved) != 1 {
		t.Fatalf("Resolve() got %d resolved, want 1", len(result.Resolved))
	}

	rs := result.Resolved[0]
	if rs.Name != "my-skill" {
		t.Errorf("Name = %q, want %q", rs.Name, "my-skill")
	}
	if rs.Version != "1.0.0" {
		t.Errorf("Version = %q, want %q", rs.Version, "1.0.0")
	}
	if len(rs.Files) != 1 {
		t.Fatalf("Files count = %d, want 1", len(rs.Files))
	}
	if rs.Files[0].Path != "SKILL.md" {
		t.Errorf("Files[0].Path = %q, want %q", rs.Files[0].Path, "SKILL.md")
	}
	if rs.Files[0].Hash != skillHash {
		t.Errorf("Files[0].Hash = %q, want %q", rs.Files[0].Hash, skillHash)
	}

	expectedBundleHash := transfer.ComputeContentHash([]transfer.FileInfo{
		{Path: "SKILL.md", Hash: skillHash},
	})
	if rs.Hash != expectedBundleHash {
		t.Errorf("Hash = %q, want %q", rs.Hash, expectedBundleHash)
	}
}

func TestGCPSkillResolver_MultipleFiles(t *testing.T) {
	skillContent := "# Skill"
	configContent := `{"key": "value"}`

	mux := http.NewServeMux()
	var server *httptest.Server

	mux.HandleFunc("/skills/multi-file", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(gcpSkillResponse{
			Name:    "multi-file",
			Version: "2.0.0",
			Files: []gcpSkillFile{
				{Path: "SKILL.md", URL: server.URL + "/files/SKILL.md"},
				{Path: "config.json", URL: server.URL + "/files/config.json"},
			},
		})
	})
	mux.HandleFunc("/files/SKILL.md", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(skillContent))
	})
	mux.HandleFunc("/files/config.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(configContent))
	})

	server = httptest.NewServer(mux)
	defer server.Close()

	resolver := &GCPSkillResolver{
		registryLookup: func(_ context.Context, name string) (*RegistryLookupResult, error) {
			return &RegistryLookupResult{
				Name: name, Endpoint: server.URL + "/skills", Type: "gcp", Status: "active",
			}, nil
		},
		httpClient:  server.Client(),
		tokenSource: func(context.Context) (string, error) { return "tok", nil },
	}

	result, err := resolver.Resolve(context.Background(), []api.SkillReference{
		{URI: "gcp-skill://reg/multi-file"},
	}, ResolveOpts{})
	if err != nil {
		t.Fatalf("Resolve() error: %v", err)
	}
	if len(result.Errors) != 0 {
		t.Fatalf("unexpected errors: %v", result.Errors)
	}
	if len(result.Resolved) != 1 {
		t.Fatalf("got %d resolved, want 1", len(result.Resolved))
	}
	if len(result.Resolved[0].Files) != 2 {
		t.Errorf("got %d files, want 2", len(result.Resolved[0].Files))
	}
}

func TestGCPSkillResolver_UnknownAlias(t *testing.T) {
	resolver := &GCPSkillResolver{
		registryLookup: func(_ context.Context, name string) (*RegistryLookupResult, error) {
			return nil, fmt.Errorf("registry %q not found", name)
		},
		httpClient:  http.DefaultClient,
		tokenSource: func(context.Context) (string, error) { return "tok", nil },
	}

	result, err := resolver.Resolve(context.Background(), []api.SkillReference{
		{URI: "gcp-skill://unknown-alias/some-skill"},
	}, ResolveOpts{})
	if err != nil {
		t.Fatalf("Resolve() hard error: %v", err)
	}
	if len(result.Errors) != 1 {
		t.Fatalf("got %d errors, want 1", len(result.Errors))
	}
	if result.Errors[0].Code != "resolve_failed" {
		t.Errorf("error code = %q, want %q", result.Errors[0].Code, "resolve_failed")
	}
}

func TestGCPSkillResolver_DisabledRegistry(t *testing.T) {
	resolver := &GCPSkillResolver{
		registryLookup: func(_ context.Context, _ string) (*RegistryLookupResult, error) {
			return &RegistryLookupResult{
				Name: "disabled-reg", Endpoint: "https://example.com", Type: "gcp", Status: "disabled",
			}, nil
		},
		httpClient:  http.DefaultClient,
		tokenSource: func(context.Context) (string, error) { return "tok", nil },
	}

	result, err := resolver.Resolve(context.Background(), []api.SkillReference{
		{URI: "gcp-skill://disabled-reg/skill"},
	}, ResolveOpts{})
	if err != nil {
		t.Fatalf("Resolve() hard error: %v", err)
	}
	if len(result.Errors) != 1 {
		t.Fatalf("got %d errors, want 1", len(result.Errors))
	}
	if got := result.Errors[0].Message; got == "" {
		t.Error("expected non-empty error message")
	}
}

func TestGCPSkillResolver_WrongType(t *testing.T) {
	resolver := &GCPSkillResolver{
		registryLookup: func(_ context.Context, _ string) (*RegistryLookupResult, error) {
			return &RegistryLookupResult{
				Name: "hub-reg", Endpoint: "https://example.com", Type: "hub", Status: "active",
			}, nil
		},
		httpClient:  http.DefaultClient,
		tokenSource: func(context.Context) (string, error) { return "tok", nil },
	}

	result, err := resolver.Resolve(context.Background(), []api.SkillReference{
		{URI: "gcp-skill://hub-reg/skill"},
	}, ResolveOpts{})
	if err != nil {
		t.Fatalf("Resolve() hard error: %v", err)
	}
	if len(result.Errors) != 1 {
		t.Fatalf("got %d errors, want 1", len(result.Errors))
	}
	if got := result.Errors[0].Message; got == "" {
		t.Error("expected error mentioning wrong type")
	}
}

func TestGCPSkillResolver_GCP404(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	resolver := &GCPSkillResolver{
		registryLookup: func(_ context.Context, _ string) (*RegistryLookupResult, error) {
			return &RegistryLookupResult{
				Name: "reg", Endpoint: server.URL + "/skills", Type: "gcp", Status: "active",
			}, nil
		},
		httpClient:  server.Client(),
		tokenSource: func(context.Context) (string, error) { return "tok", nil },
	}

	result, err := resolver.Resolve(context.Background(), []api.SkillReference{
		{URI: "gcp-skill://reg/missing-skill"},
	}, ResolveOpts{})
	if err != nil {
		t.Fatalf("Resolve() hard error: %v", err)
	}
	if len(result.Errors) != 1 {
		t.Fatalf("got %d errors, want 1", len(result.Errors))
	}
}

func TestGCPSkillResolver_GCP403(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()

	resolver := &GCPSkillResolver{
		registryLookup: func(_ context.Context, _ string) (*RegistryLookupResult, error) {
			return &RegistryLookupResult{
				Name: "reg", Endpoint: server.URL + "/skills", Type: "gcp", Status: "active",
			}, nil
		},
		httpClient:  server.Client(),
		tokenSource: func(context.Context) (string, error) { return "tok", nil },
	}

	result, err := resolver.Resolve(context.Background(), []api.SkillReference{
		{URI: "gcp-skill://reg/denied-skill"},
	}, ResolveOpts{})
	if err != nil {
		t.Fatalf("Resolve() hard error: %v", err)
	}
	if len(result.Errors) != 1 {
		t.Fatalf("got %d errors, want 1", len(result.Errors))
	}
	if got := result.Errors[0].Message; got == "" {
		t.Error("expected error mentioning permissions")
	}
}

func TestGCPSkillResolver_ADCNotConfigured(t *testing.T) {
	resolver := &GCPSkillResolver{
		registryLookup: func(_ context.Context, _ string) (*RegistryLookupResult, error) {
			return &RegistryLookupResult{
				Name: "reg", Endpoint: "https://example.com/skills", Type: "gcp", Status: "active",
			}, nil
		},
		httpClient: http.DefaultClient,
		tokenSource: func(context.Context) (string, error) {
			return "", fmt.Errorf("no GCP credentials found")
		},
	}

	result, err := resolver.Resolve(context.Background(), []api.SkillReference{
		{URI: "gcp-skill://reg/skill"},
	}, ResolveOpts{})
	if err != nil {
		t.Fatalf("Resolve() hard error: %v", err)
	}
	if len(result.Errors) != 1 {
		t.Fatalf("got %d errors, want 1", len(result.Errors))
	}
}

func TestGCPSkillResolver_EmptySkillFiles(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(gcpSkillResponse{
			Name:    "empty-skill",
			Version: "1.0.0",
			Files:   []gcpSkillFile{},
		})
	}))
	defer server.Close()

	resolver := &GCPSkillResolver{
		registryLookup: func(_ context.Context, _ string) (*RegistryLookupResult, error) {
			return &RegistryLookupResult{
				Name: "reg", Endpoint: server.URL + "/skills", Type: "gcp", Status: "active",
			}, nil
		},
		httpClient:  server.Client(),
		tokenSource: func(context.Context) (string, error) { return "tok", nil },
	}

	result, err := resolver.Resolve(context.Background(), []api.SkillReference{
		{URI: "gcp-skill://reg/empty-skill"},
	}, ResolveOpts{})
	if err != nil {
		t.Fatalf("Resolve() hard error: %v", err)
	}
	if len(result.Errors) != 1 {
		t.Fatalf("got %d errors, want 1", len(result.Errors))
	}
}

func TestGCPSkillResolver_InvalidURI(t *testing.T) {
	resolver := &GCPSkillResolver{
		registryLookup: func(_ context.Context, _ string) (*RegistryLookupResult, error) {
			return nil, fmt.Errorf("should not be called")
		},
		httpClient:  http.DefaultClient,
		tokenSource: func(context.Context) (string, error) { return "tok", nil },
	}

	result, err := resolver.Resolve(context.Background(), []api.SkillReference{
		{URI: "gcp-skill://alias"},
	}, ResolveOpts{})
	if err != nil {
		t.Fatalf("Resolve() hard error: %v", err)
	}
	if len(result.Errors) != 1 {
		t.Fatalf("got %d errors, want 1", len(result.Errors))
	}
	if result.Errors[0].Code != "invalid_uri" {
		t.Errorf("error code = %q, want %q", result.Errors[0].Code, "invalid_uri")
	}
}

func TestGCPSkillResolver_AsAlias(t *testing.T) {
	mux := http.NewServeMux()
	var server *httptest.Server

	mux.HandleFunc("/skills/my-skill", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(gcpSkillResponse{
			Name:    "my-skill",
			Version: "1.0.0",
			Files: []gcpSkillFile{
				{Path: "SKILL.md", URL: server.URL + "/files/SKILL.md"},
			},
		})
	})
	mux.HandleFunc("/files/SKILL.md", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("# content"))
	})

	server = httptest.NewServer(mux)
	defer server.Close()

	resolver := &GCPSkillResolver{
		registryLookup: func(_ context.Context, _ string) (*RegistryLookupResult, error) {
			return &RegistryLookupResult{
				Name: "reg", Endpoint: server.URL + "/skills", Type: "gcp", Status: "active",
			}, nil
		},
		httpClient:  server.Client(),
		tokenSource: func(context.Context) (string, error) { return "tok", nil },
	}

	result, err := resolver.Resolve(context.Background(), []api.SkillReference{
		{URI: "gcp-skill://reg/my-skill", As: "custom-name"},
	}, ResolveOpts{})
	if err != nil {
		t.Fatalf("Resolve() error: %v", err)
	}
	if len(result.Resolved) != 1 {
		t.Fatalf("got %d resolved, want 1", len(result.Resolved))
	}
	if result.Resolved[0].As != "custom-name" {
		t.Errorf("As = %q, want %q", result.Resolved[0].As, "custom-name")
	}
}

func TestGCPSkillResolver_VersionMismatch(t *testing.T) {
	mux := http.NewServeMux()
	var server *httptest.Server

	mux.HandleFunc("/skills/my-skill", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(gcpSkillResponse{
			Name:    "my-skill",
			Version: "v3",
			Files: []gcpSkillFile{
				{Path: "SKILL.md", URL: server.URL + "/files/SKILL.md"},
			},
		})
	})

	server = httptest.NewServer(mux)
	defer server.Close()

	resolver := &GCPSkillResolver{
		registryLookup: func(_ context.Context, _ string) (*RegistryLookupResult, error) {
			return &RegistryLookupResult{
				Name: "reg", Endpoint: server.URL + "/skills", Type: "gcp", Status: "active",
			}, nil
		},
		httpClient:  server.Client(),
		tokenSource: func(context.Context) (string, error) { return "tok", nil },
	}

	result, err := resolver.Resolve(context.Background(), []api.SkillReference{
		{URI: "gcp-skill://reg/my-skill@v2"},
	}, ResolveOpts{})
	if err != nil {
		t.Fatalf("Resolve() hard error: %v", err)
	}
	if len(result.Errors) != 1 {
		t.Fatalf("got %d errors, want 1", len(result.Errors))
	}
	if !strings.Contains(result.Errors[0].Message, "v2") || !strings.Contains(result.Errors[0].Message, "v3") {
		t.Errorf("error message should mention both versions, got: %s", result.Errors[0].Message)
	}
}

func TestGCPSkillResolver_SSRFBlocked(t *testing.T) {
	mux := http.NewServeMux()
	var server *httptest.Server

	mux.HandleFunc("/skills/evil-skill", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(gcpSkillResponse{
			Name:    "evil-skill",
			Version: "1.0.0",
			Files: []gcpSkillFile{
				{Path: "SKILL.md", URL: "http://169.254.169.254/computeMetadata/v1/"},
			},
		})
	})

	server = httptest.NewServer(mux)
	defer server.Close()

	resolver := &GCPSkillResolver{
		registryLookup: func(_ context.Context, _ string) (*RegistryLookupResult, error) {
			return &RegistryLookupResult{
				Name: "reg", Endpoint: server.URL + "/skills", Type: "gcp", Status: "active",
			}, nil
		},
		httpClient:  server.Client(),
		tokenSource: func(context.Context) (string, error) { return "tok", nil },
	}

	result, err := resolver.Resolve(context.Background(), []api.SkillReference{
		{URI: "gcp-skill://reg/evil-skill"},
	}, ResolveOpts{})
	if err != nil {
		t.Fatalf("Resolve() hard error: %v", err)
	}
	if len(result.Errors) != 1 {
		t.Fatalf("got %d errors, want 1", len(result.Errors))
	}
	if !strings.Contains(result.Errors[0].Message, "unsafe file URL") {
		t.Errorf("error should mention unsafe file URL, got: %s", result.Errors[0].Message)
	}
}

func TestGCPSkillResolver_SSRFCrossHost(t *testing.T) {
	mux := http.NewServeMux()
	var server *httptest.Server

	mux.HandleFunc("/skills/cross-host", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(gcpSkillResponse{
			Name:    "cross-host",
			Version: "1.0.0",
			Files: []gcpSkillFile{
				{Path: "SKILL.md", URL: "https://evil.example.com/malicious"},
			},
		})
	})

	server = httptest.NewServer(mux)
	defer server.Close()

	resolver := &GCPSkillResolver{
		registryLookup: func(_ context.Context, _ string) (*RegistryLookupResult, error) {
			return &RegistryLookupResult{
				Name: "reg", Endpoint: server.URL + "/skills", Type: "gcp", Status: "active",
			}, nil
		},
		httpClient:  server.Client(),
		tokenSource: func(context.Context) (string, error) { return "tok", nil },
	}

	result, err := resolver.Resolve(context.Background(), []api.SkillReference{
		{URI: "gcp-skill://reg/cross-host"},
	}, ResolveOpts{})
	if err != nil {
		t.Fatalf("Resolve() hard error: %v", err)
	}
	if len(result.Errors) != 1 {
		t.Fatalf("got %d errors, want 1", len(result.Errors))
	}
	if !strings.Contains(result.Errors[0].Message, "does not match") {
		t.Errorf("error should mention host mismatch, got: %s", result.Errors[0].Message)
	}
}

func TestGCPSkillResolver_ResolverName(t *testing.T) {
	r := NewGCPSkillResolver(nil)
	if got := r.ResolverName(); got != "gcp" {
		t.Errorf("ResolverName() = %q, want %q", got, "gcp")
	}
}
