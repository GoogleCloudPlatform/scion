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
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// BrandingSectionKey is the settings.yaml / koanf subtree path for Hub branding.
	BrandingSectionKey = "server.hub.branding"

	// DefaultBrandName is stock Scion's product display name.
	DefaultBrandName = "Scion"

	// DefaultBrandThemeColor is stock Scion's dark-surface meta theme-color.
	DefaultBrandThemeColor = "#1e293b"

	// AttributionShown and AttributionHidden are the valid string enum values
	// for BrandingConfig.Attribution. No boolean is used.
	AttributionShown  = "shown"
	AttributionHidden = "hidden"

	// BrandCornersDefault, BrandCornersSquare, and BrandCornersRounded are the
	// valid string enum values for BrandingConfig.Corners.
	BrandCornersDefault = "default"
	BrandCornersSquare  = "square"
	BrandCornersRounded = "rounded"

	MaxBrandNameRunes      = 64
	MaxBrandShortNameRunes = 24
	MaxBrandSubtitleRunes  = 80
	MaxBrandHTTPSURLLen    = 2048
	MaxBrandPathLen        = 512

	MaxBrandAssetBytes = 128 << 10 // 128 KiB decoded per data: URI
	MaxBrandTotalBytes = 512 << 10 // 512 KiB decoded across all branding data: URIs
)

// Brand asset slot identifiers for /brand/{slot}.
const (
	BrandSlotLogoLight       = "logo-light"
	BrandSlotLogoDark        = "logo-dark"
	BrandSlotFavicon         = "favicon"
	BrandSlotAppleTouchIcon  = "apple-touch-icon"
	BrandSlotIcon192         = "icon-192"
	BrandSlotIcon512         = "icon-512"
	BrandSlotIconMaskable512 = "icon-maskable-512"
)

// IsValidBrandSlot reports whether slot is one of the canonical /brand/{slot}
// asset slot names produced by ResolveBranding.
func IsValidBrandSlot(slot string) bool {
	switch slot {
	case BrandSlotLogoLight,
		BrandSlotLogoDark,
		BrandSlotFavicon,
		BrandSlotAppleTouchIcon,
		BrandSlotIcon192,
		BrandSlotIcon512,
		BrandSlotIconMaskable512:
		return true
	default:
		return false
	}
}

// ScaleSteps enumerates the allowed shade keys in BrandingScale.
var ScaleSteps = []string{"50", "100", "200", "300", "400", "500", "600", "700", "800", "900", "950"}

// BrandingScale maps shade steps ("50".."950") to #RRGGBB hex colors.
type BrandingScale map[string]string

// IsZero reports whether the scale map is empty.
func (s BrandingScale) IsZero() bool {
	return len(s) == 0
}

// Clone returns a deep copy of s, or nil when s is empty.
func (s BrandingScale) Clone() BrandingScale {
	if len(s) == 0 {
		return nil
	}
	cp := make(BrandingScale, len(s))
	for k, v := range s {
		cp[k] = v
	}
	return cp
}

// BrandingLogoConfig holds optional light and dark brand logo URIs.
type BrandingLogoConfig struct {
	Light string `json:"light,omitempty" yaml:"light,omitempty" koanf:"light"`
	Dark  string `json:"dark,omitempty" yaml:"dark,omitempty" koanf:"dark"`
}

// IsZero reports whether neither logo URI is set.
func (c *BrandingLogoConfig) IsZero() bool {
	return c == nil || (c.Light == "" && c.Dark == "")
}

// Clone returns a deep copy of c, or nil when c is zero.
func (c *BrandingLogoConfig) Clone() *BrandingLogoConfig {
	if c.IsZero() {
		return nil
	}
	return &BrandingLogoConfig{
		Light: c.Light,
		Dark:  c.Dark,
	}
}

// BrandingIconsConfig holds optional favicon and PWA icon URIs.
type BrandingIconsConfig struct {
	Favicon         string `json:"favicon,omitempty" yaml:"favicon,omitempty" koanf:"favicon"`
	AppleTouchIcon  string `json:"apple_touch_icon,omitempty" yaml:"apple_touch_icon,omitempty" koanf:"apple_touch_icon"`
	Icon192         string `json:"icon_192,omitempty" yaml:"icon_192,omitempty" koanf:"icon_192"`
	Icon512         string `json:"icon_512,omitempty" yaml:"icon_512,omitempty" koanf:"icon_512"`
	IconMaskable512 string `json:"icon_maskable_512,omitempty" yaml:"icon_maskable_512,omitempty" koanf:"icon_maskable_512"`
}

// IsZero reports whether no icon URI is set.
func (c *BrandingIconsConfig) IsZero() bool {
	return c == nil || (c.Favicon == "" &&
		c.AppleTouchIcon == "" &&
		c.Icon192 == "" &&
		c.Icon512 == "" &&
		c.IconMaskable512 == "")
}

// Clone returns a deep copy of c, or nil when c is zero.
func (c *BrandingIconsConfig) Clone() *BrandingIconsConfig {
	if c.IsZero() {
		return nil
	}
	return &BrandingIconsConfig{
		Favicon:         c.Favicon,
		AppleTouchIcon:  c.AppleTouchIcon,
		Icon192:         c.Icon192,
		Icon512:         c.Icon512,
		IconMaskable512: c.IconMaskable512,
	}
}

