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
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	wcagAATextContrastMin = 4.5
	wcagAAUIContrastMin   = 3.0
)

var hexColorRegex = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

type shadeAnchor struct {
	step         string
	targetY      float64
	chromaFactor float64
}

// oklchShadeAnchors defines the calibrated WCAG relative luminance (Y) targets
// and OKLCH chroma factors for generating a 50..950 ramp from an accent hex.
var oklchShadeAnchors = []shadeAnchor{
	{step: "50", targetY: 0.93, chromaFactor: 0.12},
	{step: "100", targetY: 0.84, chromaFactor: 0.24},
	{step: "200", targetY: 0.69, chromaFactor: 0.48},
	{step: "300", targetY: 0.50, chromaFactor: 0.75},
	{step: "400", targetY: 0.34, chromaFactor: 0.92},
	{step: "500", targetY: 0.21, chromaFactor: 1.00},
	{step: "600", targetY: 0.13, chromaFactor: 0.95},
	{step: "700", targetY: 0.085, chromaFactor: 0.85},
	{step: "800", targetY: 0.055, chromaFactor: 0.72},
	{step: "900", targetY: 0.035, chromaFactor: 0.58},
	{step: "950", targetY: 0.018, chromaFactor: 0.45},
}

// DefaultPrimaryScale returns stock Scion's Tailwind Blue 50..950 scale.
func DefaultPrimaryScale() BrandingScale {
	return BrandingScale{
		"50":  "#eff6ff",
		"100": "#dbeafe",
		"200": "#bfdbfe",
		"300": "#93c5fd",
		"400": "#60a5fa",
		"500": "#3b82f6",
		"600": "#2563eb",
		"700": "#1d4ed8",
		"800": "#1e40af",
		"900": "#1e3a8a",
		"950": "#172554",
	}
}

// DefaultNeutralScale returns stock Scion's Tailwind Slate 50..950 scale.
// Step 950 matches theme.css's --sl-color-neutral-950 mapping (#0f172a).
func DefaultNeutralScale() BrandingScale {
	return BrandingScale{
		"50":  "#f8fafc",
		"100": "#f1f5f9",
		"200": "#e2e8f0",
		"300": "#cbd5e1",
		"400": "#94a3b8",
		"500": "#64748b",
		"600": "#475569",
		"700": "#334155",
		"800": "#1e293b",
		"900": "#0f172a",
		"950": "#0f172a",
	}
}

// DefaultSuccessScale returns stock Scion's Tailwind Green 50..950 scale.
func DefaultSuccessScale() BrandingScale {
	return BrandingScale{
		"50":  "#f0fdf4",
		"100": "#dcfce7",
		"200": "#bbf7d0",
		"300": "#86efac",
		"400": "#4ade80",
		"500": "#22c55e",
		"600": "#16a34a",
		"700": "#15803d",
		"800": "#166534",
		"900": "#14532d",
		"950": "#052e16",
	}
}

// DefaultWarningScale returns stock Scion's Tailwind Amber 50..950 scale.
func DefaultWarningScale() BrandingScale {
	return BrandingScale{
		"50":  "#fffbeb",
		"100": "#fef3c7",
		"200": "#fde68a",
		"300": "#fcd34d",
		"400": "#fbbf24",
		"500": "#f59e0b",
		"600": "#d97706",
		"700": "#b45309",
		"800": "#92400e",
		"900": "#78350f",
		"950": "#451a03",
	}
}

// DefaultDangerScale returns stock Scion's Tailwind Red 50..950 scale.
func DefaultDangerScale() BrandingScale {
	return BrandingScale{
		"50":  "#fef2f2",
		"100": "#fee2e2",
		"200": "#fecaca",
		"300": "#fca5a5",
		"400": "#f87171",
		"500": "#ef4444",
		"600": "#dc2626",
		"700": "#b91c1c",
		"800": "#991b1b",
		"900": "#7f1d1d",
		"950": "#450a0a",
	}
}

// DefaultLightSemanticMode returns stock Scion's light semantic tokens.
func DefaultLightSemanticMode() BrandingSemanticMode {
	return BrandingSemanticMode{
		Bg:            "#f8fafc",
		BgSubtle:      "#f1f5f9",
		Surface:       "#ffffff",
		SurfaceRaised: "#ffffff",
		Text:          "#1e293b",
		TextSecondary: "#475569",
		TextMuted:     "#64748b",
		Border:        "#e2e8f0",
		Primary:       "#3b82f6",
		PrimaryHover:  "#2563eb",
		PrimaryActive: "#1d4ed8",
		PrimaryText:   "#ffffff",
	}
}

// DefaultDarkSemanticMode returns stock Scion's dark semantic tokens.
func DefaultDarkSemanticMode() BrandingSemanticMode {
	return BrandingSemanticMode{
		Bg:            "#0f172a",
		BgSubtle:      "#1e293b",
		Surface:       "#1e293b",
		SurfaceRaised: "#334155",
		Text:          "#f1f5f9",
		TextSecondary: "#cbd5e1",
		TextMuted:     "#94a3b8",
		Border:        "#334155",
		Primary:       "#60a5fa",
		PrimaryHover:  "#93c5fd",
		PrimaryActive: "#3b82f6",
		PrimaryText:   "#0f172a",
	}
}

// IsValidHexColor reports whether s is a 6-digit #RRGGBB hex string.
func IsValidHexColor(s string) bool {
	return hexColorRegex.MatchString(s)
}

