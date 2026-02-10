/**
 * Copyright 2026 Google LLC
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

/**
 * HTML shell templates for SSR
 *
 * Provides the HTML wrapper for server-rendered Lit components
 */

import type { PageData } from '../../shared/types.js';

export interface HtmlTemplateOptions {
  /** Page title */
  title: string;
  /** Rendered component HTML content */
  content: string;
  /** Initial page data for hydration */
  initialData: PageData;
  /** JavaScript files to load */
  scripts: string[];
  /** CSS files to load */
  styles: string[];
}

/**
 * Shoelace CDN version to use
 */
const SHOELACE_VERSION = '2.19.0';

/**
 * Generates the full HTML document with SSR content
 */
export function getHtmlTemplate(opts: HtmlTemplateOptions): string {
  const escapedData = JSON.stringify(opts.initialData)
    .replace(/</g, '\\u003c')
    .replace(/>/g, '\\u003e')
    .replace(/&/g, '\\u0026');

  return `<!DOCTYPE html>
<html lang="en" data-theme="light">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>${escapeHtml(opts.title)} - Scion</title>

    <!-- Preconnect to CDNs for faster loading -->
    <link rel="preconnect" href="https://cdn.jsdelivr.net">
    <link rel="preconnect" href="https://fonts.googleapis.com">
    <link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>

    <!-- Fonts -->
    <link href="https://fonts.googleapis.com/css2?family=Inter:wght@400;500;600;700&display=swap" rel="stylesheet">
    <link href="https://fonts.googleapis.com/css2?family=JetBrains+Mono:wght@400;500&display=swap" rel="stylesheet">

    <!-- Shoelace Component Library -->
    <link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/@shoelace-style/shoelace@${SHOELACE_VERSION}/cdn/themes/light.css">
    <link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/@shoelace-style/shoelace@${SHOELACE_VERSION}/cdn/themes/dark.css" media="(prefers-color-scheme: dark)">
    <script type="module" src="https://cdn.jsdelivr.net/npm/@shoelace-style/shoelace@${SHOELACE_VERSION}/cdn/shoelace-autoloader.js"></script>

    <!-- App styles -->
    ${opts.styles.map((s) => `<link rel="stylesheet" href="${s}">`).join('\n    ')}

    <!-- Initial state for hydration -->
    <script id="__SCION_DATA__" type="application/json">${escapedData}</script>

    <style>
        /* Critical CSS - Core layout to prevent FOUC */

        /* Color Palette - Light Mode (inlined for fast first paint) */
        :root {
            /* Primary */
            --scion-primary-50: #eff6ff;
            --scion-primary-500: #3b82f6;
            --scion-primary-600: #2563eb;
            --scion-primary-700: #1d4ed8;

            /* Neutral */
            --scion-neutral-50: #f8fafc;
            --scion-neutral-100: #f1f5f9;
            --scion-neutral-200: #e2e8f0;
            --scion-neutral-500: #64748b;
            --scion-neutral-600: #475569;
            --scion-neutral-700: #334155;
            --scion-neutral-800: #1e293b;
            --scion-neutral-900: #0f172a;

            /* Semantic */
            --scion-primary: var(--scion-primary-500);
            --scion-primary-hover: var(--scion-primary-600);
            --scion-bg: var(--scion-neutral-50);
            --scion-bg-subtle: var(--scion-neutral-100);
            --scion-surface: #ffffff;
            --scion-text: var(--scion-neutral-800);
            --scion-text-muted: var(--scion-neutral-500);
            --scion-border: var(--scion-neutral-200);

            /* Layout */
            --scion-sidebar-width: 260px;
            --scion-header-height: 60px;

            /* Typography */
            --scion-font-sans: 'Inter', ui-sans-serif, system-ui, -apple-system, sans-serif;
            --scion-font-mono: 'JetBrains Mono', ui-monospace, monospace;
        }

        /* Dark mode support */
        @media (prefers-color-scheme: dark) {
            :root:not([data-theme="light"]) {
                --scion-primary: #60a5fa;
                --scion-primary-hover: #93c5fd;
                --scion-bg: var(--scion-neutral-900);
                --scion-bg-subtle: var(--scion-neutral-800);
                --scion-surface: var(--scion-neutral-800);
                --scion-text: #f1f5f9;
                --scion-text-muted: #94a3b8;
                --scion-border: var(--scion-neutral-700);
            }
        }

        [data-theme="dark"] {
            --scion-primary: #60a5fa;
            --scion-primary-hover: #93c5fd;
            --scion-bg: var(--scion-neutral-900);
            --scion-bg-subtle: var(--scion-neutral-800);
            --scion-surface: var(--scion-neutral-800);
            --scion-text: #f1f5f9;
            --scion-text-muted: #94a3b8;
            --scion-border: var(--scion-neutral-700);
        }

        * {
            box-sizing: border-box;
            margin: 0;
            padding: 0;
        }

        html, body {
            height: 100%;
            font-family: var(--scion-font-sans);
            background: var(--scion-bg);
            color: var(--scion-text);
            -webkit-font-smoothing: antialiased;
            -moz-osx-font-smoothing: grayscale;
        }

        #app {
            min-height: 100%;
        }

        /* Prevent FOUC for custom elements */
        scion-app:not(:defined),
        scion-nav:not(:defined),
        scion-header:not(:defined),
        scion-breadcrumb:not(:defined),
        scion-status-badge:not(:defined),
        scion-page-home:not(:defined),
        scion-page-groves:not(:defined),
        scion-page-agents:not(:defined),
        scion-page-404:not(:defined) {
            display: block;
            opacity: 0.5;
        }

        /* Shoelace component loading state */
        sl-button:not(:defined),
        sl-icon:not(:defined),
        sl-badge:not(:defined),
        sl-drawer:not(:defined),
        sl-dropdown:not(:defined),
        sl-menu:not(:defined),
        sl-menu-item:not(:defined),
        sl-breadcrumb:not(:defined),
        sl-breadcrumb-item:not(:defined),
        sl-tooltip:not(:defined),
        sl-avatar:not(:defined) {
            visibility: hidden;
        }
    </style>

    <!-- Theme detection script (runs before paint) -->
    <script>
        (function() {
            const saved = localStorage.getItem('scion-theme');
            if (saved === 'dark' || (!saved && window.matchMedia('(prefers-color-scheme: dark)').matches)) {
                document.documentElement.setAttribute('data-theme', 'dark');
                document.documentElement.classList.add('sl-theme-dark');
            }
        })();
    </script>
</head>
<body>
    <div id="app">${opts.content}</div>

    <!-- Hydration scripts -->
    ${opts.scripts.map((s) => `<script type="module" src="${s}"></script>`).join('\n    ')}
</body>
</html>`;
}

/**
 * Escape HTML entities to prevent XSS
 */
function escapeHtml(str: string): string {
  return str
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#039;');
}

/**
 * Get page title based on URL path
 */
export function getPageTitle(path: string): string {
  const titles: Record<string, string> = {
    '/': 'Dashboard',
    '/groves': 'Groves',
    '/agents': 'Agents',
    '/settings': 'Settings',
    '/unauthorized': 'Access Denied',
  };

  // Check for exact match
  if (titles[path]) {
    return titles[path];
  }

  // Check for pattern matches
  if (path.startsWith('/groves/')) {
    return 'Grove Details';
  }
  if (path.startsWith('/agents/')) {
    return 'Agent Details';
  }

  return 'Page Not Found';
}
