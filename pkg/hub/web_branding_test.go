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
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type staticBrandingProvider struct {
	resolved *config.ResolvedBranding
}

func (p staticBrandingProvider) ResolvedBranding() *config.ResolvedBranding {
	return p.resolved
}

func testMinimalPNGDataURI() string {
	// Valid 8-byte PNG signature + minimal IHDR chunk bytes (accepted by verifyAssetBytes).
	raw := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0x00, 0x00, 0x00, 0x0d, 'I', 'H', 'D', 'R'}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(raw)
}

func testSVGDataURI(svg string) string {
	return "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString([]byte(svg))
}

func TestBrandAssetHandler_SandboxedHeadersCachingAndValidation(t *testing.T) {
	svgBody := `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24"><circle cx="12" cy="12" r="10" fill="#0d9488"/></svg>`
	resolved, errs := config.ResolveBranding(&config.BrandingConfig{
		Name: "Acme Cloud",
		Logo: &config.BrandingLogoConfig{
			Light: testSVGDataURI(svgBody),
		},
		Icons: &config.BrandingIconsConfig{
			Icon192: testMinimalPNGDataURI(),
		},
	})
	require.Empty(t, errs)
	asset, ok := resolved.Assets[config.BrandSlotLogoLight]
	require.True(t, ok)

	ws := newTestWebServer(t, WebServerConfig{})
	ws.SetBrandingSettingsProvider(staticBrandingProvider{resolved: resolved})
	ws.SetMaintenanceState(NewMaintenanceState(true, "Scheduled upgrade"))
	handler := ws.Handler()

	// 1. Versioned GET serves decoded SVG with sandbox CSP, nosniff, and immutable cache, even unauthenticated during maintenance.
	req := httptest.NewRequest(http.MethodGet, asset.URL, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "image/svg+xml", rec.Header().Get("Content-Type"))
	assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
	assert.Equal(t, brandAssetCSP, rec.Header().Get("Content-Security-Policy"))
	assert.Contains(t, rec.Header().Get("Content-Security-Policy"), "sandbox")
	assert.Equal(t, "DENY", rec.Header().Get("X-Frame-Options"))
	assert.Equal(t, "public, max-age=31536000, immutable", rec.Header().Get("Cache-Control"))
	assert.Equal(t, `"`+asset.SHA8+`"`, rec.Header().Get("ETag"))
	assert.Equal(t, svgBody, rec.Body.String())

	// 2. Unversioned GET (or wrong ?v=) uses Cache-Control: no-cache + ETag.
	req = httptest.NewRequest(http.MethodGet, "/brand/logo-light", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, cacheControlNoCache, rec.Header().Get("Cache-Control"))
	assert.Equal(t, `"`+asset.SHA8+`"`, rec.Header().Get("ETag"))

	// 3. Matching If-None-Match returns 304 Not Modified with empty body.
	req = httptest.NewRequest(http.MethodGet, asset.URL, nil)
	req.Header.Set("If-None-Match", `"`+asset.SHA8+`"`)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotModified, rec.Code)
	assert.Empty(t, rec.Body.Bytes())

	// 4. HEAD returns 200 with headers and empty body.
	req = httptest.NewRequest(http.MethodHead, asset.URL, nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "image/svg+xml", rec.Header().Get("Content-Type"))
	assert.Empty(t, rec.Body.Bytes())

	// 5. POST returns 405 Method Not Allowed.
	req = httptest.NewRequest(http.MethodPost, asset.URL, nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	assert.Equal(t, "GET, HEAD", rec.Header().Get("Allow"))

	// 6. Unknown slots, extra path segments, and unconfigured valid slots return 404.
	for _, p := range []string{"/brand/", "/brand/unknown", "/brand/logo-light/extra", "/brand/icon-512"} {
		req = httptest.NewRequest(http.MethodGet, p, nil)
		rec = httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusNotFound, rec.Code, "GET %s", p)
	}
}