func normalizeHex(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

func parseHexRGB(hexStr string) (r, g, b uint8, err error) {
	if !IsValidHexColor(hexStr) {
		return 0, 0, 0, fmt.Errorf("must be a 6-digit #RRGGBB hex color")
	}
	v, err := strconv.ParseUint(hexStr[1:], 16, 32)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("invalid hex color %q: %w", hexStr, err)
	}
	return uint8((v >> 16) & 0xff), uint8((v >> 8) & 0xff), uint8(v & 0xff), nil
}

func formatHexRGB(r, g, b uint8) string {
	return fmt.Sprintf("#%02x%02x%02x", r, g, b)
}

func srgbChannelToLinear(c float64) float64 {
	if c <= 0.04045 {
		return c / 12.92
	}
	return math.Pow((c+0.055)/1.055, 2.4)
}

func linearChannelToSRGB(c float64) float64 {
	if c <= 0 {
		return 0
	}
	if c >= 1 {
		return 1
	}
	if c <= 0.0031308 {
		return 12.92 * c
	}
	return 1.055*math.Pow(c, 1.0/2.4) - 0.055
}

func linearLuminance(rLin, gLin, bLin float64) float64 {
	return 0.2126*rLin + 0.7152*gLin + 0.0722*bLin
}

func rgbFloatLuminance(r255, g255, b255 float64) float64 {
	return linearLuminance(
		srgbChannelToLinear(r255/255.0),
		srgbChannelToLinear(g255/255.0),
		srgbChannelToLinear(b255/255.0),
	)
}

// RelativeLuminance returns the IEC 61966-2-1 relative luminance Y in [0, 1]
// for a validated #RRGGBB hex string.
func RelativeLuminance(hexStr string) (float64, error) {
	r, g, b, err := parseHexRGB(hexStr)
	if err != nil {
		return 0, err
	}
	return rgbFloatLuminance(float64(r), float64(g), float64(b)), nil
}

func contrastFromLuminance(y1, y2 float64) float64 {
	hi, lo := y1, y2
	if lo > hi {
		hi, lo = lo, hi
	}
	return (hi + 0.05) / (lo + 0.05)
}

// ContrastRatio returns the WCAG 2.1 contrast ratio in [1, 21] between two
// #RRGGBB hex colors.
func ContrastRatio(hexA, hexB string) (float64, error) {
	y1, err := RelativeLuminance(hexA)
	if err != nil {
		return 0, err
	}
	y2, err := RelativeLuminance(hexB)
	if err != nil {
		return 0, err
	}
	return contrastFromLuminance(y1, y2), nil
}

// compositeAlphaContrast computes the WCAG contrast ratio of fgHex against
// tintHex alpha-composited at alpha in [0, 1] over surfaceHex.
func compositeAlphaContrast(fgHex, tintHex string, alpha float64, surfaceHex string) (float64, error) {
	fgY, err := RelativeLuminance(fgHex)
	if err != nil {
		return 0, err
	}
	tr, tg, tb, err := parseHexRGB(tintHex)
	if err != nil {
		return 0, err
	}
	sr, sg, sb, err := parseHexRGB(surfaceHex)
	if err != nil {
		return 0, err
	}
	cr := float64(tr)*alpha + float64(sr)*(1.0-alpha)
	cg := float64(tg)*alpha + float64(sg)*(1.0-alpha)
	cb := float64(tb)*alpha + float64(sb)*(1.0-alpha)
	bgY := rgbFloatLuminance(cr, cg, cb)
	return contrastFromLuminance(fgY, bgY), nil
}

// sRGB -> Oklab -> OKLCH conversion (Björn Ottosson's reference matrices).
func srgbToOKLCH(r, g, b uint8) (L, C, h float64) {
	lr := srgbChannelToLinear(float64(r) / 255.0)
	lg := srgbChannelToLinear(float64(g) / 255.0)
	lb := srgbChannelToLinear(float64(b) / 255.0)

	l := 0.4122214708*lr + 0.5363325363*lg + 0.0514459929*lb
	m := 0.2119034982*lr + 0.6806995451*lg + 0.1073969566*lb
	s := 0.0883024619*lr + 0.2817188376*lg + 0.6299787005*lb

	lRoot := math.Cbrt(l)
	mRoot := math.Cbrt(m)
	sRoot := math.Cbrt(s)

	L = 0.2104542553*lRoot + 0.7936177850*mRoot - 0.0040720468*sRoot
	okA := 1.9779984951*lRoot - 2.4285922050*mRoot + 0.4505937099*sRoot
	okB := 0.0259040371*lRoot + 0.7827717662*mRoot - 0.8086757660*sRoot

	C = math.Hypot(okA, okB)
	if C > 1e-7 {
		h = math.Atan2(okB, okA)
	}
	return L, C, h
}

func oklchToLinearSRGB(L, C, h float64) (r, g, b float64) {
	okA := C * math.Cos(h)
	okB := C * math.Sin(h)

	lRoot := L + 0.3963377774*okA + 0.2158037573*okB
	mRoot := L - 0.1055613458*okA - 0.0638541728*okB
	sRoot := L - 0.0894841775*okA - 1.2914855480*okB

	l := lRoot * lRoot * lRoot
	m := mRoot * mRoot * mRoot
	s := sRoot * sRoot * sRoot

	r = +4.0767416621*l - 3.3077115913*m + 0.2309699292*s
	g = -1.2684380046*l + 2.6097574011*m - 0.3413193965*s
	b = -0.0041960863*l - 0.7034186147*m + 1.7076147010*s
	return r, g, b
}

func inSRGBGamut(r, g, b float64) bool {
	const eps = 1e-7
	return r >= -eps && r <= 1+eps && g >= -eps && g <= 1+eps && b >= -eps && b <= 1+eps
}