// BrandingSemanticMode holds semantic #RRGGBB token values for one theme mode.
type BrandingSemanticMode struct {
	Bg            string `json:"bg,omitempty" yaml:"bg,omitempty" koanf:"bg"`
	BgSubtle      string `json:"bg_subtle,omitempty" yaml:"bg_subtle,omitempty" koanf:"bg_subtle"`
	Surface       string `json:"surface,omitempty" yaml:"surface,omitempty" koanf:"surface"`
	SurfaceRaised string `json:"surface_raised,omitempty" yaml:"surface_raised,omitempty" koanf:"surface_raised"`
	Text          string `json:"text,omitempty" yaml:"text,omitempty" koanf:"text"`
	TextSecondary string `json:"text_secondary,omitempty" yaml:"text_secondary,omitempty" koanf:"text_secondary"`
	TextMuted     string `json:"text_muted,omitempty" yaml:"text_muted,omitempty" koanf:"text_muted"`
	Border        string `json:"border,omitempty" yaml:"border,omitempty" koanf:"border"`
	Primary       string `json:"primary,omitempty" yaml:"primary,omitempty" koanf:"primary"`
	PrimaryHover  string `json:"primary_hover,omitempty" yaml:"primary_hover,omitempty" koanf:"primary_hover"`
	PrimaryActive string `json:"primary_active,omitempty" yaml:"primary_active,omitempty" koanf:"primary_active"`
	PrimaryText   string `json:"primary_text,omitempty" yaml:"primary_text,omitempty" koanf:"primary_text"`
}

// IsZero reports whether all semantic token fields are empty.
func (m *BrandingSemanticMode) IsZero() bool {
	return m == nil || (*m == BrandingSemanticMode{})
}

// Clone returns a copy of m, or nil when m is zero.
func (m *BrandingSemanticMode) Clone() *BrandingSemanticMode {
	if m.IsZero() {
		return nil
	}
	cp := *m
	return &cp
}

// BrandingPaletteConfig holds optional 50..950 scale overrides and light/dark
// semantic token overrides.
type BrandingPaletteConfig struct {
	Primary BrandingScale         `json:"primary,omitempty" yaml:"primary,omitempty" koanf:"primary"`
	Neutral BrandingScale         `json:"neutral,omitempty" yaml:"neutral,omitempty" koanf:"neutral"`
	Success BrandingScale         `json:"success,omitempty" yaml:"success,omitempty" koanf:"success"`
	Warning BrandingScale         `json:"warning,omitempty" yaml:"warning,omitempty" koanf:"warning"`
	Danger  BrandingScale         `json:"danger,omitempty" yaml:"danger,omitempty" koanf:"danger"`
	Light   *BrandingSemanticMode `json:"light,omitempty" yaml:"light,omitempty" koanf:"light"`
	Dark    *BrandingSemanticMode `json:"dark,omitempty" yaml:"dark,omitempty" koanf:"dark"`
}

// IsZero reports whether all scales and semantic mode overrides are empty.
func (p *BrandingPaletteConfig) IsZero() bool {
	return p == nil || (p.Primary.IsZero() &&
		p.Neutral.IsZero() &&
		p.Success.IsZero() &&
		p.Warning.IsZero() &&
		p.Danger.IsZero() &&
		p.Light.IsZero() &&
		p.Dark.IsZero())
}

// Clone returns a deep copy of p, or nil when p is zero.
func (p *BrandingPaletteConfig) Clone() *BrandingPaletteConfig {
	if p.IsZero() {
		return nil
	}
	return &BrandingPaletteConfig{
		Primary: p.Primary.Clone(),
		Neutral: p.Neutral.Clone(),
		Success: p.Success.Clone(),
		Warning: p.Warning.Clone(),
		Danger:  p.Danger.Clone(),
		Light:   p.Light.Clone(),
		Dark:    p.Dark.Clone(),
	}
}

// BrandingConfig holds Hub-wide UI branding settings (server.hub.branding).
type BrandingConfig struct {
	Name        string                 `json:"name,omitempty" yaml:"name,omitempty" koanf:"name"`
	ShortName   string                 `json:"short_name,omitempty" yaml:"short_name,omitempty" koanf:"short_name"`
	Subtitle    string                 `json:"subtitle,omitempty" yaml:"subtitle,omitempty" koanf:"subtitle"`
	Attribution string                 `json:"attribution,omitempty" yaml:"attribution,omitempty" koanf:"attribution"`
	Corners     string                 `json:"corners,omitempty" yaml:"corners,omitempty" koanf:"corners"`
	Logo        *BrandingLogoConfig    `json:"logo,omitempty" yaml:"logo,omitempty" koanf:"logo"`
	Icons       *BrandingIconsConfig   `json:"icons,omitempty" yaml:"icons,omitempty" koanf:"icons"`
	AccentColor string                 `json:"accent_color,omitempty" yaml:"accent_color,omitempty" koanf:"accent_color"`
	Palette     *BrandingPaletteConfig `json:"palette,omitempty" yaml:"palette,omitempty" koanf:"palette"`
}