func TestDynamicWebManifest_DefaultAndBranded(t *testing.T) {
	// 1. Default unbranded manifest matches web/public/manifest.webmanifest field-for-field.
	ws := newTestWebServer(t, WebServerConfig{})
	req := httptest.NewRequest(http.MethodGet, "/manifest.webmanifest", nil)
	rec := httptest.NewRecorder()
	ws.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "application/manifest+json", rec.Header().Get("Content-Type"))
	assert.Equal(t, cacheControlNoCache, rec.Header().Get("Cache-Control"))
	etag := rec.Header().Get("ETag")
	require.NotEmpty(t, etag)

	var gotDefault, wantDefault webAppManifest
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &gotDefault))
	staticRaw, err := os.ReadFile("../../web/public/manifest.webmanifest")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(staticRaw, &wantDefault))
	assert.Equal(t, wantDefault, gotDefault)

	// Conditional 304 on ETag.
	req = httptest.NewRequest(http.MethodGet, "/manifest.webmanifest", nil)
	req.Header.Set("If-None-Match", etag)
	rec = httptest.NewRecorder()
	ws.Handler().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotModified, rec.Code)

	// 2. Branded manifest without icon_maskable_512 omits maskable purpose so Android doesn't crop.
	resolvedNoMaskable, errs := config.ResolveBranding(&config.BrandingConfig{
		Name:        "Acme Cloud",
		ShortName:   "Acme",
		AccentColor: "#0d9488",
		Icons: &config.BrandingIconsConfig{
			Icon192: testMinimalPNGDataURI(),
			Icon512: "https://cdn.example.com/icon-512.png",
		},
	})
	require.Empty(t, errs)
	ws.SetBrandingSettingsProvider(staticBrandingProvider{resolved: resolvedNoMaskable})
	ws.SetMaintenanceState(NewMaintenanceState(true, "Upgrade"))

	req = httptest.NewRequest(http.MethodGet, "/manifest.webmanifest", nil)
	rec = httptest.NewRecorder()
	ws.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	var gotBranded webAppManifest
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &gotBranded))
	assert.Equal(t, "Acme Cloud", gotBranded.Name)
	assert.Equal(t, "Acme", gotBranded.ShortName)
	assert.Empty(t, gotBranded.Description, "custom name without subtitle must omit stock tagline")
	assert.Equal(t, resolvedNoMaskable.ThemeColor, gotBranded.ThemeColor)
	assert.Equal(t, resolvedNoMaskable.ThemeColor, gotBranded.BackgroundColor)
	require.Len(t, gotBranded.Icons, 2)
	assert.True(t, strings.HasPrefix(gotBranded.Icons[0].Src, "/brand/icon-192?v="))
	assert.Equal(t, "any", gotBranded.Icons[0].Purpose)
	assert.Equal(t, "https://cdn.example.com/icon-512.png", gotBranded.Icons[1].Src)
	assert.Equal(t, "any", gotBranded.Icons[1].Purpose)

	// 3. Branded manifest with subtitle and icon_maskable_512 emits both.
	resolvedFull, errs := config.ResolveBranding(&config.BrandingConfig{
		Name:     "Acme Cloud",
		Subtitle: "Internal Engineering Hub",
		Icons: &config.BrandingIconsConfig{
			Icon192:         "https://cdn.example.com/192.png",
			IconMaskable512: testMinimalPNGDataURI(),
			Favicon:         testSVGDataURI(`<svg xmlns="http://www.w3.org/2000/svg"><circle cx="4" cy="4" r="4"/></svg>`),
		},
	})
	require.Empty(t, errs)
	ws.SetBrandingSettingsProvider(staticBrandingProvider{resolved: resolvedFull})

	req = httptest.NewRequest(http.MethodGet, "/manifest.webmanifest", nil)
	rec = httptest.NewRecorder()
	ws.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	var gotFull webAppManifest
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &gotFull))
	assert.Equal(t, "Internal Engineering Hub", gotFull.Description)
	require.Len(t, gotFull.Icons, 3)
	assert.Equal(t, "maskable", gotFull.Icons[1].Purpose)
	assert.True(t, strings.HasPrefix(gotFull.Icons[1].Src, "/brand/icon-maskable-512?v="))
	assert.Equal(t, "image/svg+xml", gotFull.Icons[2].Type)
}