func clampGamutOKLCH(L, wantC, h float64) (r, g, b float64) {
	if wantC <= 0 {
		r, g, b = oklchToLinearSRGB(L, 0, h)
		return math.Min(1, math.Max(0, r)), math.Min(1, math.Max(0, g)), math.Min(1, math.Max(0, b))
	}
	r, g, b = oklchToLinearSRGB(L, wantC, h)
	if inSRGBGamut(r, g, b) {
		return math.Min(1, math.Max(0, r)), math.Min(1, math.Max(0, g)), math.Min(1, math.Max(0, b))
	}
	lo, hi := 0.0, wantC
	for i := 0; i < 32; i++ {
		mid := 0.5 * (lo + hi)
		mr, mg, mb := oklchToLinearSRGB(L, mid, h)
		if inSRGBGamut(mr, mg, mb) {
			lo = mid
		} else {
			hi = mid
		}
	}
	r, g, b = oklchToLinearSRGB(L, lo, h)
	return math.Min(1, math.Max(0, r)), math.Min(1, math.Max(0, g)), math.Min(1, math.Max(0, b))
}

func shadeAtTargetLuminance(targetY, wantC, h float64) string {
	loL, hiL := 0.0, 1.0
	var bestR, bestG, bestB float64
	for i := 0; i < 40; i++ {
		midL := 0.5 * (loL + hiL)
		lr, lg, lb := clampGamutOKLCH(midL, wantC, h)
		bestR, bestG, bestB = lr, lg, lb
		y := linearLuminance(lr, lg, lb)
		if y < targetY {
			loL = midL
		} else {
			hiL = midL
		}
	}
	r8 := uint8(math.Round(linearChannelToSRGB(bestR) * 255.0))
	g8 := uint8(math.Round(linearChannelToSRGB(bestG) * 255.0))
	b8 := uint8(math.Round(linearChannelToSRGB(bestB) * 255.0))
	return formatHexRGB(r8, g8, b8)
}

// GenerateOKLCHScale generates a deterministic 11-step (50..950) shade ramp
// anchored to calibrated WCAG relative luminance targets while preserving the
// input accent hex's OKLCH hue angle.
func GenerateOKLCHScale(accentHex string) (BrandingScale, error) {
	r, g, b, err := parseHexRGB(accentHex)
	if err != nil {
		return nil, err
	}
	_, baseC, h := srgbToOKLCH(r, g, b)
	if baseC < 0.002 {
		baseC = 0
	}
	out := make(BrandingScale, len(oklchShadeAnchors))
	for _, anchor := range oklchShadeAnchors {
		out[anchor.step] = shadeAtTargetLuminance(anchor.targetY, baseC*anchor.chromaFactor, h)
	}
	return out, nil
}

func pickAutoForeground(darkFg string, bgs ...string) string {
	const white = "#ffffff"
	minContrast := func(fg string) (float64, float64) {
		minR := 21.0
		firstR := 21.0
		for i, bg := range bgs {
			if bg == "" {
				continue
			}
			r, err := ContrastRatio(fg, bg)
			if err != nil {
				continue
			}
			if i == 0 {
				firstR = r
			}
			if r < minR {
				minR = r
			}
		}
		return minR, firstR
	}
	wMin, wFirst := minContrast(white)
	dMin, dFirst := minContrast(darkFg)
	if dMin > wMin || (dMin == wMin && dFirst > wFirst) {
		return darkFg
	}
	return white
}

