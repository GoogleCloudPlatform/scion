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
			name: "bright primary-600 fails light Shoelace white-foreground contrast",
			cfg: BrandingConfig{
				Palette: &BrandingPaletteConfig{
					Primary: BrandingScale{
						"600": "#eab308",
					},
				},
			},
			wantPathSub: "server.hub.branding.palette.primary.600",
			wantMsgSub:  "minimum WCAG AA is 4.50:1",
		},
		{
			name: "bright success-600 fails light Shoelace white-foreground contrast",
			cfg: BrandingConfig{
				Palette: &BrandingPaletteConfig{
					Success: BrandingScale{
						"600": "#eab308",
					},
				},
			},
			wantPathSub: "server.hub.branding.palette.success.600",
			wantMsgSub:  "minimum WCAG AA is 4.50:1",
		},
		{
			name: "dark primary-300 fails dark Shoelace neutral-800 foreground contrast",
			cfg: BrandingConfig{
				Palette: &BrandingPaletteConfig{
					Primary: BrandingScale{
						"300": "#1e3a8a",
					},
				},
			},
			wantPathSub: "server.hub.branding.palette.primary.300",
			wantMsgSub:  "dark --sl-color-primary-600 (step 300)",
		},
		{
			name: "dark primary-400 fails dark Shoelace hover neutral-800 foreground contrast",
			cfg: BrandingConfig{
				Palette: &BrandingPaletteConfig{
					Primary: BrandingScale{
						"400": "#1e3a8a",
					},
				},
			},
			wantPathSub: "server.hub.branding.palette.primary.400",
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
			name: "same-origin /auth/logout route rejected",
			cfg:  BrandingConfig{Logo: &BrandingLogoConfig{Light: "/auth/logout"}},
		},
		{
			name: "same-origin /auth prefix with image suffix rejected",
			cfg:  BrandingConfig{Logo: &BrandingLogoConfig{Light: "/auth/logout/logo.svg"}},
		},
		{
			name: "same-origin /api prefix rejected",
			cfg:  BrandingConfig{Logo: &BrandingLogoConfig{Light: "/api/v1/export.png"}},
		},
		{
			name: "same-origin /brand prefix rejected",
			cfg:  BrandingConfig{Logo: &BrandingLogoConfig{Light: "/brand/logo-light.svg"}},
		},
		{
			name: "same-origin non-image path rejected",
			cfg:  BrandingConfig{Logo: &BrandingLogoConfig{Light: "/login"}},
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
			name: "svg xml-stylesheet processing instruction rejected",
			cfg:  BrandingConfig{Logo: &BrandingLogoConfig{Light: svgDataURI(`<?xml-stylesheet type="text/xsl" href="javascript:alert(1)"?><svg xmlns="http://www.w3.org/2000/svg"/>`)}},
		},
		{
			name: "svg style element @import rejected",
			cfg:  BrandingConfig{Logo: &BrandingLogoConfig{Light: svgDataURI(`<svg xmlns="http://www.w3.org/2000/svg"><style>@import url("https://evil.example/x.css");</style></svg>`)}},
		},
		{
			name: "svg style element expression() rejected",
			cfg:  BrandingConfig{Logo: &BrandingLogoConfig{Light: svgDataURI(`<svg xmlns="http://www.w3.org/2000/svg"><style>rect { width: expression(alert(1)); }</style></svg>`)}},
		},
		{
			name: "svg style element javascript URI rejected",
			cfg:  BrandingConfig{Logo: &BrandingLogoConfig{Light: svgDataURI(`<svg xmlns="http://www.w3.org/2000/svg"><style>rect { fill: url(javascript:alert(1)); }</style></svg>`)}},
		},
		{
			name: "svg style element CSS backslash escape rejected",
			cfg:  BrandingConfig{Logo: &BrandingLogoConfig{Light: svgDataURI(`<svg xmlns="http://www.w3.org/2000/svg"><style>@\69mport "https://evil.example/x.css";</style></svg>`)}},
		},
		{
			name: "svg style element comment-split @import rejected",
			cfg:  BrandingConfig{Logo: &BrandingLogoConfig{Light: svgDataURI(`<svg xmlns="http://www.w3.org/2000/svg"><style>@im<!--x-->port url("https://evil.example/x.css");</style></svg>`)}},
		},
		{
			name: "svg style element child-element-split @import rejected",
			cfg:  BrandingConfig{Logo: &BrandingLogoConfig{Light: svgDataURI(`<svg xmlns="http://www.w3.org/2000/svg"><style>@im<g>z</g>port url("https://evil.example/x.css");</style></svg>`)}},
		},
		{
			name: "svg style element child-element-split javascript URI rejected",
			cfg:  BrandingConfig{Logo: &BrandingLogoConfig{Light: svgDataURI(`<svg xmlns="http://www.w3.org/2000/svg"><style>rect{fill:url(java<desc>z</desc>script:alert(1))}</style></svg>`)}},
		},
		{
			name: "svg nested style element rejected",
			cfg:  BrandingConfig{Logo: &BrandingLogoConfig{Light: svgDataURI(`<svg xmlns="http://www.w3.org/2000/svg"><style>@im<style>z</style>port url("https://evil.example/x.css");</style></svg>`)}},
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

	// Valid static same-origin image path (outside reserved Hub prefixes) and standard <?xml?> + benign <style> SVG pass.
	xmlDeclSVG := svgDataURI(`<?xml version="1.0" encoding="UTF-8"?><svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16"><style>.mark { fill: #0d9488; }</style><circle class="mark" cx="8" cy="8" r="6"/></svg>`)
	validPathCfg := BrandingConfig{
		Logo:  &BrandingLogoConfig{Light: "/assets/custom-logo.svg", Dark: xmlDeclSVG},
		Icons: &BrandingIconsConfig{Favicon: "/favicon.ico"},
	}
	if errs := ValidateBranding(&validPathCfg); len(errs) > 0 {
		t.Fatalf("expected valid same-origin image paths to pass, got: %v", errs)
	}
}