func TestSPAShell_BrandedHeadIconsThemeColorAndCriticalTokens(t *testing.T) {
	assetsDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(assetsDir, "assets"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(assetsDir, "assets", "main.js"), []byte("console.log('boot');"), 0o644))

	ws := newTestWebServer(t, WebServerConfig{AssetsDir: assetsDir})

	// Unbranded shell omits <style id="scion-branding-tokens"> and emits stock icons.
	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	rec := httptest.NewRecorder()
	ws.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	unbrandedHTML := rec.Body.String()
	assert.NotContains(t, unbrandedHTML, `id="scion-branding-tokens"`)
	assert.Contains(t, unbrandedHTML, `href="/favicon.ico"`)
	assert.Contains(t, unbrandedHTML, `href="/favicon.svg"`)
	assert.Contains(t, unbrandedHTML, `<meta name="theme-color" content="#1e293b" />`)

	// Branded shell replaces stock favicons, updates apple-touch-icon & theme-color, and injects critical tokens.
	resolved, errs := config.ResolveBranding(&config.BrandingConfig{
		Name:        "Acme Cloud",
		AccentColor: "#0d9488",
		Palette: &config.BrandingPaletteConfig{
			Dark: &config.BrandingSemanticMode{
				Surface: "#1c1917",
			},
		},
		Icons: &config.BrandingIconsConfig{
			Favicon: "https://cdn.example.com/favicon.png",
			Icon192: testMinimalPNGDataURI(),
		},
	})
	require.Empty(t, errs)
	ws.SetBrandingSettingsProvider(staticBrandingProvider{resolved: resolved})

	req = httptest.NewRequest(http.MethodGet, "/login", nil)
	rec = httptest.NewRecorder()
	ws.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	brandedHTML := rec.Body.String()

	assert.Contains(t, brandedHTML, `<title>Acme Cloud</title>`)
	assert.Contains(t, brandedHTML, `<link rel="icon" href="https://cdn.example.com/favicon.png" type="image/png" />`)
	assert.NotContains(t, brandedHTML, `href="/favicon.ico"`)
	assert.NotContains(t, brandedHTML, `href="/favicon.svg"`)
	assert.Contains(t, brandedHTML, `<link rel="apple-touch-icon" sizes="180x180" href="`+resolved.Icon192URL+`" />`)
	assert.Contains(t, brandedHTML, `<meta name="theme-color" content="#1c1917" />`)
	assert.Contains(t, brandedHTML, `<style id="scion-branding-tokens">`)
	assert.Contains(t, brandedHTML, `--scion-primary:`)
	assert.NotContains(t, brandedHTML, "ZgotmplZ")
}