// IsZero reports whether no branding fields are configured.
func (c BrandingConfig) IsZero() bool {
	return c.Name == "" &&
		c.ShortName == "" &&
		c.Subtitle == "" &&
		c.Attribution == "" &&
		c.Corners == "" &&
		c.Logo.IsZero() &&
		c.Icons.IsZero() &&
		c.AccentColor == "" &&
		c.Palette.IsZero()
}

// Clone returns a deep copy of c, or nil when c is nil or zero.
func (c *BrandingConfig) Clone() *BrandingConfig {
	if c == nil || c.IsZero() {
		return nil
	}
	return &BrandingConfig{
		Name:        c.Name,
		ShortName:   c.ShortName,
		Subtitle:    c.Subtitle,
		Attribution: c.Attribution,
		Corners:     c.Corners,
		Logo:        c.Logo.Clone(),
		Icons:       c.Icons.Clone(),
		AccentColor: c.AccentColor,
		Palette:     c.Palette.Clone(),
	}
}

// CloneValue returns a deep value copy of c.
func (c BrandingConfig) CloneValue() BrandingConfig {
	cp := (&c).Clone()
	if cp == nil {
		return BrandingConfig{}
	}
	return *cp
}

// ResolvedBrandAsset stores a validated, decoded data: image asset for /brand/{slot}.
type ResolvedBrandAsset struct {
	Slot      string
	MediaType string
	Data      []byte
	SHA8      string
	URL       string
}

// PublicBranding is the public, pre-auth JSON representation delivered via
// __SCION_DATA__.branding and GET /api/v1/settings/public.
//
// Invariant: PublicBranding MUST NOT contain boolean fields (see GEMINI.md
// rule on /api/v1/settings/public). Attribution uses "shown" | "hidden".
type PublicBranding struct {
	Name                string                   `json:"name"`
	ShortName           string                   `json:"shortName"`
	Subtitle            string                   `json:"subtitle,omitempty"`
	Attribution         string                   `json:"attribution"`
	Corners             string                   `json:"corners,omitempty"`
	LogoLight           string                   `json:"logoLight,omitempty"`
	LogoDark            string                   `json:"logoDark,omitempty"`
	FaviconURL          string                   `json:"faviconUrl,omitempty"`
	AppleTouchIconURL   string                   `json:"appleTouchIconUrl,omitempty"`
	NotificationIconURL string                   `json:"notificationIconUrl,omitempty"`
	ThemeColor          string                   `json:"themeColor"`
	CSSText             string                   `json:"cssText,omitempty"`
	Light               *BrandingSemanticMode    `json:"light,omitempty"`
	Dark                *BrandingSemanticMode    `json:"dark,omitempty"`
	Scales              map[string]BrandingScale `json:"scales,omitempty"`
}

// ResolvedBranding holds the validated, immutable in-memory Hub branding snapshot.
type ResolvedBranding struct {
	Name                string
	ShortName           string
	Subtitle            string
	Attribution         string
	Corners             string
	LogoLight           string
	LogoDark            string
	FaviconURL          string
	FaviconMediaType    string
	AppleTouchIconURL   string
	Icon192URL          string
	Icon512URL          string
	IconMaskable512URL  string
	NotificationIconURL string
	ThemeColor          string
	HasCustomPalette    bool
	HasCustomStyle      bool
	CSSText             string
	Light               BrandingSemanticMode
	Dark                BrandingSemanticMode
	Scales              map[string]BrandingScale
	Assets              map[string]ResolvedBrandAsset
	Malformed           bool
}

// DefaultResolvedBranding returns the compiled stock Scion branding snapshot.
func DefaultResolvedBranding() *ResolvedBranding {
	return &ResolvedBranding{
		Name:        DefaultBrandName,
		ShortName:   DefaultBrandName,
		Attribution: AttributionShown,
		Corners:     BrandCornersDefault,
		ThemeColor:  DefaultBrandThemeColor,
		Light:       DefaultLightSemanticMode(),
		Dark:        DefaultDarkSemanticMode(),
		Scales: map[string]BrandingScale{
			"primary": DefaultPrimaryScale(),
			"neutral": DefaultNeutralScale(),
			"success": DefaultSuccessScale(),
			"warning": DefaultWarningScale(),
			"danger":  DefaultDangerScale(),
		},
	}
}

// IsCustomized reports whether any effective branding value differs from
// compiled stock Scion defaults.
func (r *ResolvedBranding) IsCustomized() bool {
	if r == nil {
		return false
	}
	return r.Name != DefaultBrandName ||
		r.ShortName != DefaultBrandName ||
		r.Subtitle != "" ||
		r.Attribution != AttributionShown ||
		r.LogoLight != "" ||
		r.LogoDark != "" ||
		r.FaviconURL != "" ||
		r.AppleTouchIconURL != "" ||
		r.Icon192URL != "" ||
		r.Icon512URL != "" ||
		r.IconMaskable512URL != "" ||
		r.HasCustomPalette ||
		r.HasCustomStyle
}