func TestResolveBranding_NeutralScaleOverridesMatchSemanticBgAndSurface(t *testing.T) {
	// Light neutral.50 override must re-derive light.primary to primary-600 and pass AA,
	// matching the equivalent palette.light.bg override.
	viaLightBg, errs := ResolveBranding(&BrandingConfig{
		Palette: &BrandingPaletteConfig{
			Light: &BrandingSemanticMode{Bg: "#fafaf9"},
		},
	})
	if len(errs) > 0 {
		t.Fatalf("palette.light.bg=#fafaf9 unexpected errors: %v", errs)
	}

	viaNeutral50, errs := ResolveBranding(&BrandingConfig{
		Palette: &BrandingPaletteConfig{
			Neutral: BrandingScale{"50": "#fafaf9"},
		},
	})
	if len(errs) > 0 {
		t.Fatalf("palette.neutral.50=#fafaf9 unexpected errors: %v", errs)
	}
	if viaNeutral50.Light.Bg != "#fafaf9" {
		t.Errorf("viaNeutral50.Light.Bg = %q, want #fafaf9", viaNeutral50.Light.Bg)
	}
	if viaNeutral50.Light.Primary != viaLightBg.Light.Primary {
		t.Errorf("viaNeutral50.Light.Primary = %q, want %q (same as palette.light.bg)", viaNeutral50.Light.Primary, viaLightBg.Light.Primary)
	}
	if viaNeutral50.Light.Primary != DefaultPrimaryScale()["600"] {
		t.Errorf("viaNeutral50.Light.Primary = %q, want primary-600 %q", viaNeutral50.Light.Primary, DefaultPrimaryScale()["600"])
	}
	// Dark mode was not touched by neutral.50, so dark primary stays stock.
	if viaNeutral50.Dark.Primary != DefaultDarkSemanticMode().Primary {
		t.Errorf("viaNeutral50.Dark.Primary = %q, want untouched stock %q", viaNeutral50.Dark.Primary, DefaultDarkSemanticMode().Primary)
	}

	// Dark neutral 900/800 overrides must similarly re-derive dark primary tokens consistently
	// with palette.dark.bg / palette.dark.surface without perturbing light mode.
	viaDarkBgSurface, errs := ResolveBranding(&BrandingConfig{
		Palette: &BrandingPaletteConfig{
			Dark: &BrandingSemanticMode{Bg: "#0c0a09", Surface: "#1c1917"},
		},
	})
	if len(errs) > 0 {
		t.Fatalf("palette.dark.{bg,surface} unexpected errors: %v", errs)
	}

	viaDarkNeutral, errs := ResolveBranding(&BrandingConfig{
		Palette: &BrandingPaletteConfig{
			Neutral: BrandingScale{"900": "#0c0a09", "800": "#1c1917"},
		},
	})
	if len(errs) > 0 {
		t.Fatalf("palette.neutral.{900,800} unexpected errors: %v", errs)
	}
	if viaDarkNeutral.Dark.Primary != viaDarkBgSurface.Dark.Primary ||
		viaDarkNeutral.Dark.PrimaryHover != viaDarkBgSurface.Dark.PrimaryHover ||
		viaDarkNeutral.Dark.PrimaryActive != viaDarkBgSurface.Dark.PrimaryActive ||
		viaDarkNeutral.Dark.PrimaryText != viaDarkBgSurface.Dark.PrimaryText {
		t.Errorf("dark neutral override primary tokens %+v != explicit dark bg/surface tokens %+v", viaDarkNeutral.Dark, viaDarkBgSurface.Dark)
	}
	if viaDarkNeutral.Light.Primary != DefaultLightSemanticMode().Primary {
		t.Errorf("viaDarkNeutral.Light.Primary = %q, want untouched stock %q", viaDarkNeutral.Light.Primary, DefaultLightSemanticMode().Primary)
	}
}