// resolveAndValidatePalette computes effective scales and light/dark semantic
// tokens for cfg, returning validation errors on invalid hex strings, unknown
// scale step keys, or WCAG 2.1 AA contrast violations.
func resolveAndValidatePalette(cfg *BrandingConfig) (
	scales map[string]BrandingScale,
	light BrandingSemanticMode,
	dark BrandingSemanticMode,
	hasCustomPalette bool,
	errs []ValidationError,
) {
	scales = map[string]BrandingScale{
		"primary": DefaultPrimaryScale(),
		"neutral": DefaultNeutralScale(),
		"success": DefaultSuccessScale(),
		"warning": DefaultWarningScale(),
		"danger":  DefaultDangerScale(),
	}
	light = DefaultLightSemanticMode()
	dark = DefaultDarkSemanticMode()

	if cfg == nil {
		return scales, light, dark, false, nil
	}

	accent := strings.TrimSpace(cfg.AccentColor)
	hasPalette := cfg.Palette != nil && !cfg.Palette.IsZero()
	hasCustomPalette = accent != "" || hasPalette
	if !hasCustomPalette {
		return scales, light, dark, false, nil
	}

	if accent != "" {
		ramp, err := GenerateOKLCHScale(accent)
		if err != nil {
			errs = append(errs, ValidationError{
				Path:    BrandingSectionKey + ".accent_color",
				Message: err.Error(),
			})
		} else {
			scales["primary"] = ramp
			accent = normalizeHex(accent)
		}
	}

	allowedSteps := make(map[string]bool, len(ScaleSteps))
	stepOrder := make(map[string]int, len(ScaleSteps))
	for idx, st := range ScaleSteps {
		allowedSteps[st] = true
		stepOrder[st] = idx
	}

	validateAndOverlayScale := func(name string, override BrandingScale) bool {
		if len(override) == 0 {
			return false
		}
		steps := make([]string, 0, len(override))
		for step := range override {
			steps = append(steps, step)
		}
		sort.Slice(steps, func(i, j int) bool {
			oi, okI := stepOrder[steps[i]]
			oj, okJ := stepOrder[steps[j]]
			if okI && okJ {
				return oi < oj
			}
			if okI != okJ {
				return okI
			}
			return steps[i] < steps[j]
		})
		for _, step := range steps {
			val := override[step]
			path := fmt.Sprintf("%s.palette.%s.%s", BrandingSectionKey, name, step)
			if !allowedSteps[step] {
				errs = append(errs, ValidationError{
					Path:    path,
					Message: fmt.Sprintf("unknown shade step %q (allowed: %s)", step, strings.Join(ScaleSteps, ", ")),
				})
				continue
			}
			if !IsValidHexColor(val) {
				errs = append(errs, ValidationError{
					Path:    path,
					Message: "must be a 6-digit #RRGGBB hex color",
				})
				continue
			}
			scales[name][step] = normalizeHex(val)
		}
		return true
	}

	var customPrimaryScale, customNeutralScale, customSuccessScale, customWarningScale, customDangerScale bool
	if cfg.Palette != nil {
		customPrimaryScale = validateAndOverlayScale("primary", cfg.Palette.Primary)
		customNeutralScale = validateAndOverlayScale("neutral", cfg.Palette.Neutral)
		customSuccessScale = validateAndOverlayScale("success", cfg.Palette.Success)
		customWarningScale = validateAndOverlayScale("warning", cfg.Palette.Warning)
		customDangerScale = validateAndOverlayScale("danger", cfg.Palette.Danger)
	}

	validateModeHexes := func(modeName string, m *BrandingSemanticMode) {
		if m == nil {
			return
		}
		fields := []struct {
			key string
			val string
		}{
			{"bg", m.Bg},
			{"bg_subtle", m.BgSubtle},
			{"surface", m.Surface},
			{"surface_raised", m.SurfaceRaised},
			{"text", m.Text},
			{"text_secondary", m.TextSecondary},
			{"text_muted", m.TextMuted},
			{"border", m.Border},
			{"primary", m.Primary},
			{"primary_hover", m.PrimaryHover},
			{"primary_active", m.PrimaryActive},
			{"primary_text", m.PrimaryText},
		}
		for _, f := range fields {
			if f.val == "" {
				continue
			}
			if !IsValidHexColor(f.val) {
				errs = append(errs, ValidationError{
					Path:    fmt.Sprintf("%s.palette.%s.%s", BrandingSectionKey, modeName, f.key),
					Message: "must be a 6-digit #RRGGBB hex color",
				})
			}
		}
	}

	var lightIn, darkIn *BrandingSemanticMode
	if cfg.Palette != nil {
		lightIn = cfg.Palette.Light
		darkIn = cfg.Palette.Dark
		validateModeHexes("light", lightIn)
		validateModeHexes("dark", darkIn)
	}
	if len(errs) > 0 {
		return scales, light, dark, hasCustomPalette, errs
	}

	// When neither accent_color nor palette.primary was provided, seed the
	// primary ramp from an explicit mode primary so --scion-primary-50..950,
	// --sl-color-primary-*, and the sibling mode's derived tokens match the
	// configured brand hue instead of leaking stock blue.
	explicitPrimarySeed := ""
	if accent == "" && !customPrimaryScale {
		switch {
		case lightIn != nil && lightIn.Primary != "":
			explicitPrimarySeed = normalizeHex(lightIn.Primary)
		case darkIn != nil && darkIn.Primary != "":
			explicitPrimarySeed = normalizeHex(darkIn.Primary)
		}
		if explicitPrimarySeed != "" {
			if ramp, err := GenerateOKLCHScale(explicitPrimarySeed); err == nil {
				scales["primary"] = ramp
			}
		}
	}

	neutral := scales["neutral"]
	primary := scales["primary"]

	// Derive semantic backgrounds, surfaces, text, and border from resolved neutral scale,
	// then overlay explicit light/dark fields.
	light.Bg = neutral["50"]
	light.BgSubtle = neutral["100"]
	light.Surface = "#ffffff"
	light.SurfaceRaised = "#ffffff"
	light.Text = neutral["800"]
	light.TextSecondary = neutral["600"]
	light.TextMuted = neutral["500"]
	light.Border = neutral["200"]

	if lightIn != nil {
		if lightIn.Bg != "" {
			light.Bg = normalizeHex(lightIn.Bg)
		}
		if lightIn.BgSubtle != "" {
			light.BgSubtle = normalizeHex(lightIn.BgSubtle)
		}
		if lightIn.Surface != "" {
			light.Surface = normalizeHex(lightIn.Surface)
		}
		if lightIn.SurfaceRaised != "" {
			light.SurfaceRaised = normalizeHex(lightIn.SurfaceRaised)
		}
		if lightIn.Text != "" {
			light.Text = normalizeHex(lightIn.Text)
		}
		if lightIn.TextSecondary != "" {
			light.TextSecondary = normalizeHex(lightIn.TextSecondary)
		}
		if lightIn.TextMuted != "" {
			light.TextMuted = normalizeHex(lightIn.TextMuted)
		}
		if lightIn.Border != "" {
			light.Border = normalizeHex(lightIn.Border)
		}
	}

	dark.Bg = neutral["900"]
	dark.BgSubtle = neutral["800"]
	dark.Surface = neutral["800"]
	dark.SurfaceRaised = neutral["700"]
	dark.Text = neutral["100"]
	dark.TextSecondary = neutral["300"]
	dark.TextMuted = neutral["400"]
	dark.Border = neutral["700"]

	if darkIn != nil {
		if darkIn.Bg != "" {
			dark.Bg = normalizeHex(darkIn.Bg)
		}
		if darkIn.BgSubtle != "" {
			dark.BgSubtle = normalizeHex(darkIn.BgSubtle)
		}
		if darkIn.Surface != "" {
			dark.Surface = normalizeHex(darkIn.Surface)
		}
		if darkIn.SurfaceRaised != "" {
			dark.SurfaceRaised = normalizeHex(darkIn.SurfaceRaised)
		}
		if darkIn.Text != "" {
			dark.Text = normalizeHex(darkIn.Text)
		}
		if darkIn.TextSecondary != "" {
			dark.TextSecondary = normalizeHex(darkIn.TextSecondary)
		}
		if darkIn.TextMuted != "" {
			dark.TextMuted = normalizeHex(darkIn.TextMuted)
		}
		if darkIn.Border != "" {
			dark.Border = normalizeHex(darkIn.Border)
		}
	}

	neutralStepCustomized := func(step string) bool {
		return cfg.Palette != nil && cfg.Palette.Neutral != nil && cfg.Palette.Neutral[step] != ""
	}
	modeTokenCustomized := func(fieldVal, backingNeutralStep string) bool {
		if fieldVal != "" {
			return true
		}
		return backingNeutralStep != "" && neutralStepCustomized(backingNeutralStep)
	}

	basePrimaryChanged := accent != "" || customPrimaryScale || explicitPrimarySeed != ""
	lightPrimaryChanged := basePrimaryChanged ||
		(lightIn != nil && (lightIn.Primary != "" || lightIn.PrimaryHover != "" || lightIn.PrimaryActive != "" || lightIn.PrimaryText != ""))
	darkPrimaryChanged := basePrimaryChanged ||
		(darkIn != nil && (darkIn.Primary != "" || darkIn.PrimaryHover != "" || darkIn.PrimaryActive != "" || darkIn.PrimaryText != ""))

	lightBgOrSurfaceCustomized := modeTokenCustomized(modeField(lightIn, "bg"), "50") ||
		modeTokenCustomized(modeField(lightIn, "surface"), "")
	darkBgOrSurfaceCustomized := modeTokenCustomized(modeField(darkIn, "bg"), "900") ||
		modeTokenCustomized(modeField(darkIn, "surface"), "800")

	recalcLightPrimary := lightPrimaryChanged || lightBgOrSurfaceCustomized
	recalcDarkPrimary := darkPrimaryChanged || darkBgOrSurfaceCustomized

	modePrimaryRamp := func(explicitHex string) BrandingScale {
		if explicitHex != "" && !customPrimaryScale {
			if ramp, err := GenerateOKLCHScale(explicitHex); err == nil {
				return ramp
			}
		}
		return primary
	}

	// Resolve Light primary tokens using the same predicate as the WCAG AA gate below,
	// so neutral-scale background overrides (palette.neutral.50) behave identically to
	// explicit palette.light.bg overrides.
	if recalcLightPrimary {
		if lightIn != nil && lightIn.Primary != "" {
			light.Primary = normalizeHex(lightIn.Primary)
		} else if accent != "" {
			rSurf, _ := ContrastRatio(accent, light.Surface)
			rBg, _ := ContrastRatio(accent, light.Bg)
			if rSurf >= wcagAATextContrastMin && rBg >= wcagAATextContrastMin {
				light.Primary = accent
			} else {
				light.Primary = primary["600"]
			}
		} else {
			light.Primary = primary["600"]
		}

		lightRamp := primary
		if lightIn != nil && lightIn.Primary != "" {
			lightRamp = modePrimaryRamp(light.Primary)
		}

		if lightIn != nil && lightIn.PrimaryHover != "" {
			light.PrimaryHover = normalizeHex(lightIn.PrimaryHover)
		} else {
			light.PrimaryHover = darkerPrimaryStep(lightRamp, light.Primary, "700")
		}

		if lightIn != nil && lightIn.PrimaryActive != "" {
			light.PrimaryActive = normalizeHex(lightIn.PrimaryActive)
		} else {
			activeRamp := lightRamp
			if lightIn != nil && lightIn.Primary == "" && lightIn.PrimaryHover != "" {
				activeRamp = modePrimaryRamp(light.PrimaryHover)
			}
			light.PrimaryActive = darkerPrimaryStep(activeRamp, light.PrimaryHover, "800")
		}

		if lightIn != nil && lightIn.PrimaryText != "" {
			light.PrimaryText = normalizeHex(lightIn.PrimaryText)
		} else {
			light.PrimaryText = pickAutoForeground(neutral["950"], light.Primary, light.PrimaryHover, light.PrimaryActive)
		}
	}

	// Resolve Dark primary tokens using the same predicate as the WCAG AA gate below,
	// so neutral-scale dark background/surface overrides (palette.neutral.900/800)
	// behave identically to explicit palette.dark.bg/surface overrides.
	if recalcDarkPrimary {
		if darkIn != nil && darkIn.Primary != "" {
			dark.Primary = normalizeHex(darkIn.Primary)
		} else if accent != "" {
			rSurf, _ := ContrastRatio(accent, dark.Surface)
			rBg, _ := ContrastRatio(accent, dark.Bg)
			if rSurf >= wcagAATextContrastMin && rBg >= wcagAATextContrastMin {
				dark.Primary = accent
			} else {
				dark.Primary = primary["400"]
			}
		} else {
			dark.Primary = primary["400"]
		}

		darkRamp := primary
		if darkIn != nil && darkIn.Primary != "" {
			darkRamp = modePrimaryRamp(dark.Primary)
		}

		if darkIn != nil && darkIn.PrimaryHover != "" {
			dark.PrimaryHover = normalizeHex(darkIn.PrimaryHover)
		} else {
			dark.PrimaryHover = lighterPrimaryStep(darkRamp, dark.Primary, "300")
		}

		if darkIn != nil && darkIn.PrimaryActive != "" {
			dark.PrimaryActive = normalizeHex(darkIn.PrimaryActive)
		} else {
			activeRamp := darkRamp
			if darkIn != nil && darkIn.Primary == "" && darkIn.PrimaryHover != "" {
				activeRamp = modePrimaryRamp(dark.PrimaryHover)
			}
			dark.PrimaryActive = lighterPrimaryStep(activeRamp, dark.PrimaryHover, "200")
		}

		if darkIn != nil && darkIn.PrimaryText != "" {
			dark.PrimaryText = normalizeHex(darkIn.PrimaryText)
		} else {
			dark.PrimaryText = pickAutoForeground(neutral["950"], dark.Primary, dark.PrimaryHover, dark.PrimaryActive)
		}
	}

	// --- WCAG 2.1 AA Contrast Matrix Enforcement ---
	checkContrast := func(path, fgHex, targetLabel, bgHex string, minRatio float64) {
		ratio, err := ContrastRatio(fgHex, bgHex)
		if err != nil {
			return
		}
		if ratio+1e-9 < minRatio {
			errs = append(errs, ValidationError{
				Path: path,
				Message: fmt.Sprintf(
					"contrast (%s) against %s (%s) is %.2f:1, minimum WCAG AA is %.2f:1",
					fgHex, targetLabel, bgHex, ratio, minRatio,
				),
			})
		}
	}

	// 1. Body, secondary, and muted text vs backgrounds & surfaces (4.5:1)
	type tokenInfo struct {
		name       string
		hex        string
		customized bool
	}
	lightTexts := []tokenInfo{
		{"text", light.Text, modeTokenCustomized(modeField(lightIn, "text"), "800")},
		{"text_secondary", light.TextSecondary, modeTokenCustomized(modeField(lightIn, "text_secondary"), "600")},
		{"text_muted", light.TextMuted, modeTokenCustomized(modeField(lightIn, "text_muted"), "500")},
	}
	lightBgs := []tokenInfo{
		{"bg", light.Bg, modeTokenCustomized(modeField(lightIn, "bg"), "50")},
		{"bg_subtle", light.BgSubtle, modeTokenCustomized(modeField(lightIn, "bg_subtle"), "100")},
		{"surface", light.Surface, modeTokenCustomized(modeField(lightIn, "surface"), "")},
		{"surface_raised", light.SurfaceRaised, modeTokenCustomized(modeField(lightIn, "surface_raised"), "")},
	}
	for _, tTok := range lightTexts {
		for _, bTok := range lightBgs {
			if tTok.customized || bTok.customized {
				checkContrast(
					fmt.Sprintf("%s.palette.light.%s", BrandingSectionKey, tTok.name),
					tTok.hex,
					"light."+bTok.name,
					bTok.hex,
					wcagAATextContrastMin,
				)
			}
		}
	}

	darkTexts := []tokenInfo{
		{"text", dark.Text, modeTokenCustomized(modeField(darkIn, "text"), "100")},
		{"text_secondary", dark.TextSecondary, modeTokenCustomized(modeField(darkIn, "text_secondary"), "300")},
		{"text_muted", dark.TextMuted, modeTokenCustomized(modeField(darkIn, "text_muted"), "400")},
	}
	darkBgs := []tokenInfo{
		{"bg", dark.Bg, modeTokenCustomized(modeField(darkIn, "bg"), "900")},
		{"bg_subtle", dark.BgSubtle, modeTokenCustomized(modeField(darkIn, "bg_subtle"), "800")},
		{"surface", dark.Surface, modeTokenCustomized(modeField(darkIn, "surface"), "800")},
		{"surface_raised", dark.SurfaceRaised, modeTokenCustomized(modeField(darkIn, "surface_raised"), "700")},
	}
	for _, tTok := range darkTexts {
		for _, bTok := range darkBgs {
			if tTok.customized || bTok.customized {
				checkContrast(
					fmt.Sprintf("%s.palette.dark.%s", BrandingSectionKey, tTok.name),
					tTok.hex,
					"dark."+bTok.name,
					bTok.hex,
					wcagAATextContrastMin,
				)
			}
		}
	}

	// 2 & 3 & 6 (focus ring): Primary vs bg/surface (4.5:1 text/links & >=3.0:1 focus)
	// and primary_text vs primary/primary_hover/primary_active (4.5:1).
	if recalcLightPrimary {
		checkContrast(BrandingSectionKey+".palette.light.primary", light.Primary, "light.bg", light.Bg, wcagAATextContrastMin)
		checkContrast(BrandingSectionKey+".palette.light.primary", light.Primary, "light.surface", light.Surface, wcagAATextContrastMin)
		checkContrast(BrandingSectionKey+".palette.light.primary_text", light.PrimaryText, "light.primary", light.Primary, wcagAATextContrastMin)
		checkContrast(BrandingSectionKey+".palette.light.primary_text", light.PrimaryText, "light.primary_hover", light.PrimaryHover, wcagAATextContrastMin)
		checkContrast(BrandingSectionKey+".palette.light.primary_text", light.PrimaryText, "light.primary_active", light.PrimaryActive, wcagAATextContrastMin)
	}
	if recalcDarkPrimary {
		checkContrast(BrandingSectionKey+".palette.dark.primary", dark.Primary, "dark.bg", dark.Bg, wcagAATextContrastMin)
		checkContrast(BrandingSectionKey+".palette.dark.primary", dark.Primary, "dark.surface", dark.Surface, wcagAATextContrastMin)
		checkContrast(BrandingSectionKey+".palette.dark.primary_text", dark.PrimaryText, "dark.primary", dark.Primary, wcagAATextContrastMin)
		checkContrast(BrandingSectionKey+".palette.dark.primary_text", dark.PrimaryText, "dark.primary_hover", dark.PrimaryHover, wcagAATextContrastMin)
		checkContrast(BrandingSectionKey+".palette.dark.primary_text", dark.PrimaryText, "dark.primary_active", dark.PrimaryActive, wcagAATextContrastMin)
	}

	// 4 & 5: Shoelace filled button/badge contrast — Shoelace 2.20.1 uses
	// --sl-color-neutral-0 as the foreground for all 5 filled variants:
	// #ffffff on step 600 in light mode; neutral-800 on step 300 (--sl-color-*-600)
	// and hover step 400 (--sl-color-*-500) in dark mode, matching theme.css —
	// and status badge contrast (light 800 on 100/200, dark 300 on 20% 500 over
	// dark.surface >= 4.5:1).
	type rampCheck struct {
		name       string
		customized bool
		lightBgKey string
	}
	ramps := []rampCheck{
		{"primary", basePrimaryChanged, "100"},
		{"neutral", customNeutralScale, "200"},
		{"success", customSuccessScale, "100"},
		{"warning", customWarningScale, "100"},
		{"danger", customDangerScale, "100"},
	}
	rampStepCustomized := func(rampName, step string) bool {
		if rampName == "primary" && (accent != "" || explicitPrimarySeed != "") {
			return true
		}
		if cfg.Palette == nil {
			return false
		}
		var m BrandingScale
		switch rampName {
		case "primary":
			m = cfg.Palette.Primary
		case "neutral":
			m = cfg.Palette.Neutral
		case "success":
			m = cfg.Palette.Success
		case "warning":
			m = cfg.Palette.Warning
		case "danger":
			m = cfg.Palette.Danger
		}
		return m != nil && m[step] != ""
	}
	darkSurfaceCustomized := modeTokenCustomized(modeField(darkIn, "surface"), "800")
	for _, rc := range ramps {
		sc := scales[rc.name]
		if rampStepCustomized(rc.name, "600") {
			// Light Shoelace filled button/badge: #ffffff (--sl-color-neutral-0) on step 600.
			checkContrast(
				fmt.Sprintf("%s.palette.%s.600", BrandingSectionKey, rc.name),
				"#ffffff",
				fmt.Sprintf("light --sl-color-%s-600", rc.name),
				sc["600"],
				wcagAATextContrastMin,
			)
		}
		if rampStepCustomized(rc.name, "300") || neutralStepCustomized("800") {
			// Dark Shoelace filled button/badge: neutral-800 (--sl-color-neutral-0) on
			// step 300 (--sl-color-<variant>-600).
			checkContrast(
				fmt.Sprintf("%s.palette.%s.300", BrandingSectionKey, rc.name),
				neutral["800"],
				fmt.Sprintf("dark --sl-color-%s-600 (step 300)", rc.name),
				sc["300"],
				wcagAATextContrastMin,
			)
		}
		if rampStepCustomized(rc.name, "400") || neutralStepCustomized("800") {
			// Dark Shoelace filled button hover: neutral-800 (--sl-color-neutral-0) on
			// step 400 (--sl-color-<variant>-500).
			checkContrast(
				fmt.Sprintf("%s.palette.%s.400", BrandingSectionKey, rc.name),
				neutral["800"],
				fmt.Sprintf("dark --sl-color-%s-500 hover (step 400)", rc.name),
				sc["400"],
				wcagAATextContrastMin,
			)
		}
		if rc.customized {
			// Light badge: step 800 text on step 100 (or 200 for neutral) bg
			checkContrast(
				fmt.Sprintf("%s.palette.%s.800", BrandingSectionKey, rc.name),
				sc["800"],
				fmt.Sprintf("light badge %s-%s", rc.name, rc.lightBgKey),
				sc[rc.lightBgKey],
				wcagAATextContrastMin,
			)
		}
		if rc.customized || darkSurfaceCustomized {
			// Dark badge: step 300 text on 20% alpha step 500 composited over dark.surface
			ratio, err := compositeAlphaContrast(sc["300"], sc["500"], 0.20, dark.Surface)
			if err == nil && ratio+1e-9 < wcagAATextContrastMin {
				errs = append(errs, ValidationError{
					Path: fmt.Sprintf("%s.palette.%s.300", BrandingSectionKey, rc.name),
					Message: fmt.Sprintf(
						"dark badge contrast (%s on 20%% %s over dark.surface %s) is %.2f:1, minimum WCAG AA is %.2f:1",
						sc["300"], sc["500"], dark.Surface, ratio, wcagAATextContrastMin,
					),
				})
			}
		}
	}

	// 6. Non-text UI component boundary contrast (3.0:1): neutral input/hover borders vs surface.
	if neutralStepCustomized("600") || modeTokenCustomized(modeField(lightIn, "surface"), "") {
		checkContrast(
			BrandingSectionKey+".palette.neutral.600",
			neutral["600"],
			"light.surface (UI border contrast)",
			light.Surface,
			wcagAAUIContrastMin,
		)
	}
	if neutralStepCustomized("400") || darkSurfaceCustomized {
		checkContrast(
			BrandingSectionKey+".palette.neutral.400",
			neutral["400"],
			"dark.surface (UI border contrast)",
			dark.Surface,
			wcagAAUIContrastMin,
		)
	}

	return scales, light, dark, hasCustomPalette, errs
}