// Public returns a deep-copied PublicBranding for pre-auth delivery, or nil
// when the Hub uses compiled stock defaults (!r.IsCustomized()).
func (r *ResolvedBranding) Public() *PublicBranding {
	if !r.IsCustomized() {
		return nil
	}
	out := &PublicBranding{
		Name:                r.Name,
		ShortName:           r.ShortName,
		Subtitle:            r.Subtitle,
		Attribution:         r.Attribution,
		LogoLight:           r.LogoLight,
		LogoDark:            r.LogoDark,
		FaviconURL:          r.FaviconURL,
		AppleTouchIconURL:   r.AppleTouchIconURL,
		NotificationIconURL: r.NotificationIconURL,
		ThemeColor:          r.ThemeColor,
	}
	if r.Corners != "" && r.Corners != BrandCornersDefault {
		out.Corners = r.Corners
	}
	if r.HasCustomPalette || r.HasCustomStyle {
		out.CSSText = r.CSSText
	}
	if r.HasCustomPalette {
		lightCopy := r.Light
		darkCopy := r.Dark
		out.Light = &lightCopy
		out.Dark = &darkCopy
		out.Scales = make(map[string]BrandingScale, len(r.Scales))
		for k, sc := range r.Scales {
			out.Scales[k] = sc.Clone()
		}
	}
	return out
}

// ValidateBranding validates cfg against all text, asset URI, SVG safety, byte
// budget, hex syntax, and WCAG 2.1 AA contrast rules.
func ValidateBranding(cfg *BrandingConfig) []ValidationError {
	_, errs := resolveBrandingInternal(cfg)
	return errs
}

// ResolveBranding resolves cfg into an immutable ResolvedBranding snapshot. If
// cfg is invalid (e.g. hand-edited YAML or corrupted DB row), it returns
// DefaultResolvedBranding() with Malformed=true alongside the validation errors.
func ResolveBranding(cfg *BrandingConfig) (*ResolvedBranding, []ValidationError) {
	resolved, errs := resolveBrandingInternal(cfg)
	if len(errs) > 0 {
		fallback := DefaultResolvedBranding()
		fallback.Malformed = true
		return fallback, errs
	}
	return resolved, nil
}