func TestValidateBranding_SameOriginPathEncodingAndHTTPSQuotes(t *testing.T) {
	rejectURIs := []struct {
		name string
		uri  string
	}{
		// Percent-encoded dot-segment traversal variants
		{"encoded .. lowercase", "/x/%2e%2e/api/v1/foo.png"},
		{"encoded .. mixed-case %2E.", "/x/%2E./api/v1/foo.png"},
		{"encoded .. mixed-case .%2e", "/x/.%2e/api/v1/foo.png"},
		{"encoded .. uppercase %2E%2E", "/x/%2E%2E/auth/logout.svg"},
		// Double-encoded traversal and prefix variants
		{"double-encoded ..", "/x/%252e%252e/api/v1/foo.png"},
		{"double-encoded reserved prefix", "/%2561pi/v1/foo.png"},
		// Percent-encoded reserved prefixes, slashes, backslashes, and spaces
		{"encoded /api prefix", "/%61pi/v1/foo.png"},
		{"encoded /AUTH prefix", "/%41UTH/logout.png"},
		{"encoded slash traversal", "/assets%2f..%2fapi/v1/foo.png"},
		{"encoded backslash", "/assets%5clogo.png"},
		{"encoded space in same-origin path", "/assets/my%20logo.png"},
		// Mixed-case literal reserved prefixes with valid image extensions
		{"mixed-case /API prefix", "/API/v1/foo.png"},
		{"mixed-case /AuTh prefix", "/AuTh/logout.svg"},
		{"mixed-case /BrAnD prefix", "/BrAnD/logo-light.svg"},
		{"mixed-case /EvEnTs prefix", "/EvEnTs/stream.png"},
		{"mixed-case /.WeLl-KnOwN prefix", "/.WeLl-KnOwN/logo.svg"},
		{"mixed-case /HeAlThZ prefix", "/HeAlThZ/check.png"},
		{"mixed-case /ReAdYz prefix", "/ReAdYz/check.ico"},
		// Literal dot segments, empty segments, backslashes, quotes/metacharacters, and C1/bidi controls
		{"leading ./ segment", "/./logo.svg"},
		{"leading ../ segment", "/../logo.svg"},
		{"interior ./ segment", "/assets/./logo.svg"},
		{"interior ../ segment", "/assets/../logo.svg"},
		{"empty interior // segment", "/assets//logo.svg"},
		{"backslash in same-origin path", `/assets\logo.svg`},
		{"double quote in same-origin path", `/assets/a"b.png`},
		{"single quote in same-origin path", `/assets/a'b.png`},
		{"parentheses in same-origin path", `/assets/a(b).png`},
		{"angle brackets in same-origin path", `/assets/a<b>.png`},
		{"C1 control in same-origin path", "/assets/logo\u0085.svg"},
		{"bidi override in same-origin path", "/assets/logo\u202e.svg"},
		// Quotes, parens, angle brackets, backticks, and backslashes in https:// URLs
		{"double quote and paren in https URL", `https://e.com/a")x.png`},
		{"single quote in https URL", `https://e.com/a'x.png`},
		{"parentheses in https URL", `https://e.com/a(1).png`},
		{"angle brackets in https URL", `https://e.com/a<x>.png`},
		{"backtick in https URL", "https://e.com/a`x.png"},
		{"backslash in https URL", `https://e.com/a\x.png`},
		{"bidi override in https URL", "https://e.com/a\u202ex.png"},
	}

	for _, tc := range rejectURIs {
		t.Run("reject_"+tc.name, func(t *testing.T) {
			cfg := BrandingConfig{Logo: &BrandingLogoConfig{Light: tc.uri}}
			if errs := ValidateBranding(&cfg); len(errs) == 0 {
				t.Fatalf("expected validation error for URI %q, got none", tc.uri)
			}
		})
	}

	// Bidi override and C1 control in display names are also rejected.
	for _, badName := range []string{"Acme\u202eevil", "Acme\u0085Cloud"} {
		if errs := ValidateBranding(&BrandingConfig{Name: badName}); len(errs) == 0 {
			t.Fatalf("expected validation error for Name %q, got none", badName)
		}
	}

	// Valid static same-origin paths and https URLs continue to pass.
	validURIs := []string{
		"/assets/custom-logo.svg",
		"/static/v2/acme_brand-dark@2x.png",
		"/favicon.ico",
		"https://cdn.example.com/brand/logo-v2.svg?rev=1",
	}
	for _, u := range validURIs {
		cfg := BrandingConfig{Logo: &BrandingLogoConfig{Light: u}}
		if errs := ValidateBranding(&cfg); len(errs) > 0 {
			t.Fatalf("expected valid URI %q to pass, got: %v", u, errs)
		}
	}
}