func modeField(m *BrandingSemanticMode, field string) string {
	if m == nil {
		return ""
	}
	switch field {
	case "bg":
		return m.Bg
	case "bg_subtle":
		return m.BgSubtle
	case "surface":
		return m.Surface
	case "surface_raised":
		return m.SurfaceRaised
	case "text":
		return m.Text
	case "text_secondary":
		return m.TextSecondary
	case "text_muted":
		return m.TextMuted
	case "border":
		return m.Border
	}
	return ""
}

func darkerPrimaryStep(scale BrandingScale, baseHex, fallbackStep string) string {
	baseY, err := RelativeLuminance(baseHex)
	if err != nil {
		return scale[fallbackStep]
	}
	for _, st := range []string{"600", "700", "800", "900", "950"} {
		cand := scale[st]
		if cand == "" {
			continue
		}
		if y, err := RelativeLuminance(cand); err == nil && y < baseY-0.015 {
			return cand
		}
	}
	return scale[fallbackStep]
}

func lighterPrimaryStep(scale BrandingScale, baseHex, fallbackStep string) string {
	baseY, err := RelativeLuminance(baseHex)
	if err != nil {
		return scale[fallbackStep]
	}
	for _, st := range []string{"300", "200", "100", "50"} {
		cand := scale[st]
		if cand == "" {
			continue
		}
		if y, err := RelativeLuminance(cand); err == nil && y > baseY+0.03 {
			return cand
		}
	}
	return scale[fallbackStep]
}

