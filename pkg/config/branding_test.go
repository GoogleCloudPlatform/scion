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

package config

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"math"
	"reflect"
	"strings"
	"testing"
)

func minimalPNGDataURI(t *testing.T) string {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.NRGBA{R: 13, G: 148, B: 136, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}

func svgDataURI(svg string) string {
	return "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString([]byte(svg))
}

func TestDefaultResolvedBranding_Uncustomized(t *testing.T) {
	def := DefaultResolvedBranding()
	if def.Name != DefaultBrandName {
		t.Fatalf("Name = %q, want %q", def.Name, DefaultBrandName)
	}
	if def.ShortName != DefaultBrandName {
		t.Fatalf("ShortName = %q, want %q", def.ShortName, DefaultBrandName)
	}
	if def.Attribution != AttributionShown {
		t.Fatalf("Attribution = %q, want %q", def.Attribution, AttributionShown)
	}
	if def.IsCustomized() {
		t.Fatal("default branding should not report IsCustomized() == true")
	}
	if def.Public() != nil {
		t.Fatalf("default branding Public() = %+v, want nil", def.Public())
	}

	resolved, errs := ResolveBranding(nil)
	if len(errs) != 0 {
		t.Fatalf("ResolveBranding(nil) errs = %v", errs)
	}
	if resolved.IsCustomized() || resolved.Public() != nil {
		t.Fatal("ResolveBranding(nil) should be uncustomized with nil Public()")
	}
}

func TestPublicBranding_HasZeroBooleanFields(t *testing.T) {
	visited := map[reflect.Type]bool{}
	var check func(typ reflect.Type, path string)
	check = func(typ reflect.Type, path string) {
		for typ.Kind() == reflect.Pointer {
			typ = typ.Elem()
		}
		if typ.Kind() == reflect.Bool {
			t.Errorf("PublicBranding contains forbidden boolean field at %s", path)
			return
		}
		switch typ.Kind() {
		case reflect.Struct:
			if visited[typ] {
				return
			}
			visited[typ] = true
			for i := 0; i < typ.NumField(); i++ {
				f := typ.Field(i)
				if !f.IsExported() {
					continue
				}
				check(f.Type, path+"."+f.Name)
			}
		case reflect.Map:
			check(typ.Key(), path+"[key]")
			check(typ.Elem(), path+"[val]")
		case reflect.Slice, reflect.Array:
			check(typ.Elem(), path+"[]")
		}
	}
	check(reflect.TypeOf(PublicBranding{}), "PublicBranding")
}

func TestGenerateOKLCHScale_LuminanceAnchorsAcrossHues(t *testing.T) {
	accents := []string{
		"#0d9488", // teal
		"#d97706", // amber
		"#eab308", // yellow
		"#3b82f6", // blue
		"#1e3a8a", // dark navy
		"#7c3aed", // violet
		"#dc2626", // red
		"#64748b", // slate
		"#555555", // neutral gray
	}
	for _, accent := range accents {
		t.Run(accent, func(t *testing.T) {
			scale, err := GenerateOKLCHScale(accent)
			if err != nil {
				t.Fatalf("GenerateOKLCHScale(%q): %v", accent, err)
			}
			prevY := 1.1
			for _, anchor := range oklchShadeAnchors {
				hexVal, ok := scale[anchor.step]
				if !ok {
					t.Fatalf("missing step %q", anchor.step)
				}
				if !IsValidHexColor(hexVal) {
					t.Fatalf("step %s = %q is not valid #rrggbb", anchor.step, hexVal)
				}
				y, err := RelativeLuminance(hexVal)
				if err != nil {
					t.Fatalf("RelativeLuminance(%q): %v", hexVal, err)
				}
				if y >= prevY {
					t.Errorf("step %s luminance %.4f is not strictly darker than previous %.4f", anchor.step, y, prevY)
				}
				prevY = y
				// 8-bit sRGB rounding introduces at most ~0.015 luminance quantization near white.
				if math.Abs(y-anchor.targetY) > 0.02 {
					t.Errorf("step %s (%s) luminance = %.4f, want %.4f ± 0.02", anchor.step, hexVal, y, anchor.targetY)
				}
			}
		})
	}
}

func TestResolveBranding_MidToneAccentsAutoSelectForeground(t *testing.T) {
	cases := []struct {
		name           string
		accent         string
		wantDarkPrimFg string
	}{
		{name: "teal", accent: "#0d9488", wantDarkPrimFg: "#0f172a"},
		{name: "amber", accent: "#d97706", wantDarkPrimFg: "#0f172a"},
		{name: "yellow", accent: "#eab308", wantDarkPrimFg: "#0f172a"},
		{name: "navy", accent: "#1e3a8a", wantDarkPrimFg: "#0f172a"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resolved, errs := ResolveBranding(&BrandingConfig{
				Name:        "Acme Cloud Platform",
				AccentColor: tc.accent,
			})
			if len(errs) > 0 {
				t.Fatalf("unexpected validation errors for accent %s: %v", tc.accent, errs)
			}
			if !resolved.IsCustomized() || !resolved.HasCustomPalette {
				t.Fatal("expected IsCustomized and HasCustomPalette to be true")
			}
			if resolved.Dark.PrimaryText != tc.wantDarkPrimFg {
				t.Errorf("Dark.PrimaryText = %q, want %q", resolved.Dark.PrimaryText, tc.wantDarkPrimFg)
			}
			if r, _ := ContrastRatio(resolved.Light.PrimaryText, resolved.Light.Primary); r < 4.5 {
				t.Errorf("light primary_text contrast = %.2f:1, want >= 4.5", r)
			}
			if r, _ := ContrastRatio(resolved.Dark.PrimaryText, resolved.Dark.Primary); r < 4.5 {
				t.Errorf("dark primary_text contrast = %.2f:1, want >= 4.5", r)
			}
			pub := resolved.Public()
			if pub == nil {
				t.Fatal("expected non-nil PublicBranding")
			}
			if pub.CSSText == "" || pub.Light == nil || pub.Dark == nil || len(pub.Scales) != 5 {
				t.Fatalf("incomplete PublicBranding palette fields: %+v", pub)
			}
		})
	}
}