func TestValidateBranding_DarkBadgeCompositeContrast(t *testing.T) {
	// Branch 1: Custom ramp step 300 fails both dark Shoelace button contrast (against neutral-800)
	// and dark badge composite contrast (20% step 500 over dark.surface) while untouched steps
	// (600, 400, 800, 100) do not produce spurious errors.
	errs := ValidateBranding(&BrandingConfig{
		Palette: &BrandingPaletteConfig{
			Success: BrandingScale{"300": "#334155"},
		},
	})
	if len(errs) != 2 {
		t.Fatalf("expected 2 errors on success.300 (dark Shoelace button + dark badge contrast), got %d: %v", len(errs), errs)
	}
	var sawButton, sawBadge bool
	for _, e := range errs {
		if e.Path != "server.hub.branding.palette.success.300" {
			t.Errorf("unexpected error path: %+v", e)
		}
		if strings.Contains(e.Message, "dark --sl-color-success-600 (step 300)") {
			sawButton = true
		}
		if strings.Contains(e.Message, "dark badge contrast") {
			sawBadge = true
		}
	}
	if !sawButton || !sawBadge {
		t.Errorf("expected both dark Shoelace button and dark badge errors, got: %v", errs)
	}

	// Branch 2: Custom lightish dark.surface triggers dark-badge composite checks even when ramps are stock.
	surfaceErrs := ValidateBranding(&BrandingConfig{
		Palette: &BrandingPaletteConfig{
			Dark: &BrandingSemanticMode{
				Bg:            "#0f172a",
				Surface:       "#cbd5e1",
				Text:          "#0f172a",
				TextSecondary: "#0f172a",
				TextMuted:     "#1e293b",
				Primary:       "#1d4ed8",
				PrimaryHover:  "#1e40af",
				PrimaryActive: "#1e3a8a",
				PrimaryText:   "#ffffff",
			},
			Neutral: BrandingScale{"400": "#334155"},
		},
	})
	foundDarkBadge := false
	for _, e := range surfaceErrs {
		if strings.HasSuffix(e.Path, ".300") && strings.Contains(e.Message, "dark badge contrast") {
			foundDarkBadge = true
			break
		}
	}
	if !foundDarkBadge {
		t.Fatalf("expected darkSurfaceCustomized branch to flag dark badge contrast on .300, got: %v", surfaceErrs)
	}
}

func hexHueDegrees(t *testing.T, hexStr string) float64 {
	t.Helper()
	r, g, b, err := parseHexRGB(hexStr)
	if err != nil {
		t.Fatalf("parseHexRGB(%q): %v", hexStr, err)
	}
	_, _, h := srgbToOKLCH(r, g, b)
	return h * 180.0 / math.Pi
}

func hueAngleDiffDeg(a, b float64) float64 {
	d := math.Mod(math.Abs(a-b), 360.0)
	if d > 180.0 {
		d = 360.0 - d
	}
	return d
}

func assertSameHue(t *testing.T, label, gotHex, wantHueRefHex string, maxDeg float64) {
	t.Helper()
	gotDeg := hexHueDegrees(t, gotHex)
	wantDeg := hexHueDegrees(t, wantHueRefHex)
	if diff := hueAngleDiffDeg(gotDeg, wantDeg); diff > maxDeg {
		t.Errorf("%s (%s, hue %.1f°) drifted %.1f° from %s (hue %.1f°); max %.1f°",
			label, gotHex, gotDeg, diff, wantHueRefHex, wantDeg, maxDeg)
	}
}