// buildBrandingCSSText renders validated CSS custom property declarations for
// custom palettes (light, explicit dark, and system dark modes) and/or
// mode-independent style overrides (such as corner radius and sans-serif font).
func buildBrandingCSSText(hasCustomPalette bool, scales map[string]BrandingScale, light, dark BrandingSemanticMode, corners, fontSans, fontSansStack string) string {
	hasCustomCorners := corners != "" && corners != BrandCornersDefault
	hasCustomFontSans := fontSans != "" && fontSans != BrandFontSansDefault && fontSansStack != ""
	if !hasCustomPalette && !hasCustomCorners && !hasCustomFontSans {
		return ""
	}
	var b strings.Builder
	b.WriteString("html:root, :host, .sl-theme-light {\n")
	if hasCustomPalette {
		for _, ramp := range []string{"primary", "neutral", "success", "warning", "danger"} {
			sc := scales[ramp]
			for _, step := range ScaleSteps {
				if v := sc[step]; v != "" {
					fmt.Fprintf(&b, "  --scion-%s-%s: %s;\n", ramp, step, v)
					fmt.Fprintf(&b, "  --sl-color-%s-%s: %s;\n", ramp, step, v)
				}
			}
		}
		writeSemanticVars(&b, light)
	}
	if hasCustomCorners {
		writeCornerVars(&b, corners)
	}
	if hasCustomFontSans {
		fmt.Fprintf(&b, "  --scion-font-sans: %s;\n", fontSansStack)
	}
	b.WriteString("}\n")

	if hasCustomPalette {
		b.WriteString("html:root.sl-theme-dark, html:root[data-theme='dark'], :root.dark {\n")
		writeSemanticVars(&b, dark)
		b.WriteString("}\n")

		b.WriteString("@media (prefers-color-scheme: dark) {\n  html:root:not([data-theme='light']) {\n")
		writeSemanticVarsIndented(&b, dark, "    ")
		b.WriteString("  }\n}\n")
	}
	return b.String()
}