func resolveBrandingInternal(cfg *BrandingConfig) (*ResolvedBranding, []ValidationError) {
	if cfg == nil || cfg.IsZero() {
		return DefaultResolvedBranding(), nil
	}

	var errs []ValidationError

	// 1. Strings: name, short_name, subtitle, attribution
	name := DefaultBrandName
	if cfg.Name != "" {
		if hasControlChars(cfg.Name) {
			errs = append(errs, ValidationError{
				Path:    BrandingSectionKey + ".name",
				Message: "must not contain control characters",
			})
		} else {
			trimmed := strings.TrimSpace(cfg.Name)
			n := utf8.RuneCountInString(trimmed)
			if n == 0 || utf8.RuneCountInString(cfg.Name) > MaxBrandNameRunes {
				errs = append(errs, ValidationError{
					Path:    BrandingSectionKey + ".name",
					Message: fmt.Sprintf("must be 1..%d non-whitespace characters", MaxBrandNameRunes),
				})
			} else {
				name = trimmed
			}
		}
	}

	shortName := truncateRunes(name, MaxBrandShortNameRunes)
	if cfg.ShortName != "" {
		if hasControlChars(cfg.ShortName) {
			errs = append(errs, ValidationError{
				Path:    BrandingSectionKey + ".short_name",
				Message: "must not contain control characters",
			})
		} else {
			trimmed := strings.TrimSpace(cfg.ShortName)
			n := utf8.RuneCountInString(trimmed)
			if n == 0 || utf8.RuneCountInString(cfg.ShortName) > MaxBrandShortNameRunes {
				errs = append(errs, ValidationError{
					Path:    BrandingSectionKey + ".short_name",
					Message: fmt.Sprintf("must be 1..%d non-whitespace characters", MaxBrandShortNameRunes),
				})
			} else {
				shortName = trimmed
			}
		}
	}

	subtitle := ""
	if cfg.Subtitle != "" {
		if hasControlChars(cfg.Subtitle) {
			errs = append(errs, ValidationError{
				Path:    BrandingSectionKey + ".subtitle",
				Message: "must not contain control characters",
			})
		} else if utf8.RuneCountInString(cfg.Subtitle) > MaxBrandSubtitleRunes {
			errs = append(errs, ValidationError{
				Path:    BrandingSectionKey + ".subtitle",
				Message: fmt.Sprintf("must be at most %d characters", MaxBrandSubtitleRunes),
			})
		} else {
			subtitle = strings.TrimSpace(cfg.Subtitle)
		}
	}

	attribution := AttributionShown
	if cfg.Attribution != "" {
		switch cfg.Attribution {
		case AttributionShown, AttributionHidden:
			attribution = cfg.Attribution
		default:
			errs = append(errs, ValidationError{
				Path:    BrandingSectionKey + ".attribution",
				Message: fmt.Sprintf("must be %q or %q", AttributionShown, AttributionHidden),
			})
		}
	}

	corners := BrandCornersDefault
	if cfg.Corners != "" {
		switch cfg.Corners {
		case BrandCornersDefault, BrandCornersSquare, BrandCornersRounded:
			corners = cfg.Corners
		default:
			errs = append(errs, ValidationError{
				Path:    BrandingSectionKey + ".corners",
				Message: fmt.Sprintf("must be %q, %q, or %q", BrandCornersDefault, BrandCornersSquare, BrandCornersRounded),
			})
		}
	}
	hasCustomStyle := corners != BrandCornersDefault

	// 2. Assets (logos and icons)
	assets := make(map[string]ResolvedBrandAsset)
	var totalAssetBytes int

	resolveSlot := func(raw, fieldPath, slot string, pngOnlyData bool) (resolvedURL, mediaType string) {
		if raw == "" {
			return "", ""
		}
		u, mt, asset, err := parseAndValidateBrandAsset(raw, slot, pngOnlyData)
		if err != nil {
			errs = append(errs, ValidationError{
				Path:    BrandingSectionKey + "." + fieldPath,
				Message: err.Error(),
			})
			return "", ""
		}
		if asset != nil {
			totalAssetBytes += len(asset.Data)
			if totalAssetBytes > MaxBrandTotalBytes {
				errs = append(errs, ValidationError{
					Path:    BrandingSectionKey + "." + fieldPath,
					Message: fmt.Sprintf("total decoded data: asset size exceeds %d bytes (%d KiB)", MaxBrandTotalBytes, MaxBrandTotalBytes>>10),
				})
				return "", ""
			}
			assets[slot] = *asset
		}
		return u, mt
	}

	var logoLight, logoDark string
	if cfg.Logo != nil {
		logoLight, _ = resolveSlot(cfg.Logo.Light, "logo.light", BrandSlotLogoLight, false)
		logoDark, _ = resolveSlot(cfg.Logo.Dark, "logo.dark", BrandSlotLogoDark, false)
		if logoLight != "" && logoDark == "" && cfg.Logo.Dark == "" {
			logoDark = logoLight
		} else if logoDark != "" && logoLight == "" && cfg.Logo.Light == "" {
			logoLight = logoDark
		}
	}

	var faviconURL, faviconMediaType, appleTouchURL, icon192URL, icon512URL, iconMaskable512URL string
	if cfg.Icons != nil {
		faviconURL, faviconMediaType = resolveSlot(cfg.Icons.Favicon, "icons.favicon", BrandSlotFavicon, false)
		appleTouchURL, _ = resolveSlot(cfg.Icons.AppleTouchIcon, "icons.apple_touch_icon", BrandSlotAppleTouchIcon, true)
		icon192URL, _ = resolveSlot(cfg.Icons.Icon192, "icons.icon_192", BrandSlotIcon192, true)
		icon512URL, _ = resolveSlot(cfg.Icons.Icon512, "icons.icon_512", BrandSlotIcon512, true)
		iconMaskable512URL, _ = resolveSlot(cfg.Icons.IconMaskable512, "icons.icon_maskable_512", BrandSlotIconMaskable512, true)
	}
	if appleTouchURL == "" && icon192URL != "" {
		appleTouchURL = icon192URL
	}
	notificationIconURL := icon192URL
	if notificationIconURL == "" {
		notificationIconURL = appleTouchURL
	}
	if notificationIconURL == "" {
		notificationIconURL = faviconURL
	}

	// 3. Palette & WCAG 2.1 AA contrast validation
	scales, light, dark, hasCustomPalette, paletteErrs := resolveAndValidatePalette(cfg)
	errs = append(errs, paletteErrs...)

	if len(errs) > 0 {
		return nil, errs
	}

	themeColor := DefaultBrandThemeColor
	if hasCustomPalette {
		themeColor = dark.Surface
	}
	cssText := ""
	if hasCustomPalette || hasCustomStyle {
		cssText = buildBrandingCSSText(hasCustomPalette, scales, light, dark, corners)
	}
	if len(assets) == 0 {
		assets = nil
	}

	return &ResolvedBranding{
		Name:                name,
		ShortName:           shortName,
		Subtitle:            subtitle,
		Attribution:         attribution,
		Corners:             corners,
		LogoLight:           logoLight,
		LogoDark:            logoDark,
		FaviconURL:          faviconURL,
		FaviconMediaType:    faviconMediaType,
		AppleTouchIconURL:   appleTouchURL,
		Icon192URL:          icon192URL,
		Icon512URL:          icon512URL,
		IconMaskable512URL:  iconMaskable512URL,
		NotificationIconURL: notificationIconURL,
		ThemeColor:          themeColor,
		HasCustomPalette:    hasCustomPalette,
		HasCustomStyle:      hasCustomStyle,
		CSSText:             cssText,
		Light:               light,
		Dark:                dark,
		Scales:              scales,
		Assets:              assets,
	}, nil
}

func isBidiControl(r rune) bool {
	return r == 0x061C || r == 0x200E || r == 0x200F ||
		(r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069)
}

func hasControlChars(s string) bool {
	if !utf8.ValidString(s) {
		return true
	}
	for _, r := range s {
		if unicode.IsControl(r) || isBidiControl(r) {
			return true
		}
	}
	return false
}

func hasControlOrSpace(s string) bool {
	if !utf8.ValidString(s) {
		return true
	}
	for _, r := range s {
		if unicode.IsControl(r) || unicode.IsSpace(r) || isBidiControl(r) {
			return true
		}
	}
	return false
}

