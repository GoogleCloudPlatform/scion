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
	"fmt"
	"html"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

func brandOrDefault(b *config.ResolvedBranding) *config.ResolvedBranding {
	if b == nil {
		return config.DefaultResolvedBranding()
	}
	return b
}

func isCustomBrandIdentity(b *config.ResolvedBranding) bool {
	return b.Name != config.DefaultBrandName || b.LogoLight != ""
}

func brandPageTitle(b *config.ResolvedBranding, suffix string) string {
	b = brandOrDefault(b)
	return b.Name + " - " + suffix
}

func brandBadgeText(b *config.ResolvedBranding) string {
	b = brandOrDefault(b)
	if !isCustomBrandIdentity(b) {
		return "scion"
	}
	if b.Attribution == config.AttributionHidden {
		return b.Name
	}
	return b.Name + " · Powered by Scion"
}

func brandLogoHTML(b *config.ResolvedBranding) string {
	b = brandOrDefault(b)
	if b.LogoLight == "" {
		return ""
	}
	alt := html.EscapeString(b.Name)
	light := html.EscapeString(b.LogoLight)
	if b.LogoDark != "" && b.LogoDark != b.LogoLight {
		dark := html.EscapeString(b.LogoDark)
		return fmt.Sprintf(`<picture class="brand-logo-wrap"><source media="(prefers-color-scheme: dark)" srcset="%s"><img class="brand-logo" src="%s" alt="%s"></picture>`, dark, light, alt)
	}
	return fmt.Sprintf(`<img class="brand-logo" src="%s" alt="%s">`, light, alt)
}

func brandPageControlRadius(b *config.ResolvedBranding) string {
	b = brandOrDefault(b)
	switch b.Corners {
	case config.BrandCornersSquare:
		return "0"
	case config.BrandCornersRounded:
		return "10px"
	default:
		return "6px"
	}
}

func brandPageNoAssetsRadii(b *config.ResolvedBranding) (codeRadius, hintRadius string) {
	b = brandOrDefault(b)
	switch b.Corners {
	case config.BrandCornersSquare:
		return "0", "0"
	case config.BrandCornersRounded:
		return "6px", "12px"
	default:
		return "4px", "8px"
	}
}

func brandPageFontSansStack(b *config.ResolvedBranding) string {
	b = brandOrDefault(b)
	if b.FontSans != "" && b.FontSans != config.BrandFontSansDefault && b.FontSansStack != "" {
		return b.FontSansStack
	}
	return "'Inter', ui-sans-serif, system-ui, -apple-system, sans-serif"
}

func brandPageColorsCSS(b *config.ResolvedBranding) string {
	b = brandOrDefault(b)
	light := b.Light
	dark := b.Dark
	return fmt.Sprintf(`        :root {
            --bg: %s;
            --surface: %s;
            --text: %s;
            --text-muted: %s;
            --border: %s;
            --accent: %s;
        }

        @media (prefers-color-scheme: dark) {
            :root {
                --bg: %s;
                --surface: %s;
                --text: %s;
                --text-muted: %s;
                --border: %s;
                --accent: %s;
            }
        }`,
		light.Bg, light.Surface, light.Text, light.TextMuted, light.Border, light.Primary,
		dark.Bg, dark.Surface, dark.Text, dark.TextMuted, dark.Border, dark.Primary,
	)
}

// renderErrorPageWithBranding returns a self-contained HTML error page with
// the given parameters and Hub branding. It uses inline styles (no external
// dependencies) with dark mode support. Both proxyErrorPageHTML and
// maintenancePageHTML delegate to this shared template.
//
// Parameters are HTML-escaped internally, so callers pass raw strings.
func renderErrorPageWithBranding(b *config.ResolvedBranding, pageTitle, icon, iconLabel, heading, message string) string {
	b = brandOrDefault(b)
	return fmt.Sprintf(`<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>%s</title>
    <style>
%s

        * {
            box-sizing: border-box;
            margin: 0;
            padding: 0;
        }

        html, body {
            height: 100%%;
            font-family: %s;
            background: var(--bg);
            color: var(--text);
            -webkit-font-smoothing: antialiased;
        }

        body {
            display: flex;
            align-items: center;
            justify-content: center;
        }

        .container {
            text-align: center;
            padding: 2rem;
            max-width: 480px;
        }

        .brand-logo-wrap {
            display: block;
        }

        .brand-logo {
            max-height: 36px;
            max-width: 180px;
            width: auto;
            height: auto;
            object-fit: contain;
            display: block;
            margin: 0 auto 1.25rem;
        }

        .icon {
            font-size: 3rem;
            margin-bottom: 1.5rem;
            display: block;
        }

        h1 {
            font-size: 1.5rem;
            font-weight: 600;
            margin-bottom: 0.75rem;
        }

        .message {
            color: var(--text-muted);
            font-size: 1rem;
            line-height: 1.6;
        }

        .badge {
            display: inline-block;
            margin-top: 1.5rem;
            padding: 0.25rem 0.75rem;
            font-size: 0.75rem;
            font-weight: 500;
            color: var(--accent);
            border: 1px solid var(--border);
            border-radius: 9999px;
            background: var(--surface);
        }
    </style>
</head>
<body>
    <div class="container">
        %s<span class="icon" role="img" aria-label="%s">%s</span>
        <h1>%s</h1>
        <p class="message">%s</p>
        <span class="badge">%s</span>
    </div>
</body>
</html>`,
		html.EscapeString(pageTitle),
		brandPageColorsCSS(b),
		brandPageFontSansStack(b),
		brandLogoHTML(b),
		html.EscapeString(iconLabel),
		icon, // icon is a raw HTML entity, not user input
		html.EscapeString(heading),
		html.EscapeString(message),
		html.EscapeString(brandBadgeText(b)),
	)
}