func TestValidateBranding_WCAGContrastRejections(t *testing.T) {
	cases := []struct {
		name        string
		cfg         BrandingConfig
		wantPathSub string
		wantMsgSub  string
	}{
		{
			name: "light primary low contrast on white surface",
			cfg: BrandingConfig{
				Palette: &BrandingPaletteConfig{
					Light: &BrandingSemanticMode{
						Primary: "#eab308",
					},
				},
			},
			wantPathSub: "server.hub.branding.palette.light.primary",
			wantMsgSub:  "minimum WCAG AA is 4.50:1",
		},
		{
			name: "explicit white primary_text on bright dark-mode primary",
			cfg: BrandingConfig{
				AccentColor: "#d97706",
				Palette: &BrandingPaletteConfig{
					Dark: &BrandingSemanticMode{
						PrimaryText: "#ffffff",
					},
				},
			},
			wantPathSub: "server.hub.branding.palette.dark.primary_text",
			wantMsgSub:  "minimum WCAG AA is 4.50:1",
		},
		{
			name: "light text_muted too faint against surface",
			cfg: BrandingConfig{
				Palette: &BrandingPaletteConfig{
					Light: &BrandingSemanticMode{
						TextMuted: "#94a3b8",
					},
				},
			},
			wantPathSub: "server.hub.branding.palette.light.text_muted",
			wantMsgSub:  "minimum WCAG AA is 4.50:1",
		},
		{
			name: "dead-zone primary-600 fails Shoelace button contrast",
			cfg: BrandingConfig{
				Palette: &BrandingPaletteConfig{
					Primary: BrandingScale{
						"600": "#777777",
					},
				},
			},
			wantPathSub: "server.hub.branding.palette.primary.600",
			wantMsgSub:  "minimum WCAG AA is 4.50:1",
		},
		{
			name: "neutral-600 fails 3.0 UI border contrast on light surface",
			cfg: BrandingConfig{
				Palette: &BrandingPaletteConfig{
					Neutral: BrandingScale{
						"600": "#cbd5e1",
					},
				},
			},
			wantPathSub: "server.hub.branding.palette.neutral.600",
			wantMsgSub:  "3.00:1",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := ValidateBranding(&tc.cfg)
			if len(errs) == 0 {
				t.Fatal("expected WCAG validation error, got none")
			}
			matched := false
			for _, e := range errs {
				if strings.Contains(e.Path, tc.wantPathSub) && strings.Contains(e.Message, tc.wantMsgSub) {
					matched = true
					break
				}
			}
			if !matched {
				t.Fatalf("errors %v did not contain path %q and message %q", errs, tc.wantPathSub, tc.wantMsgSub)
			}

			fallback, resolveErrs := ResolveBranding(&tc.cfg)
			if len(resolveErrs) == 0 {
				t.Fatal("expected ResolveBranding errors")
			}
			if !fallback.Malformed || fallback.IsCustomized() || fallback.Name != DefaultBrandName {
				t.Fatalf("expected Malformed stock fallback, got %+v", fallback)
			}
		})
	}
}