func TestResolveBranding_ExplicitModePrimaryPreservesHueAndSingleTokenSymmetry(t *testing.T) {
	// 1. Both palette.light.primary and palette.dark.primary set (teal) without accent_color or palette.primary:
	// hover, active, scales["primary"], and PublicBranding/CSSText must stay teal (no stock-blue leak).
	bothModes, errs := ResolveBranding(&BrandingConfig{
		Palette: &BrandingPaletteConfig{
			Light: &BrandingSemanticMode{Primary: "#0f766e"},
			Dark:  &BrandingSemanticMode{Primary: "#2dd4bf"},
		},
	})
	if len(errs) > 0 {
		t.Fatalf("explicit light+dark teal primary unexpected errors: %v", errs)
	}
	assertSameHue(t, "light.PrimaryHover", bothModes.Light.PrimaryHover, "#0f766e", 10.0)
	assertSameHue(t, "light.PrimaryActive", bothModes.Light.PrimaryActive, "#0f766e", 10.0)
	assertSameHue(t, "dark.PrimaryHover", bothModes.Dark.PrimaryHover, "#2dd4bf", 10.0)
	assertSameHue(t, "dark.PrimaryActive", bothModes.Dark.PrimaryActive, "#2dd4bf", 10.0)
	assertSameHue(t, "scales[primary][600]", bothModes.Scales["primary"]["600"], "#0f766e", 10.0)
	pub := bothModes.Public()
	if pub == nil || !strings.Contains(pub.CSSText, "--scion-primary-hover: "+bothModes.Light.PrimaryHover) {
		t.Fatalf("expected PublicBranding.CSSText to publish teal light.PrimaryHover %s", bothModes.Light.PrimaryHover)
	}

	// 2. Single-mode palette.light.primary alone (#0f766e): no stock-blue leak in light, dark, or primary scale.
	lightOnly, errs := ResolveBranding(&BrandingConfig{
		Palette: &BrandingPaletteConfig{
			Light: &BrandingSemanticMode{Primary: "#0f766e"},
		},
	})
	if len(errs) > 0 {
		t.Fatalf("light-only primary unexpected errors: %v", errs)
	}
	assertSameHue(t, "lightOnly light.PrimaryHover", lightOnly.Light.PrimaryHover, "#0f766e", 10.0)
	assertSameHue(t, "lightOnly light.PrimaryActive", lightOnly.Light.PrimaryActive, "#0f766e", 10.0)
	assertSameHue(t, "lightOnly dark.Primary", lightOnly.Dark.Primary, "#0f766e", 10.0)
	assertSameHue(t, "lightOnly dark.PrimaryHover", lightOnly.Dark.PrimaryHover, "#0f766e", 10.0)
	assertSameHue(t, "lightOnly dark.PrimaryActive", lightOnly.Dark.PrimaryActive, "#0f766e", 10.0)

	// 3. Single-mode palette.dark.primary alone (#2dd4bf): no stock-blue leak in dark, light, or primary scale.
	darkOnly, errs := ResolveBranding(&BrandingConfig{
		Palette: &BrandingPaletteConfig{
			Dark: &BrandingSemanticMode{Primary: "#2dd4bf"},
		},
	})
	if len(errs) > 0 {
		t.Fatalf("dark-only primary unexpected errors: %v", errs)
	}
	assertSameHue(t, "darkOnly dark.PrimaryHover", darkOnly.Dark.PrimaryHover, "#2dd4bf", 10.0)
	assertSameHue(t, "darkOnly dark.PrimaryActive", darkOnly.Dark.PrimaryActive, "#2dd4bf", 10.0)
	assertSameHue(t, "darkOnly light.Primary", darkOnly.Light.Primary, "#2dd4bf", 10.0)
	assertSameHue(t, "darkOnly light.PrimaryHover", darkOnly.Light.PrimaryHover, "#2dd4bf", 10.0)
	assertSameHue(t, "darkOnly light.PrimaryActive", darkOnly.Light.PrimaryActive, "#2dd4bf", 10.0)

	// 4. Split-hue explicit light (#0f766e teal) + dark (#c084fc purple): each mode keeps its own hue.
	splitHue, errs := ResolveBranding(&BrandingConfig{
		Palette: &BrandingPaletteConfig{
			Light: &BrandingSemanticMode{Primary: "#0f766e"},
			Dark:  &BrandingSemanticMode{Primary: "#c084fc"},
		},
	})
	if len(errs) > 0 {
		t.Fatalf("split-hue primary unexpected errors: %v", errs)
	}
	assertSameHue(t, "splitHue light.PrimaryHover", splitHue.Light.PrimaryHover, "#0f766e", 10.0)
	assertSameHue(t, "splitHue dark.PrimaryHover", splitHue.Dark.PrimaryHover, "#c084fc", 10.0)

	// 5. accent_color alone (#d97706 amber): both modes and scale stay amber.
	accentOnly, errs := ResolveBranding(&BrandingConfig{AccentColor: "#d97706"})
	if len(errs) > 0 {
		t.Fatalf("accent_color alone unexpected errors: %v", errs)
	}
	assertSameHue(t, "accentOnly light.Primary", accentOnly.Light.Primary, "#d97706", 12.0)
	assertSameHue(t, "accentOnly light.PrimaryHover", accentOnly.Light.PrimaryHover, "#d97706", 12.0)
	assertSameHue(t, "accentOnly dark.Primary", accentOnly.Dark.Primary, "#d97706", 12.0)
	assertSameHue(t, "accentOnly dark.PrimaryHover", accentOnly.Dark.PrimaryHover, "#d97706", 12.0)

	// 6. Pin O3: setting palette.light.primary_text alone ("#ffffff") shifts light.Primary from
	// grandfathered stock #3b82f6 (3.68:1 on white) to AA-passing primary-600 (#2563eb, 5.17:1).
	primaryTextOnly, errs := ResolveBranding(&BrandingConfig{
		Palette: &BrandingPaletteConfig{
			Light: &BrandingSemanticMode{PrimaryText: "#ffffff"},
		},
	})
	if len(errs) > 0 {
		t.Fatalf("primary_text alone unexpected errors: %v", errs)
	}
	if primaryTextOnly.Light.Primary != DefaultPrimaryScale()["600"] {
		t.Errorf("primaryTextOnly.Light.Primary = %q, want AA-compliant primary-600 %q",
			primaryTextOnly.Light.Primary, DefaultPrimaryScale()["600"])
	}
}