func writeCornerVars(b *strings.Builder, corners string) {
	switch corners {
	case BrandCornersSquare:
		b.WriteString("  --scion-radius-sm: 0;\n")
		b.WriteString("  --scion-radius: 0;\n")
		b.WriteString("  --scion-radius-md: 0;\n")
		b.WriteString("  --scion-radius-lg: 0;\n")
		b.WriteString("  --scion-radius-xl: 0;\n")
	case BrandCornersRounded:
		b.WriteString("  --scion-radius-sm: 0.375rem;\n")
		b.WriteString("  --scion-radius: 0.75rem;\n")
		b.WriteString("  --scion-radius-md: 0.75rem;\n")
		b.WriteString("  --scion-radius-lg: 1rem;\n")
		b.WriteString("  --scion-radius-xl: 1.25rem;\n")
	}
}

func writeSemanticVars(b *strings.Builder, m BrandingSemanticMode) {
	writeSemanticVarsIndented(b, m, "  ")
}

func writeSemanticVarsIndented(b *strings.Builder, m BrandingSemanticMode, indent string) {
	fmt.Fprintf(b, "%s--scion-bg: %s;\n", indent, m.Bg)
	fmt.Fprintf(b, "%s--scion-bg-subtle: %s;\n", indent, m.BgSubtle)
	fmt.Fprintf(b, "%s--scion-surface: %s;\n", indent, m.Surface)
	fmt.Fprintf(b, "%s--scion-surface-raised: %s;\n", indent, m.SurfaceRaised)
	fmt.Fprintf(b, "%s--scion-text: %s;\n", indent, m.Text)
	fmt.Fprintf(b, "%s--scion-text-secondary: %s;\n", indent, m.TextSecondary)
	fmt.Fprintf(b, "%s--scion-text-muted: %s;\n", indent, m.TextMuted)
	fmt.Fprintf(b, "%s--scion-border: %s;\n", indent, m.Border)
	fmt.Fprintf(b, "%s--scion-primary: %s;\n", indent, m.Primary)
	fmt.Fprintf(b, "%s--scion-primary-hover: %s;\n", indent, m.PrimaryHover)
	fmt.Fprintf(b, "%s--scion-primary-active: %s;\n", indent, m.PrimaryActive)
	fmt.Fprintf(b, "%s--scion-primary-text: %s;\n", indent, m.PrimaryText)
}
