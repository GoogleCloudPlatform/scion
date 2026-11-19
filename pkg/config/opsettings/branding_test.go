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

package opsettings

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/v2"
)

func TestBrandingSection_KoanfRoundTripAndValidation(t *testing.T) {
	k := koanf.New(".")
	err := k.Load(confmap.Provider(map[string]interface{}{
		"server.hub.branding.name":            "Acme Cloud",
		"server.hub.branding.short_name":      "Acme",
		"server.hub.branding.attribution":     "shown",
		"server.hub.branding.accent_color":    "#0d9488",
		"server.hub.branding.logo.light":      "https://cdn.example.com/acme.svg",
		"server.hub.branding.palette.primary": map[string]interface{}{"600": "#0f766e"},
	}, "."), nil)
	if err != nil {
		t.Fatalf("koanf load: %v", err)
	}

	doc, err := ExtractSectionFromKoanf(k, BrandingSectionName)
	if err != nil {
		t.Fatalf("ExtractSectionFromKoanf: %v", err)
	}
	if errs := Validate(BrandingSectionName, doc); len(errs) > 0 {
		t.Fatalf("Validate valid branding doc: %v", errs)
	}

	merged, err := LoadSectionsIntoKoanf(map[string]json.RawMessage{
		BrandingSectionName: doc,
	})
	if err != nil {
		t.Fatalf("LoadSectionsIntoKoanf: %v", err)
	}
	if got := merged.String("server.hub.branding.name"); got != "Acme Cloud" {
		t.Errorf("merged name = %q, want Acme Cloud", got)
	}
	if got := merged.String("server.hub.branding.logo.light"); got != "https://cdn.example.com/acme.svg" {
		t.Errorf("merged logo.light = %q", got)
	}
	if got := merged.String("server.hub.branding.palette.primary.600"); got != "#0f766e" {
		t.Errorf("merged palette.primary.600 = %q, want #0f766e", got)
	}

	// Failing WCAG AA contrast is rejected by Validate, while ValidateSchema passes syntax.
	badContrastDoc := json.RawMessage(`{"palette":{"light":{"primary":"#eab308"}}}`)
	if schemaErrs := ValidateSchema(BrandingSectionName, badContrastDoc); len(schemaErrs) > 0 {
		t.Fatalf("ValidateSchema should accept syntactically valid hexes: %v", schemaErrs)
	}
	crossErrs := Validate(BrandingSectionName, badContrastDoc)
	if len(crossErrs) == 0 {
		t.Fatal("Validate should reject low-contrast light.primary")
	}
	if !strings.Contains(crossErrs[0].Error(), "4.50:1") {
		t.Errorf("expected WCAG AA 4.50:1 error message, got: %v", crossErrs)
	}

	// Invalid shade step key is rejected by JSON schema.
	badStepDoc := json.RawMessage(`{"palette":{"primary":{"999":"#0d9488"}}}`)
	if errs := Validate(BrandingSectionName, badStepDoc); len(errs) == 0 {
		t.Fatal("Validate should reject unknown scale step 999")
	}

	//Verify server.hub.branding keys classify as Layer-1 under "branding".
	for _, kp := range BrandingKoanfPaths {
		if got := OwningSection(kp); got != BrandingSectionName {
			t.Errorf("OwningSection(%q) = %q, want %q", kp, got, BrandingSectionName)
		}
		if IsRestartRequired(kp) {
			t.Errorf("IsRestartRequired(%q) = true, want false (live reload)", kp)
		}
	}
	if got := OwningSection(config.BrandingSectionKey + ".palette.primary.500"); got != BrandingSectionName {
		t.Errorf("OwningSection(palette.primary.500) = %q, want %q", got, BrandingSectionName)
	}
}
