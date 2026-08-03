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
)

// renderErrorPage returns a self-contained HTML error page with the given
// parameters. It uses inline styles (no external dependencies) with dark mode
// support. Both proxyErrorPageHTML and maintenancePageHTML delegate to this
// shared template to avoid duplicating ~80 lines of HTML/CSS.
//
// Parameters are HTML-escaped internally, so callers pass raw strings.
func renderErrorPage(pageTitle, icon, iconLabel, heading, message string) string {
	return fmt.Sprintf(`<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>%s</title>
    <style>
        :root {
            --bg: #f8fafc;
            --surface: #ffffff;
            --text: #1e293b;
            --text-muted: #64748b;
            --border: #e2e8f0;
            --accent: #3b82f6;
        }

        @media (prefers-color-scheme: dark) {
            :root {
                --bg: #0f172a;
                --surface: #1e293b;
                --text: #f1f5f9;
                --text-muted: #94a3b8;
                --border: #334155;
                --accent: #60a5fa;
            }
        }

        * {
            box-sizing: border-box;
            margin: 0;
            padding: 0;
        }

        html, body {
            height: 100%%;
            font-family: 'Inter', ui-sans-serif, system-ui, -apple-system, sans-serif;
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
        <span class="icon" role="img" aria-label="%s">%s</span>
        <h1>%s</h1>
        <p class="message">%s</p>
        <span class="badge">scion</span>
    </div>
</body>
</html>`,
		html.EscapeString(pageTitle),
		html.EscapeString(iconLabel),
		icon, // icon is a raw HTML entity, not user input
		html.EscapeString(heading),
		html.EscapeString(message),
	)
}