func TestValidateBranding_DeterministicScaleErrorOrder(t *testing.T) {
	cfg := BrandingConfig{
		Palette: &BrandingPaletteConfig{
			Primary: BrandingScale{
				"999": "#000000",
				"500": "not-a-hex",
				"50":  "bad",
				"123": "#000000",
				"900": "nope",
			},
		},
	}
	first := ValidateBranding(&cfg)
	wantPaths := []string{
		"server.hub.branding.palette.primary.50",
		"server.hub.branding.palette.primary.500",
		"server.hub.branding.palette.primary.900",
		"server.hub.branding.palette.primary.123",
		"server.hub.branding.palette.primary.999",
	}
	gotPaths := make([]string, len(first))
	for i, e := range first {
		gotPaths[i] = e.Path
	}
	if !reflect.DeepEqual(gotPaths, wantPaths) {
		t.Fatalf("got error paths %v, want %v", gotPaths, wantPaths)
	}
	for run := 0; run < 50; run++ {
		again := ValidateBranding(&cfg)
		if !reflect.DeepEqual(again, first) {
			t.Fatalf("run %d returned different error order: %v vs %v", run, again, first)
		}
	}
}

func TestIsValidBrandSlot(t *testing.T) {
	valid := []string{
		BrandSlotLogoLight,
		BrandSlotLogoDark,
		BrandSlotFavicon,
		BrandSlotAppleTouchIcon,
		BrandSlotIcon192,
		BrandSlotIcon512,
		BrandSlotIconMaskable512,
	}
	for _, slot := range valid {
		if !IsValidBrandSlot(slot) {
			t.Errorf("IsValidBrandSlot(%q) = false, want true", slot)
		}
	}
	invalid := []string{"", "logo", "logo-light/extra", "../favicon", "LOGO-LIGHT", "favicon.svg", "unknown"}
	for _, slot := range invalid {
		if IsValidBrandSlot(slot) {
			t.Errorf("IsValidBrandSlot(%q) = true, want false", slot)
		}
	}
}