func isValidSameOriginSegment(seg string) bool {
	if seg == "" {
		return false
	}
	for i := 0; i < len(seg); i++ {
		c := seg[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '.' || c == '_' || c == '-' || c == '@' {
			continue
		}
		return false
	}
	return true
}

func truncateRunes(s string, maxRunes int) string {
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	var b strings.Builder
	count := 0
	for _, r := range s {
		if count >= maxRunes {
			break
		}
		b.WriteRune(r)
		count++
	}
	return b.String()
}

func parseAndValidateBrandAsset(raw, slot string, pngOnlyData bool) (resolvedURL, mediaType string, asset *ResolvedBrandAsset, err error) {
	if hasControlOrSpace(raw) {
		return "", "", nil, errors.New("URI must not contain whitespace or control characters")
	}

	if strings.HasPrefix(strings.ToLower(raw), "data:") {
		return parseBrandDataURI(raw, slot, pngOnlyData)
	}

	if strings.HasPrefix(raw, "/") {
		if strings.HasPrefix(raw, "//") {
			return "", "", nil, errors.New("protocol-relative '//' paths are not allowed; use https:// or a single-slash same-origin path")
		}
		if len(raw) > MaxBrandPathLen {
			return "", "", nil, fmt.Errorf("same-origin path exceeds maximum length of %d characters", MaxBrandPathLen)
		}
		if strings.IndexByte(raw, '%') >= 0 {
			return "", "", nil, errors.New("same-origin asset path must not contain percent-encoding ('%')")
		}
		if strings.ContainsAny(raw, "?#\\") {
			return "", "", nil, errors.New("same-origin path must not contain query, fragment, or backslash characters")
		}
		for _, seg := range strings.Split(raw[1:], "/") {
			if seg == ".." || seg == "." {
				return "", "", nil, errors.New("same-origin path must not contain '.' or '..' traversal segments")
			}
			if !isValidSameOriginSegment(seg) {
				return "", "", nil, errors.New("same-origin asset path segments may contain only ASCII letters, digits, '.', '_', '-', or '@' without empty segments")
			}
		}
		if reservedPrefix := matchingReservedHubAssetPrefix(raw); reservedPrefix != "" {
			return "", "", nil, fmt.Errorf("same-origin asset path must not target reserved Hub route prefix %q", reservedPrefix)
		}
		mt := inferAssetMediaTypeFromPath(raw)
		if mt == "" {
			return "", "", nil, errors.New("same-origin asset path must end with .svg, .png, .webp, or .ico")
		}
		return raw, mt, nil, nil
	}

	if len(raw) > MaxBrandHTTPSURLLen {
		return "", "", nil, fmt.Errorf("URL exceeds maximum length of %d characters", MaxBrandHTTPSURLLen)
	}
	if strings.ContainsAny(raw, "\"'`()<>\\") {
		return "", "", nil, errors.New("https:// URL must not contain quote, parenthesis, angle-bracket, or backslash characters")
	}
	u, parseErr := url.Parse(raw)
	if parseErr != nil {
		return "", "", nil, fmt.Errorf("invalid URL: %w", parseErr)
	}
	if strings.EqualFold(u.Scheme, "http") {
		return "", "", nil, errors.New("http:// URLs are not allowed (blocked by Hub Content-Security-Policy img-src); use https://")
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return "", "", nil, errors.New("asset URI must use https://, a same-origin /path, or a base64 data:image URI")
	}
	if u.Host == "" {
		return "", "", nil, errors.New("https:// URL must include a non-empty host")
	}
	if u.User != nil {
		return "", "", nil, errors.New("https:// URL must not include user credentials")
	}
	return raw, inferAssetMediaTypeFromPath(u.Path), nil, nil
}

var reservedHubAssetPrefixes = []string{
	"/auth",
	"/api",
	"/brand",
	"/events",
	"/.well-known",
	"/health",
	"/healthz",
	"/readyz",
}

func matchingReservedHubAssetPrefix(rawPath string) string {
	lower := strings.ToLower(rawPath)
	for _, prefix := range reservedHubAssetPrefixes {
		if lower == prefix || strings.HasPrefix(lower, prefix+"/") {
			return prefix
		}
	}
	return ""
}

func inferAssetMediaTypeFromPath(p string) string {
	lower := strings.ToLower(p)
	switch {
	case strings.HasSuffix(lower, ".svg"):
		return "image/svg+xml"
	case strings.HasSuffix(lower, ".png"):
		return "image/png"
	case strings.HasSuffix(lower, ".webp"):
		return "image/webp"
	case strings.HasSuffix(lower, ".ico"):
		return "image/x-icon"
	default:
		return ""
	}
}

func parseBrandDataURI(raw, slot string, pngOnly bool) (resolvedURL, mediaType string, asset *ResolvedBrandAsset, err error) {
	commaIdx := strings.IndexByte(raw, ',')
	if commaIdx < 0 {
		return "", "", nil, errors.New("invalid data: URI: missing ',' payload separator")
	}
	header := strings.ToLower(raw[len("data:"):commaIdx])
	payload := raw[commaIdx+1:]

	if !strings.HasSuffix(header, ";base64") {
		return "", "", nil, errors.New("data: URI must be base64-encoded (data:image/<subtype>;base64,...)")
	}
	mt := strings.TrimSuffix(header, ";base64")
	if mt == "image/vnd.microsoft.icon" {
		mt = "image/x-icon"
	}
	switch mt {
	case "image/png", "image/svg+xml", "image/webp", "image/x-icon":
	default:
		return "", "", nil, fmt.Errorf("unsupported data: media type %q (allowed: image/svg+xml, image/png, image/webp, image/x-icon)", mt)
	}
	if pngOnly && mt != "image/png" {
		return "", "", nil, fmt.Errorf("data: URI for %s must use image/png (got %q)", slot, mt)
	}

	maxEncodedLen := ((MaxBrandAssetBytes + 2) / 3) * 4
	if len(payload) == 0 {
		return "", "", nil, errors.New("data: URI payload must not be empty")
	}
	if len(payload) > maxEncodedLen+16 {
		return "", "", nil, fmt.Errorf("decoded data: asset exceeds maximum size of %d bytes (%d KiB)", MaxBrandAssetBytes, MaxBrandAssetBytes>>10)
	}
	decoded, decErr := base64.StdEncoding.DecodeString(payload)
	if decErr != nil {
		return "", "", nil, fmt.Errorf("invalid base64 payload in data: URI: %w", decErr)
	}
	if len(decoded) == 0 {
		return "", "", nil, errors.New("decoded data: asset must not be empty")
	}
	if len(decoded) > MaxBrandAssetBytes {
		return "", "", nil, fmt.Errorf("decoded data: asset size %d bytes exceeds maximum of %d bytes (%d KiB)", len(decoded), MaxBrandAssetBytes, MaxBrandAssetBytes>>10)
	}

	if err := verifyAssetBytes(mt, decoded); err != nil {
		return "", "", nil, err
	}

	sum := sha256.Sum256(decoded)
	sha8 := hex.EncodeToString(sum[:4])
	u := fmt.Sprintf("/brand/%s?v=%s", slot, sha8)
	return u, mt, &ResolvedBrandAsset{
		Slot:      slot,
		MediaType: mt,
		Data:      decoded,
		SHA8:      sha8,
		URL:       u,
	}, nil
}

var pngMagic = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}

