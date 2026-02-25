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
 * Vite plugin that returns empty JSON arrays for /api/* routes
 * when running without the Go backend. This lets page components
 * render their empty states instead of JSON parse errors.
 */
function mockApiPlugin(): Plugin {
    return {
        name: 'mock-api',
        configureServer(server) {
            server.middlewares.use((req, res, next) => {
                if (req.url === '/auth/me') {
                    res.setHeader('Content-Type', 'application/json');
                    res.statusCode = 200;
                    res.end(JSON.stringify({
                        id: 'dev-user',
                        email: 'dev@scion.local',
                        displayName: 'Dev User',
                    }));
                    return;
                }
                if (req.url === '/auth/providers') {
                    res.setHeader('Content-Type', 'application/json');
                    res.statusCode = 200;
                    res.end(JSON.stringify({ google: true, github: true }));
                    return;
                }
                if (req.url === '/auth/debug') {
                    res.statusCode = 404;
                    res.end();
                    return;
                }
                if (req.url?.startsWith('/api/v1/')) {
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

export default defineConfig({
    root: '.',
    publicDir: 'public',
    // SPA mode: serve index.html for all unmatched routes (history API fallback)
    appType: 'spa',
    plugins: [mockApiPlugin()],
    build: {
        outDir: 'dist/client',
        emptyOutDir: true,
        rollupOptions: {
            input: {
                main: resolve(__dirname, 'index.html'),
            },
            output: {
                // Use consistent naming for SSR compatibility
                entryFileNames: 'assets/[name].js',
                chunkFileNames: 'assets/[name]-[hash].js',
                assetFileNames: 'assets/[name]-[hash].[ext]',
            },
        },
        sourcemap: true,
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
        include: ['lit', '@vaadin/router'],
    },
});