func TestValidateBranding_SVGAllowlistAndExternalRefs(t *testing.T) {
	// Valid static brand SVG using gradients, clipPath, filter, <use href="#id"/>, and <style> with url(#grad).
	richValidSVG := svgDataURI(`<?xml version="1.0" encoding="UTF-8"?>
<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 120 32">
  <defs>
    <linearGradient id="g1" x1="0" y1="0" x2="1" y2="1">
      <stop offset="0%" stop-color="#0d9488"/>
      <stop offset="100%" stop-color="#0f766e"/>
    </linearGradient>
    <clipPath id="c1"><rect width="32" height="32" rx="6"/></clipPath>
    <filter id="f1"><feGaussianBlur stdDeviation="1"/></filter>
    <symbol id="mark" viewBox="0 0 32 32"><path d="M4 16L16 4L28 16L16 28Z"/></symbol>
  </defs>
  <style>.badge { fill: url(#g1); }</style>
  <g clip-path="url(#c1)" filter="url(#f1)">
    <use href="#mark" class="badge"/>
  </g>
  <text x="40" y="22">Acme</text>
</svg>`)
	if errs := ValidateBranding(&BrandingConfig{Logo: &BrandingLogoConfig{Light: richValidSVG}}); len(errs) > 0 {
		t.Fatalf("expected valid rich static SVG to pass, got: %v", errs)
	}

	rejectSVGs := []struct {
		name string
		svg  string
	}{
		{"SMIL animate element", `<svg xmlns="http://www.w3.org/2000/svg"><animate attributeName="x" from="0" to="10"/></svg>`},
		{"SMIL set element", `<svg xmlns="http://www.w3.org/2000/svg"><set attributeName="onbegin" to="alert(1)"/></svg>`},
		{"SMIL animateTransform element", `<svg xmlns="http://www.w3.org/2000/svg"><animateTransform attributeName="transform" type="rotate"/></svg>`},
		{"handler element", `<svg xmlns="http://www.w3.org/2000/svg"><handler>alert(1)</handler></svg>`},
		{"image element", `<svg xmlns="http://www.w3.org/2000/svg"><image href="https://evil.example/track.png"/></svg>`},
		{"feImage filter element", `<svg xmlns="http://www.w3.org/2000/svg"><filter id="f"><feImage href="https://evil.example/x.png"/></filter></svg>`},
		{"hyperlink a element", `<svg xmlns="http://www.w3.org/2000/svg"><a href="https://evil.example"><rect width="10" height="10"/></a></svg>`},
		{"XHTML namespace element", `<svg xmlns="http://www.w3.org/2000/svg"><div xmlns="http://www.w3.org/1999/xhtml">hi</div></svg>`},
		{"use with external https href", `<svg xmlns="http://www.w3.org/2000/svg"><use href="https://evil.example/x.svg#a"/></svg>`},
		{"use with xlink:href data URI", `<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink"><use xlink:href="data:image/svg+xml;base64,PHN2Zy8+"/></svg>`},
		{"style element external url()", `<svg xmlns="http://www.w3.org/2000/svg"><style>rect { fill: url(https://evil.example/track.svg#a); }</style></svg>`},
		{"style attribute external url()", `<svg xmlns="http://www.w3.org/2000/svg"><rect style="background:url(https://evil.example/track.png)"/></svg>`},
		{"style element data URI", `<svg xmlns="http://www.w3.org/2000/svg"><style>rect { fill: url(data:image/png;base64,AAAA); }</style></svg>`},
		{"style element image-set external URL", `<svg xmlns="http://www.w3.org/2000/svg"><style>rect { background-image: image-set("https://evil.example/track.png" 1x); }</style></svg>`},
		{"style attribute image-set external URL", `<svg xmlns="http://www.w3.org/2000/svg"><rect style='background: -webkit-image-set("https://evil.example/track.png" 1x)'/></svg>`},
		{"fill presentation attribute external url()", `<svg xmlns="http://www.w3.org/2000/svg"><rect fill="url(https://evil.example/paint.svg#g)"/></svg>`},
		{"stroke presentation attribute external url()", `<svg xmlns="http://www.w3.org/2000/svg"><rect stroke="url(https://evil.example/paint.svg#s)"/></svg>`},
		{"filter presentation attribute external url()", `<svg xmlns="http://www.w3.org/2000/svg"><rect filter="url(https://evil.example/f.svg#f)"/></svg>`},
		{"clip-path presentation attribute external url()", `<svg xmlns="http://www.w3.org/2000/svg"><rect clip-path="url(https://evil.example/c.svg#c)"/></svg>`},
		{"mask presentation attribute external url()", `<svg xmlns="http://www.w3.org/2000/svg"><rect mask="url(https://evil.example/m.svg#m)"/></svg>`},
		{"marker-end presentation attribute external url()", `<svg xmlns="http://www.w3.org/2000/svg"><path d="M0 0 L10 10" marker-end="url(https://evil.example/m.svg#a)"/></svg>`},
		{"fill presentation attribute CSS-escaped url()", `<svg xmlns="http://www.w3.org/2000/svg"><rect fill="u\72l(https://evil.example/p.svg#g)"/></svg>`},
		{"filter presentation attribute CSS-escaped url()", `<svg xmlns="http://www.w3.org/2000/svg"><rect filter="u\72l(https://evil.example/f.svg#f)"/></svg>`},
	}
	for _, tc := range rejectSVGs {
		t.Run("reject_"+tc.name, func(t *testing.T) {
			cfg := BrandingConfig{Logo: &BrandingLogoConfig{Light: svgDataURI(tc.svg)}}
			if errs := ValidateBranding(&cfg); len(errs) == 0 {
				t.Fatalf("expected SVG validation error for %s, got nil", tc.name)
			}
		})
	}
}

