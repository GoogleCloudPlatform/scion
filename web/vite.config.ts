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

import { defineConfig, type Plugin } from 'vite';
import { resolve } from 'path';

/**
 * When running behind the Scion hub proxy, the proxy strips the
 * base-path prefix from incoming requests before forwarding them
 * to Vite. But Vite's `base` config makes it expect the prefix on
 * all incoming requests. This middleware re-adds the prefix so
 * Vite can function with its `base` set correctly (which ensures
 * HTML asset URLs and dynamic-import paths all include the prefix
 * for the browser).
 *
 * Flow:
 *   Browser → hub/.../proxy/src/main.ts
 *   Hub proxy strips prefix → Vite receives /src/main.ts
 *   This middleware prepends prefix → /api/v1/.../proxy/src/main.ts
 *   Vite (base = prefix) serves the file normally.
 */
function proxyPrefixRestorePlugin(): Plugin {
    const proxyBase = process.env.PROXY_BASE_PATH; // e.g. /api/v1/agents/.../proxy/
    if (!proxyBase) {
        return { name: 'proxy-prefix-restore-noop' };
    }

    // Strip trailing slash for prefix matching
    const prefix = proxyBase.replace(/\/$/, '');

    return {
        name: 'proxy-prefix-restore',
        configureServer(server) {
            // Must run before Vite's own middleware (use unshift-style
            // by returning a function from configureServer — Vite calls
            // the returned function *after* its internal middleware is
            // installed). Instead, we hook via server.middlewares.use
            // at plugin apply time which runs *before* Vite internals.
            server.middlewares.use((req, _res, next) => {
                // If the URL already has the prefix (shouldn't happen
                // through the hub proxy, but guard against double-prefix)
                if (req.url?.startsWith(prefix)) {
                    next();
                    return;
                }
                // Re-add the prefix so Vite's base-path routing works
                req.url = prefix + (req.url || '/');
                // Also fix req.originalUrl if present
                if (req.originalUrl && !req.originalUrl.startsWith(prefix)) {
                    req.originalUrl = prefix + req.originalUrl;
                }
                next();
            });
        },
    };
}

/**
 * Vite plugin that returns empty JSON arrays for /api/* routes
 * when running without the Go backend. This lets page components
 * render their empty states instead of JSON parse errors.
 */
function mockApiPlugin(): Plugin {
    const proxyBase = process.env.PROXY_BASE_PATH;
    // When behind proxy, mock API paths are prefixed
    const apiPrefix = proxyBase ? proxyBase.replace(/\/$/, '') + '/api/v1/' : '/api/v1/';
    const authMePrefix = proxyBase ? proxyBase.replace(/\/$/, '') + '/auth/me' : '/auth/me';
    const authProvidersPrefix = proxyBase ? proxyBase.replace(/\/$/, '') + '/auth/providers' : '/auth/providers';
    const authDebugPrefix = proxyBase ? proxyBase.replace(/\/$/, '') + '/auth/debug' : '/auth/debug';

    return {
        name: 'mock-api',
        configureServer(server) {
            server.middlewares.use((req, res, next) => {
                if (req.url === authMePrefix) {
                    res.setHeader('Content-Type', 'application/json');
                    res.statusCode = 200;
                    res.end(JSON.stringify({
                        id: 'dev-user',
                        email: 'dev@scion.local',
                        displayName: 'Dev User',
                    }));
                    return;
                }
                if (req.url === authProvidersPrefix) {
                    res.setHeader('Content-Type', 'application/json');
                    res.statusCode = 200;
                    res.end(JSON.stringify({ google: true, github: true }));
                    return;
                }
                if (req.url === authDebugPrefix) {
                    res.statusCode = 404;
                    res.end();
                    return;
                }
                if (req.url?.startsWith(apiPrefix)) {
                    res.setHeader('Content-Type', 'application/json');
                    res.statusCode = 200;
                    res.end('[]');
                    return;
                }
                next();
            });
        },
    };
}

const proxyBase = process.env.PROXY_BASE_PATH;

export default defineConfig({
    root: '.',
    publicDir: 'public',
    // Set base to the proxy path so HTML asset URLs and dynamic imports
    // use the full proxy prefix. The proxyPrefixRestorePlugin re-adds
    // the prefix to incoming requests (which the hub proxy strips).
    //
    // IMPORTANT: PROXY_BASE_PATH must NEVER be set in production/CI builds.
    // It is a dev-only env var for previewing the Vite dev server through
    // the hub's port proxy. In production, base must be '/' so the Go
    // server can serve the built assets at the site root.
    base: proxyBase || '/',
    // SPA mode: serve index.html for all unmatched routes (history API fallback)
    appType: 'spa',
    plugins: [proxyPrefixRestorePlugin(), mockApiPlugin()],
    build: {
        outDir: 'dist/client',
        emptyOutDir: true,
        rollupOptions: {
            input: {
                main: resolve(__dirname, 'src/client/main.ts'),
            },
            output: {
                // Use consistent naming for SSR compatibility
                entryFileNames: 'assets/[name].js',
                chunkFileNames: 'assets/[name]-[hash].js',
                assetFileNames: 'assets/[name]-[hash].[ext]',
                manualChunks(id) {
                    if (id.includes('node_modules/@shoelace-style')) {
                        return 'shoelace';
                    }
                    if (id.includes('node_modules/lit') || id.includes('node_modules/@lit')) {
                        return 'lit';
                    }
                    if (id.includes('node_modules/@codemirror') || id.includes('node_modules/@lezer')) {
                        return 'codemirror';
                    }
                    if (id.includes('node_modules/marked') || id.includes('node_modules/dompurify')) {
                        return 'markdown';
                    }
                    if (id.includes('node_modules/@xterm')) {
                        // Basename without extension for stable chunk names
                        const match = id.match(/@xterm\/([^/]+)/);
                        return match ? `xterm` : 'xterm';
                    }
                },
            },
        },
        sourcemap: true,
        // CodeMirror and xterm are lazy-loaded, so large chunks are acceptable
        chunkSizeWarningLimit: 800,
        // Ensure Lit components are properly bundled
        target: 'esnext',
        minify: 'esbuild',
    },
    server: {
        port: 3000,
        strictPort: true,
    },
    resolve: {
        alias: {
            '@': resolve(__dirname, 'src'),
        },
    },
    // Ensure decorators work correctly
    esbuild: {
        target: 'esnext',
    },
    // Optimize Lit dependency
    optimizeDeps: {
        include: ['lit'],
    },
});
