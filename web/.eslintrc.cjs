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

module.exports = {
    root: true,
    env: {
        node: true,
        es2022: true,
    },
    parser: '@typescript-eslint/parser',
    parserOptions: {
        ecmaVersion: 'latest',
        sourceType: 'module',
        project: './tsconfig.json',
    },
    overrides: [
        {
            files: ['e2e/terminal-workspace/*.ts'],
            parserOptions: { project: './e2e/terminal-workspace/tsconfig.json' },
        },
        {
            files: ['e2e/terminal-entrypoints/*.ts'],
            parserOptions: { project: './e2e/terminal-entrypoints/tsconfig.json' },
        },
        {
            files: ['e2e/terminal-hidden/*.ts'],
            parserOptions: { project: './e2e/terminal-hidden/tsconfig.json' },
        },
        {
            files: ['e2e/chat-mobile/*.ts'],
            parserOptions: { project: './e2e/chat-mobile/tsconfig.json' },
        },
        {
            files: ['e2e/agent-store-count/*.ts'],
            parserOptions: { project: './e2e/agent-store-count/tsconfig.json' },
        },
        {
            files: ['src/client/terminal-*.test.ts'],
            parserOptions: { project: './src/client/tsconfig.terminal-tests.json' },
        },
        {
            files: [
                'src/client/agent-store.test.ts',
                'src/client/agent-store-feed.test.ts',
                'src/client/agent-store-probe.test.ts',
                'src/client/paginate-all.test.ts',
                'src/client/state.test.ts',
                'src/client/__fixtures__/agent-store-harness.ts',
            ],
            parserOptions: { project: './src/client/tsconfig.client-tests.json' },
        },
        // Explicit lists, not globs: only these files are lint-clean against
        // their project. Other files in the same directories are not.
        {
            files: [
                'src/components/shared/palette/quick-palette.test.ts',
                'src/components/shared/palette/quick-palette-groups.test.ts',
                'src/components/shared/palette/quick-palette-ranking-memo.test.ts',
                'src/components/shared/palette/quick-palette-host.test.ts',
                'src/components/shared/palette/graph-palette-controller.test.ts',
                'src/components/shared/palette/palette-typeahead.test.ts',
                'src/utils/platform.test.ts',
                'src/components/pages/graph-palette-hosts.test.ts',
                'src/components/shared/open-modal.test.ts',
                'src/components/shared/agent-tree-view.test.ts',
                'src/components/shared/deep-active-element.test.ts',
                'src/components/terminal/terminal-pane.test.ts',
                'src/components/shared/header.test.ts',
                'src/components/shared/group-member-editor-membership.test.ts',
                'src/components/pages/onboarding.test.ts',
                'src/components/pages/chat-hub-members.test.ts',
                'src/components/shared/chat/chat-thread-peer-project.test.ts',
                'src/components/pages/agent-detail-reincarnate.test.ts',
            ],
            parserOptions: { project: './src/components/tsconfig.component-tests.json' },
        },
        {
            files: [
                'e2e/chat-palette/accessibility.pw.ts',
                'e2e/chat-palette/agent-selection.pw.ts',
                'e2e/chat-palette/agents-livelock.pw.ts',
                'e2e/chat-palette/agents-progressive.pw.ts',
                'e2e/chat-palette/document-preview.pw.ts',
                'e2e/chat-palette/fixture.ts',
                'e2e/chat-palette/focus-and-guards.pw.ts',
                'e2e/chat-palette/group-navigation.pw.ts',
                'e2e/chat-palette/palette-button.pw.ts',
                'e2e/chat-palette/playwright.config.ts',
                'e2e/chat-palette/reopen-race.pw.ts',
                'e2e/chat-palette/shortcut-then-enter.pw.ts',
                'e2e/chat-palette/terminal-and-modal.pw.ts',
                'e2e/chat-palette/terminal-guard-under-shell.pw.ts',
                'e2e/chat-palette/thread-navigation.pw.ts',
                'e2e/chat-palette/touch-keyboard.pw.ts',
                'e2e/chat-palette/typography.pw.ts',
                'e2e/palette-typography.ts',
                'e2e/palette-focus.ts',
            ],
            parserOptions: { project: './e2e/chat-palette/tsconfig.json' },
        },
        // Components navigate through src/client/navigation.ts (#2857, #3118):
        // no importing the client entry module (its load boots the app) and
        // no raw history.pushState/replaceState (skips the base path). The
        // router itself (src/client/main.ts, route-history.ts, navigation.ts)
        // lives outside src/components and is unaffected. Tests are excluded:
        // they vi.mock client/main legitimately.
        {
            files: ['src/components/**/*.ts'],
            // TEMPORARY: chat still needs stateManager/pushRoute/replaceRoute
            // from main.ts; the chat lane migrates these files later.
            excludedFiles: [
                'src/components/**/*.test.ts',
                'src/components/pages/chat*.ts',
                'src/components/shared/chat/**',
            ],
            rules: {
                'no-restricted-imports': [
                    'error',
                    {
                        patterns: [
                            {
                                group: ['**/client/main', '**/client/main.js', '**/client/main.ts'],
                                message:
                                    'Import navigation helpers from client/navigation.js; importing client/main boots the app.',
                            },
                        ],
                    },
                ],
                // Raw history writes in any form: history.pushState,
                // window.history.pushState, history['pushState'],
                // history[`pushState`], const { pushState } = history.
                // Plus dynamic import() of client/main, which
                // no-restricted-imports does not see.
                'no-restricted-syntax': [
                    'error',
                    {
                        selector:
                            'MemberExpression[property.name=/^(push|replace)State$/], MemberExpression[property.value=/^(push|replace)State$/], MemberExpression[property.type="TemplateLiteral"][property.quasis.0.value.cooked=/^(push|replace)State$/], ObjectPattern > Property[key.name=/^(push|replace)State$/], ObjectPattern > Property[key.value=/^(push|replace)State$/]',
                        message:
                            'Use navigateTo(), pushUrl() or replaceSearch() from client/navigation.js instead of raw history.pushState/replaceState.',
                    },
                    {
                        selector:
                            'ImportExpression[source.value=/\\/client\\/main(\\.(js|ts))?$/], ImportExpression[source.type="TemplateLiteral"][source.quasis.length=1][source.quasis.0.value.cooked=/\\/client\\/main(\\.(js|ts))?$/]',
                        message:
                            'Import navigation helpers from client/navigation.js; importing client/main boots the app.',
                    },
                ],
            },
        },
        // e2e-perf/*.mjs (the large-project performance harness's browser
        // benchmark) isn't part of the tsconfig.json TS program the root
        // parserOptions.project requires, so it needs the plain ESLint
        // parser and non-type-checked rules rather than inheriting the
        // root @typescript-eslint/recommended-requiring-type-checking
        // config, which would fail to parse it. Mixes Node-side
        // orchestration code with inline functions passed to Playwright's
        // page.evaluate()/addInitScript(), which run in the browser -- both
        // sets of globals are legitimately used in this one file.
        {
            files: ['e2e-perf/**/*.mjs'],
            env: { node: true, browser: true, es2022: true },
            parser: 'espree',
            parserOptions: { ecmaVersion: 'latest', sourceType: 'module', project: null },
            extends: [
                'eslint:recommended',
                'plugin:@typescript-eslint/disable-type-checked',
                'plugin:prettier/recommended',
            ],
            rules: {
                'no-console': 'off',
                // Return-type annotations aren't meaningful in plain (non-TS) JS.
                '@typescript-eslint/explicit-function-return-type': 'off',
            },
        },
    ],
    plugins: ['@typescript-eslint', 'prettier'],
    extends: [
        'eslint:recommended',
        'plugin:@typescript-eslint/recommended',
        'plugin:@typescript-eslint/recommended-requiring-type-checking',
        'plugin:prettier/recommended',
    ],
    rules: {
        '@typescript-eslint/explicit-function-return-type': 'warn',
        '@typescript-eslint/no-unused-vars': ['error', { argsIgnorePattern: '^_' }],
        '@typescript-eslint/no-explicit-any': 'warn',
        'no-console': ['warn', { allow: ['warn', 'error', 'info'] }],
        'prettier/prettier': 'error',
    },
    ignorePatterns: ['dist', 'node_modules', 'public', '*.cjs'],
};