func TestValidateBranding_AssetsAndSVGSafety(t *testing.T) {
	pngURI := minimalPNGDataURI(t)
	safeSVG := svgDataURI(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24"><circle cx="12" cy="12" r="10" fill="#0d9488"/></svg>`)

	resolved, errs := ResolveBranding(&BrandingConfig{
		Name:     "Acme Engineering Cloud Platform",
		Subtitle: "Internal Agent Hub",
		Logo:     &BrandingLogoConfig{Light: safeSVG},
		Icons:    &BrandingIconsConfig{Favicon: "https://cdn.example.com/favicon.svg", Icon192: pngURI},
	})
	if len(errs) > 0 {
		t.Fatalf("unexpected valid asset errors: %v", errs)
	}
	if resolved.ShortName != "Acme Engineering Cloud P" {
		t.Errorf("ShortName = %q, want 24-rune truncation", resolved.ShortName)
	}
	if !strings.HasPrefix(resolved.LogoLight, "/brand/logo-light?v=") {
		t.Errorf("LogoLight = %q, want /brand/logo-light?v=<sha8>", resolved.LogoLight)
	}
	if resolved.LogoDark != resolved.LogoLight {
		t.Errorf("LogoDark = %q, want fallback to LogoLight %q", resolved.LogoDark, resolved.LogoLight)
	}
	if resolved.AppleTouchIconURL != resolved.Icon192URL {
		t.Errorf("AppleTouchIconURL = %q, want fallback to Icon192URL %q", resolved.AppleTouchIconURL, resolved.Icon192URL)
	}
	if resolved.NotificationIconURL != resolved.Icon192URL {
		t.Errorf("NotificationIconURL = %q, want %q", resolved.NotificationIconURL, resolved.Icon192URL)
	}

	rejectCases := []struct {
		name string
		cfg  BrandingConfig
	}{
		{
			name: "http URL rejected",
			cfg:  BrandingConfig{Logo: &BrandingLogoConfig{Light: "http://cdn.example.com/logo.svg"}},
		},
		{
			name: "protocol-relative path rejected",
			cfg:  BrandingConfig{Logo: &BrandingLogoConfig{Light: "//evil.example.com/logo.svg"}},
		},
		{
			name: "path traversal rejected",
			cfg:  BrandingConfig{Logo: &BrandingLogoConfig{Light: "/assets/../secret.svg"}},
		},
		{
			name: "svg data URI rejected on icon_192",
			cfg:  BrandingConfig{Icons: &BrandingIconsConfig{Icon192: safeSVG}},
		},
		{
			name: "invalid PNG magic bytes rejected",
			cfg:  BrandingConfig{Icons: &BrandingIconsConfig{Icon192: "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("not-a-png-header"))}},
		},
		{
			name: "svg script element rejected",
			cfg:  BrandingConfig{Logo: &BrandingLogoConfig{Light: svgDataURI(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)}},
		},
		{
			name: "svg foreignObject rejected",
			cfg:  BrandingConfig{Logo: &BrandingLogoConfig{Light: svgDataURI(`<svg xmlns="http://www.w3.org/2000/svg"><foreignObject><div>x</div></foreignObject></svg>`)}},
		},
		{
			name: "svg onload handler rejected",
			cfg:  BrandingConfig{Logo: &BrandingLogoConfig{Light: svgDataURI(`<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"/>`)}},
		},
		{
			name: "svg javascript href rejected",
			cfg:  BrandingConfig{Logo: &BrandingLogoConfig{Light: svgDataURI(`<svg xmlns="http://www.w3.org/2000/svg"><a href="javascript:alert(1)"><rect width="10" height="10"/></a></svg>`)}},
		},
		{
			name: "svg DOCTYPE ENTITY rejected",
			cfg:  BrandingConfig{Logo: &BrandingLogoConfig{Light: svgDataURI(`<!DOCTYPE svg [<!ENTITY xxe "boom">]><svg xmlns="http://www.w3.org/2000/svg"/>`)}},
		},
		{
			name: "oversized single asset > 128 KiB rejected",
			cfg: BrandingConfig{
				Logo: &BrandingLogoConfig{
					Light: svgDataURI(`<svg xmlns="http://www.w3.org/2000/svg"><!-- ` + strings.Repeat("a", MaxBrandAssetBytes) + ` --></svg>`),
				},
			},
		},
	}

	for _, tc := range rejectCases {
		t.Run(tc.name, func(t *testing.T) {
			if errs := ValidateBranding(&tc.cfg); len(errs) == 0 {
				t.Fatalf("expected validation error for %s, got nil", tc.name)
			}
		})
	}

	// Oversized total > 512 KiB across 5 valid 110 KiB SVGs.
	chunkSVG := svgDataURI(`<svg xmlns="http://www.w3.org/2000/svg"><!-- ` + strings.Repeat("x", 110<<10) + ` --></svg>`)
	totalErrs := ValidateBranding(&BrandingConfig{
		Logo: &BrandingLogoConfig{
			Light: chunkSVG,
			Dark:  chunkSVG,
		},
		Icons: &BrandingIconsConfig{
			Favicon:        chunkSVG,
			AppleTouchIcon: "https://cdn.example.com/a.png",
		},
	})
	if len(totalErrs) != 0 {
		t.Fatalf("330 KiB across 3 SVGs should pass, got: %v", totalErrs)
	}

	bigPNGPayload := append([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}, bytes.Repeat([]byte{0}, 110<<10)...)
	bigPNG := "data:image/png;base64," + base64.StdEncoding.EncodeToString(bigPNGPayload)
	exceedTotalErrs := ValidateBranding(&BrandingConfig{
		Logo: &BrandingLogoConfig{
			Light: bigPNG,
			Dark:  bigPNG,
		},
		Icons: &BrandingIconsConfig{
			Favicon:        bigPNG,
			AppleTouchIcon: bigPNG,
			Icon192:        bigPNG,
		},
	})
	if len(exceedTotalErrs) == 0 {
		t.Fatal("expected > 512 KiB total asset error across 5 slots, got none")
	}
}