func TestHubGoPages_BrandingAndAttribution(t *testing.T) {
	// 1. Default unbranded pages keep stock "Scion" title and "scion" badge.
	defMaint := maintenancePageHTML("Down for upgrade")
	assert.Contains(t, defMaint, "<title>Scion - Maintenance</title>")
	assert.Contains(t, defMaint, `<span class="badge">scion</span>`)
	assert.NotContains(t, defMaint, "Powered by Scion")
	assert.NotContains(t, defMaint, `<img class="brand-logo"`)

	defSusp := suspendedPageHTMLWithBranding(nil, "user@example.com")
	assert.Contains(t, defSusp, "<title>Scion - Account Suspended</title>")
	assert.Contains(t, defSusp, "access to Scion is currently unavailable")
	assert.Contains(t, defSusp, `<span class="badge">scion</span>`)

	defNoAssets := renderNoAssetsPage(nil)
	assert.Contains(t, defNoAssets, "<title>Scion – Web UI Not Available</title>")
	assert.Contains(t, defNoAssets, `<span class="badge">scion</span>`)

	// 2. Branded with attribution="shown" (default) and light+dark logos.
	brandedShown, errs := config.ResolveBranding(&config.BrandingConfig{
		Name:        "Acme <Cloud>",
		AccentColor: "#0d9488",
		Logo: &config.BrandingLogoConfig{
			Light: "https://cdn.example.com/light.svg",
			Dark:  "https://cdn.example.com/dark.svg",
		},
	})
	require.Empty(t, errs)

	maintHTML := maintenancePageHTMLWithBranding(brandedShown, "Upgrading DB")
	assert.Contains(t, maintHTML, "<title>Acme &lt;Cloud&gt; - Maintenance</title>")
	assert.Contains(t, maintHTML, `<source media="(prefers-color-scheme: dark)" srcset="https://cdn.example.com/dark.svg">`)
	assert.Contains(t, maintHTML, `<img class="brand-logo" src="https://cdn.example.com/light.svg" alt="Acme &lt;Cloud&gt;">`)
	assert.Contains(t, maintHTML, "--accent: "+brandedShown.Light.Primary+";")
	assert.Contains(t, maintHTML, "--accent: "+brandedShown.Dark.Primary+";")
	assert.Contains(t, maintHTML, `<span class="badge">Acme &lt;Cloud&gt; · Powered by Scion</span>`)

	suspHTML := suspendedPageHTMLWithBranding(brandedShown, "alice@example.com")
	assert.Contains(t, suspHTML, "<title>Acme &lt;Cloud&gt; - Account Suspended</title>")
	assert.Contains(t, suspHTML, "access to Acme &lt;Cloud&gt; is currently unavailable")
	assert.Contains(t, suspHTML, `<span class="badge">Acme &lt;Cloud&gt; · Powered by Scion</span>`)
	assert.Contains(t, suspHTML, `<img class="brand-logo"`)

	proxyHTML := proxyErrorPageHTMLWithBranding(brandedShown, "Agent Offline", "Offline msg")
	assert.Contains(t, proxyHTML, "<title>Acme &lt;Cloud&gt; - Agent Offline</title>")
	assert.Contains(t, proxyHTML, `<span class="badge">Acme &lt;Cloud&gt; · Powered by Scion</span>`)

	noAssetsHTML := renderNoAssetsPage(brandedShown)
	assert.Contains(t, noAssetsHTML, "<title>Acme &lt;Cloud&gt; – Web UI Not Available</title>")
	assert.Contains(t, noAssetsHTML, `<span class="badge">Acme &lt;Cloud&gt; · Powered by Scion</span>`)
	assert.Contains(t, noAssetsHTML, `<img class="brand-logo"`)

	// Plain-text 401 port proxy no-credentials body uses brand name.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/a1/ports/8080/proxy/app.js", nil)
	writePortProxyNoCredentialsWithBranding(rec, req, brandedShown)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.True(t, strings.HasPrefix(rec.Body.String(), "Acme <Cloud> port proxy:"))

	// 3. Branded with attribution="hidden" omits "Powered by Scion".
	brandedHidden, errs := config.ResolveBranding(&config.BrandingConfig{
		Name:        "Acme Cloud",
		Attribution: config.AttributionHidden,
	})
	require.Empty(t, errs)
	hiddenMaint := maintenancePageHTMLWithBranding(brandedHidden, "Maintenance")
	assert.Contains(t, hiddenMaint, `<span class="badge">Acme Cloud</span>`)
	assert.NotContains(t, hiddenMaint, "Powered by Scion")

	// 4. End-to-end WebServer adminModeWebMiddleware renders branded 503 maintenance page.
	ws := newTestWebServer(t, WebServerConfig{})
	ws.SetBrandingSettingsProvider(staticBrandingProvider{resolved: brandedHidden})
	ws.SetMaintenanceState(NewMaintenanceState(true, "Planned window"))
	ws.mux.HandleFunc("/dashboard", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mwHandler := ws.adminModeWebMiddleware(ws.mux)
	rec = httptest.NewRecorder()
	mwHandler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/dashboard", nil))
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Contains(t, rec.Body.String(), "<title>Acme Cloud - Maintenance</title>")
	assert.Contains(t, rec.Body.String(), `<span class="badge">Acme Cloud</span>`)
}