func verifyAssetBytes(mediaType string, data []byte) error {
	switch mediaType {
	case "image/png":
		if len(data) < len(pngMagic) || !bytes.Equal(data[:len(pngMagic)], pngMagic) {
			return errors.New("data:image/png payload does not start with a valid PNG signature")
		}
		return nil
	case "image/webp":
		if len(data) < 12 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
			return errors.New("data:image/webp payload does not have a valid RIFF/WEBP header")
		}
		return nil
	case "image/x-icon":
		if len(data) < 4 || data[0] != 0x00 || data[1] != 0x00 || data[2] != 0x01 || data[3] != 0x00 {
			return errors.New("data:image/x-icon payload does not have a valid ICO header")
		}
		return nil
	case "image/svg+xml":
		return verifySafeSVG(data)
	default:
		return fmt.Errorf("unsupported asset media type %q", mediaType)
	}
}

// allowedSVGElements is an explicit allowlist of static SVG 1.1/2 elements
// permitted in admin-supplied data:image/svg+xml branding logos and favicons.
//
// Rationale (allowlist vs denylist): brand logos and favicons are static vector
// graphics served pre-auth from /brand/{slot}. A denylist misses SVG/XML active
// or external-loading constructs such as SMIL animation (<animate>, <set>,
// <animateTransform>, <animateMotion>), event/script bindings (<handler>,
// <listener>), external resource loaders (<image>, <feImage>), hyperlinks (<a>),
// and foreign-namespace elements (XHTML/MathML). An explicit allowlist covers
// standard Figma/Illustrator/Inkscape/SVGO vector exports while failing closed
// on every unknown or active element, complementing the /brand/{slot}
// Content-Security-Policy sandbox header as defense-in-depth.
var allowedSVGElements = map[string]bool{
	// Structure and metadata
	"svg":      true,
	"g":        true,
	"defs":     true,
	"symbol":   true,
	"use":      true,
	"switch":   true,
	"title":    true,
	"desc":     true,
	"metadata": true,
	// Basic shapes and paths
	"path":     true,
	"circle":   true,
	"ellipse":  true,
	"rect":     true,
	"line":     true,
	"polyline": true,
	"polygon":  true,
	// Text
	"text":     true,
	"tspan":    true,
	"textpath": true,
	// Gradients and patterns
	"lineargradient": true,
	"radialgradient": true,
	"stop":           true,
	"pattern":        true,
	// Clipping, masking, and markers
	"clippath": true,
	"mask":     true,
	"marker":   true,
	// Static filter primitives (deliberately excludes feimage)
	"filter":              true,
	"feblend":             true,
	"fecolormatrix":       true,
	"fecomponenttransfer": true,
	"fecomposite":         true,
	"feconvolvematrix":    true,
	"fediffuselighting":   true,
	"fedisplacementmap":   true,
	"fedistantlight":      true,
	"fedropshadow":        true,
	"feflood":             true,
	"fefunca":             true,
	"fefuncb":             true,
	"fefuncg":             true,
	"fefuncr":             true,
	"fegaussianblur":      true,
	"femerge":             true,
	"femergenode":         true,
	"femorphology":        true,
	"feoffset":            true,
	"fepointlight":        true,
	"fespecularlighting":  true,
	"fespotlight":         true,
	"fetile":              true,
	"feturbulence":        true,
	// Inline styling (checked by checkSafeSVGStyleText)
	"style": true,
}

