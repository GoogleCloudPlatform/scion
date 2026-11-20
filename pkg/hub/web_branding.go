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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// brandAssetCSP is the strict sandboxed Content-Security-Policy applied to
// every response served from /brand/{slot}. Even if a user navigates directly
// to an admin-supplied SVG at /brand/logo-light, the browser executes it in an
// opaque origin with script execution disabled.
const brandAssetCSP = "default-src 'none'; img-src data:; style-src 'unsafe-inline'; sandbox"

const defaultManifestDescription = "Orchestrate concurrent LLM code agents."

type webManifestIcon struct {
	Src     string `json:"src"`
	Sizes   string `json:"sizes,omitempty"`
	Type    string `json:"type,omitempty"`
	Purpose string `json:"purpose,omitempty"`
}

type webAppManifest struct {
	Name            string            `json:"name"`
	ShortName       string            `json:"short_name"`
	Description     string            `json:"description,omitempty"`
	ID              string            `json:"id"`
	StartURL        string            `json:"start_url"`
	Scope           string            `json:"scope"`
	Display         string            `json:"display"`
	ThemeColor      string            `json:"theme_color"`
	BackgroundColor string            `json:"background_color"`
	Icons           []webManifestIcon `json:"icons"`
}

// handleBrandAsset serves validated, decoded data: image assets from the
// in-memory ResolvedBranding snapshot at GET /brand/{slot}.
func (ws *WebServer) handleBrandAsset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	slot := strings.TrimPrefix(r.URL.Path, "/brand/")
	if !config.IsValidBrandSlot(slot) {
		http.NotFound(w, r)
		return
	}

	resolved := ws.resolvedBranding()
	asset, ok := resolved.Assets[slot]
	if !ok || len(asset.Data) == 0 {
		http.NotFound(w, r)
		return
	}

	etag := fmt.Sprintf(`"%s"`, asset.SHA8)
	w.Header().Set("Content-Type", asset.MediaType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", brandAssetCSP)
	w.Header().Del("Content-Security-Policy-Report-Only")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("ETag", etag)
	if r.URL.Query().Get("v") == asset.SHA8 {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", cacheControlNoCache)
	}

	if etagMatches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(asset.Data)
}

// handleWebManifest dynamically serves GET /manifest.webmanifest from the
// current ResolvedBranding snapshot. When branding is unconfigured, it emits
// the exact same manifest fields as web/public/manifest.webmanifest.
func (ws *WebServer) handleWebManifest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	manifest := buildWebAppManifest(ws.resolvedBranding())
	raw, err := json.Marshal(manifest)
	if err != nil {
		ws.logger().Error("Failed to marshal web app manifest", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	sum := sha256.Sum256(raw)
	etag := fmt.Sprintf(`"%s"`, hex.EncodeToString(sum[:8]))

	w.Header().Set("Content-Type", "application/manifest+json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", cacheControlNoCache)
	w.Header().Set("ETag", etag)

	if etagMatches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(raw)
}

func buildWebAppManifest(b *config.ResolvedBranding) webAppManifest {
	b = brandOrDefault(b)

	desc := defaultManifestDescription
	if b.Subtitle != "" {
		desc = b.Subtitle
	} else if b.Name != config.DefaultBrandName {
		desc = ""
	}

	themeColor := b.ThemeColor
	if themeColor == "" {
		themeColor = config.DefaultBrandThemeColor
	}

	icons := stockWebManifestIcons()
	if b.Icon192URL != "" || b.Icon512URL != "" || b.IconMaskable512URL != "" || b.FaviconURL != "" {
		icons = make([]webManifestIcon, 0, 4)
		if b.Icon192URL != "" {
			icons = append(icons, webManifestIcon{
				Src:     b.Icon192URL,
				Sizes:   "192x192",
				Type:    manifestIconMediaType(b, config.BrandSlotIcon192, b.Icon192URL, "image/png"),
				Purpose: "any",
			})
		}
		if b.Icon512URL != "" {
			icons = append(icons, webManifestIcon{
				Src:     b.Icon512URL,
				Sizes:   "512x512",
				Type:    manifestIconMediaType(b, config.BrandSlotIcon512, b.Icon512URL, "image/png"),
				Purpose: "any",
			})
		}
		if b.IconMaskable512URL != "" {
			icons = append(icons, webManifestIcon{
				Src:     b.IconMaskable512URL,
				Sizes:   "512x512",
				Type:    manifestIconMediaType(b, config.BrandSlotIconMaskable512, b.IconMaskable512URL, "image/png"),
				Purpose: "maskable",
			})
		}
		if b.FaviconURL != "" {
			if b.FaviconMediaType == "image/svg+xml" {
				icons = append(icons, webManifestIcon{
					Src:     b.FaviconURL,
					Sizes:   "any",
					Type:    "image/svg+xml",
					Purpose: "any",
				})
			} else if len(icons) == 0 {
				icons = append(icons, webManifestIcon{
					Src:     b.FaviconURL,
					Type:    b.FaviconMediaType,
					Purpose: "any",
				})
			}
		}
	}

	return webAppManifest{
		Name:            b.Name,
		ShortName:       b.ShortName,
		Description:     desc,
		ID:              "/",
		StartURL:        "/",
		Scope:           "/",
		Display:         "standalone",
		ThemeColor:      themeColor,
		BackgroundColor: themeColor,
		Icons:           icons,
	}
}

func stockWebManifestIcons() []webManifestIcon {
	return []webManifestIcon{
		{Src: "/icon-192.png", Sizes: "192x192", Type: "image/png", Purpose: "any"},
		{Src: "/icon-512.png", Sizes: "512x512", Type: "image/png", Purpose: "any"},
		{Src: "/icon-maskable-512.png", Sizes: "512x512", Type: "image/png", Purpose: "maskable"},
		{Src: "/favicon.svg", Sizes: "any", Type: "image/svg+xml", Purpose: "any"},
	}
}

func manifestIconMediaType(b *config.ResolvedBranding, slot, rawURL, fallback string) string {
	if asset, ok := b.Assets[slot]; ok && asset.MediaType != "" {
		return asset.MediaType
	}
	lower := strings.ToLower(strings.SplitN(rawURL, "?", 2)[0])
	switch {
	case strings.HasSuffix(lower, ".png"):
		return "image/png"
	case strings.HasSuffix(lower, ".svg"):
		return "image/svg+xml"
	case strings.HasSuffix(lower, ".webp"):
		return "image/webp"
	case strings.HasSuffix(lower, ".ico"):
		return "image/x-icon"
	default:
		return fallback
	}
}

func etagMatches(headerVal, etag string) bool {
	if headerVal == "" {
		return false
	}
	for _, part := range strings.Split(headerVal, ",") {
		cand := strings.TrimSpace(part)
		if cand == "*" || cand == etag || strings.TrimPrefix(cand, "W/") == etag {
			return true
		}
	}
	return false
}