func TestResolveBranding_CornersPresetsAndComposition(t *testing.T) {
	t.Run("default corners stays uncustomized", func(t *testing.T) {
		def := DefaultResolvedBranding()
		if def.Corners != BrandCornersDefault || def.HasCustomStyle {
			t.Fatalf("DefaultResolvedBranding corners=%q hasCustomStyle=%v", def.Corners, def.HasCustomStyle)
		}
		resolved, errs := ResolveBranding(&BrandingConfig{Corners: BrandCornersDefault})
		if len(errs) != 0 {
			t.Fatalf("unexpected errs: %v", errs)
		}
		if resolved.IsCustomized() || resolved.HasCustomStyle || resolved.HasCustomPalette {
			t.Fatalf("explicit default corners should remain uncustomized: %+v", resolved)
		}
		if resolved.Public() != nil || resolved.CSSText != "" {
			t.Fatalf("explicit default corners should have nil Public() and empty CSSText")
		}
	})

	t.Run("square corners without custom palette emits mode-independent radius CSS only", func(t *testing.T) {
		cfg := BrandingConfig{Corners: BrandCornersSquare}
		if cfg.IsZero() {
			t.Fatal("BrandingConfig with Corners set must not report IsZero()")
		}
		if cloned := cfg.Clone(); cloned == nil || cloned.Corners != BrandCornersSquare {
			t.Fatalf("Clone() did not preserve Corners: %+v", cloned)
		}

		resolved, errs := ResolveBranding(&cfg)
		if len(errs) != 0 {
			t.Fatalf("unexpected errs: %v", errs)
		}
		if !resolved.IsCustomized() || !resolved.HasCustomStyle || resolved.HasCustomPalette {
			t.Fatalf("unexpected flags: IsCustomized=%v HasCustomStyle=%v HasCustomPalette=%v",
				resolved.IsCustomized(), resolved.HasCustomStyle, resolved.HasCustomPalette)
		}
		if resolved.ThemeColor != DefaultBrandThemeColor {
			t.Errorf("ThemeColor = %q, want default %q", resolved.ThemeColor, DefaultBrandThemeColor)
		}
		for _, want := range []string{
			"html:root, :host, .sl-theme-light {",
			"--scion-radius-sm: 0;",
			"--scion-radius: 0;",
			"--scion-radius-md: 0;",
			"--scion-radius-lg: 0;",
			"--scion-radius-xl: 0;",
		} {
			if !strings.Contains(resolved.CSSText, want) {
				t.Errorf("CSSText missing %q:\n%s", want, resolved.CSSText)
			}
		}
		for _, forbidden := range []string{"--scion-primary:", "sl-theme-dark", "prefers-color-scheme: dark"} {
			if strings.Contains(resolved.CSSText, forbidden) {
				t.Errorf("corners-only CSSText must not contain %q:\n%s", forbidden, resolved.CSSText)
			}
		}
		pub := resolved.Public()
		if pub == nil {
			t.Fatal("expected non-nil PublicBranding")
		}
		if pub.Corners != BrandCornersSquare {
			t.Errorf("Public().Corners = %q, want %q", pub.Corners, BrandCornersSquare)
		}
		if pub.CSSText != resolved.CSSText {
			t.Errorf("Public().CSSText mismatch")
		}
		if pub.Light != nil || pub.Dark != nil || pub.Scales != nil {
			t.Errorf("corners-only PublicBranding must omit palette fields: %+v", pub)
		}
	})

	t.Run("rounded corners emits rounded radius scale", func(t *testing.T) {
		resolved, errs := ResolveBranding(&BrandingConfig{Corners: BrandCornersRounded})
		if len(errs) != 0 {
			t.Fatalf("unexpected errs: %v", errs)
		}
		for _, want := range []string{
			"--scion-radius-sm: 0.375rem;",
			"--scion-radius: 0.75rem;",
			"--scion-radius-md: 0.75rem;",
			"--scion-radius-lg: 1rem;",
			"--scion-radius-xl: 1.25rem;",
		} {
			if !strings.Contains(resolved.CSSText, want) {
				t.Errorf("CSSText missing %q:\n%s", want, resolved.CSSText)
			}
		}
		if pub := resolved.Public(); pub == nil || pub.Corners != BrandCornersRounded {
			t.Fatalf("Public().Corners = %+v, want %q", pub, BrandCornersRounded)
		}
	})

	t.Run("corners composes with custom palette across light and dark blocks", func(t *testing.T) {
		resolved, errs := ResolveBranding(&BrandingConfig{
			AccentColor: "#0d9488",
			Corners:     BrandCornersSquare,
		})
		if len(errs) != 0 {
			t.Fatalf("unexpected errs: %v", errs)
		}
		if !resolved.HasCustomPalette || !resolved.HasCustomStyle {
			t.Fatal("expected both HasCustomPalette and HasCustomStyle to be true")
		}
		for _, want := range []string{
			"--scion-primary:",
			"--scion-radius: 0;",
			"html:root.sl-theme-dark",
			"prefers-color-scheme: dark",
		} {
			if !strings.Contains(resolved.CSSText, want) {
				t.Errorf("combined CSSText missing %q:\n%s", want, resolved.CSSText)
			}
		}
		pub := resolved.Public()
		if pub == nil || pub.Corners != BrandCornersSquare || pub.Light == nil || pub.Dark == nil || len(pub.Scales) != 5 {
			t.Fatalf("combined PublicBranding incomplete: %+v", pub)
		}
	})

	t.Run("invalid corners rejected and triggers malformed fallback", func(t *testing.T) {
		for _, bad := range []string{"pill", "SQUARE", "0; } body { color: red; }"} {
			fallback, errs := ResolveBranding(&BrandingConfig{Corners: bad})
			if len(errs) == 0 {
				t.Fatalf("expected validation error for corners=%q", bad)
			}
			if errs[0].Path != BrandingSectionKey+".corners" {
				t.Errorf("err.Path = %q, want %q", errs[0].Path, BrandingSectionKey+".corners")
			}
			for _, allowed := range []string{BrandCornersDefault, BrandCornersSquare, BrandCornersRounded} {
				if !strings.Contains(errs[0].Message, allowed) {
					t.Errorf("err.Message %q should name allowed enum value %q", errs[0].Message, allowed)
				}
			}
			if !fallback.Malformed || fallback.IsCustomized() || fallback.Corners != BrandCornersDefault {
				t.Fatalf("malformed fallback unexpected: %+v", fallback)
			}
		}
	})
}