const svgXMLNamespace = "http://www.w3.org/2000/svg"

func checkSafeSVGURLRefs(valNorm, where string) error {
	rest := valNorm
	for {
		idx := strings.Index(rest, "url(")
		if idx < 0 {
			return nil
		}
		after := strings.TrimLeft(rest[idx+4:], "\"'")
		if !strings.HasPrefix(after, "#") {
			return fmt.Errorf("SVG %s url() references must be internal fragment (#id) references", where)
		}
		rest = rest[idx+4:]
	}
}

func checkSafeSVGStyleText(raw string, where string) error {
	if strings.IndexByte(raw, '\\') >= 0 {
		return fmt.Errorf("SVG %s must not contain CSS backslash escapes", where)
	}
	valNorm := stripASCIIWhitespaceAndControl(strings.ToLower(raw))
	if strings.Contains(valNorm, "@import") || strings.Contains(valNorm, "expression(") || strings.Contains(valNorm, "image-set(") {
		return fmt.Errorf("SVG %s must not contain @import, expression(), or image-set()", where)
	}
	if strings.Contains(valNorm, "javascript:") || strings.Contains(valNorm, "vbscript:") || strings.Contains(valNorm, "data:") {
		return fmt.Errorf("SVG %s contains a disallowed URI scheme", where)
	}
	return checkSafeSVGURLRefs(valNorm, where)
}

func verifySafeSVG(data []byte) error {
	if !utf8.Valid(data) {
		return errors.New("SVG payload must be valid UTF-8")
	}
	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.Strict = true

	sawRoot := false
	inStyle := false
	var styleBuf strings.Builder
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("SVG XML syntax error: %w", err)
		}
		switch t := tok.(type) {
		case xml.Directive:
			upper := strings.ToUpper(string(t))
			if strings.Contains(upper, "ENTITY") || strings.Contains(upper, "DOCTYPE") {
				return errors.New("SVG must not contain DOCTYPE or ENTITY declarations")
			}
		case xml.ProcInst:
			if sawRoot || !strings.EqualFold(t.Target, "xml") {
				return fmt.Errorf("SVG processing instruction <?%s?> is not allowed", t.Target)
			}
		case xml.StartElement:
			if inStyle {
				return fmt.Errorf("SVG <style> element must not contain child elements (got <%s>)", t.Name.Local)
			}
			if t.Name.Space != "" && !strings.EqualFold(t.Name.Space, svgXMLNamespace) {
				return fmt.Errorf("SVG element <%s> uses disallowed XML namespace %q", t.Name.Local, t.Name.Space)
			}
			elemName := strings.ToLower(t.Name.Local)
			if !sawRoot {
				if elemName != "svg" {
					return fmt.Errorf("SVG root element must be <svg>, got <%s>", t.Name.Local)
				}
				sawRoot = true
			}
			if !allowedSVGElements[elemName] {
				return fmt.Errorf("SVG element <%s> is not allowed", t.Name.Local)
			}
			if elemName == "style" {
				inStyle = true
				styleBuf.Reset()
			}
			for _, attr := range t.Attr {
				attrLocal := strings.ToLower(attr.Name.Local)
				attrFull := attrLocal
				if attr.Name.Space != "" {
					attrFull = strings.ToLower(attr.Name.Space) + ":" + attrLocal
				}
				if strings.HasPrefix(attrLocal, "on") || strings.HasPrefix(attrFull, "on") {
					return fmt.Errorf("SVG event handler attribute %q is not allowed", attr.Name.Local)
				}
				if strings.IndexByte(attr.Value, '\\') >= 0 {
					return fmt.Errorf("SVG attribute %q must not contain CSS backslash escapes", attr.Name.Local)
				}
				valNorm := stripASCIIWhitespaceAndControl(strings.ToLower(attr.Value))
				if strings.Contains(valNorm, "javascript:") || strings.Contains(valNorm, "vbscript:") || strings.Contains(valNorm, "data:text/html") {
					return fmt.Errorf("SVG attribute %q contains a disallowed URI scheme", attr.Name.Local)
				}
				if attrLocal == "href" {
					trimmed := strings.TrimSpace(attr.Value)
					if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
						return fmt.Errorf("SVG attribute %q must be an internal fragment (#id) reference", attr.Name.Local)
					}
				}
				if attrLocal == "style" {
					if err := checkSafeSVGStyleText(attr.Value, "style attribute"); err != nil {
						return err
					}
				} else if err := checkSafeSVGURLRefs(valNorm, fmt.Sprintf("attribute %q", attr.Name.Local)); err != nil {
					return err
				}
			}
		case xml.CharData:
			if inStyle {
				styleBuf.Write(t)
			}
		case xml.EndElement:
			if inStyle && strings.EqualFold(t.Name.Local, "style") {
				inStyle = false
				if err := checkSafeSVGStyleText(styleBuf.String(), "<style> element"); err != nil {
					return err
				}
				styleBuf.Reset()
			}
		}
	}
	if !sawRoot {
		return errors.New("SVG payload contains no <svg> root element")
	}
	return nil
}

func stripASCIIWhitespaceAndControl(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c <= 0x20 || c == 0x7f {
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}
