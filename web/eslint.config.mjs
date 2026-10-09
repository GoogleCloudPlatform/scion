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

// ESLint flat config. Later entries override earlier ones for the files they
// match, the same way `overrides` did in the old .eslintrc.cjs.

import js from '@eslint/js';
import { defineConfig, globalIgnores } from 'eslint/config';
import prettierRecommended from 'eslint-plugin-prettier/recommended';
import globals from 'globals';
import tseslint from 'typescript-eslint';

const tsconfigRootDir = import.meta.dirname;

// Rules shared by every linted file.
const sharedRules = {
  '@typescript-eslint/explicit-function-return-type': 'warn',
  // caughtErrors: 'none' keeps the typescript-eslint v7 default; v8
  // changed the default to 'all'.
  '@typescript-eslint/no-unused-vars': ['error', { argsIgnorePattern: '^_', caughtErrors: 'none' }],
  '@typescript-eslint/no-explicit-any': 'warn',
  'no-console': ['warn', { allow: ['warn', 'error', 'info'] }],
  'prettier/prettier': 'error',
};

/** Points the given files at their own TypeScript project. */
const project = (files, path) => ({
  files,
  languageOptions: { parserOptions: { project: path } },
});

export default defineConfig([
  globalIgnores(['**/dist/', '**/node_modules/', '**/public/', '**/*.cjs']),

  {
    // ESLint 9+ reports unused eslint-disable comments by default.
    // Keep the ESLint 8 behaviour.
    linterOptions: { reportUnusedDisableDirectives: 'off' },
  },

  {
    files: ['**/*.ts', '**/*.tsx'],
    extends: [js.configs.recommended, tseslint.configs.recommendedTypeChecked, prettierRecommended],
    languageOptions: {
      ecmaVersion: 'latest',
      sourceType: 'module',
      globals: { ...globals.node, ...globals.es2022 },
      parserOptions: {
        project: './tsconfig.json',
        tsconfigRootDir,
      },
    },
    rules: sharedRules,
  },

  project(['e2e/terminal-workspace/*.ts'], './e2e/terminal-workspace/tsconfig.json'),
  project(['e2e/terminal-entrypoints/*.ts'], './e2e/terminal-entrypoints/tsconfig.json'),
  project(['e2e/terminal-hidden/*.ts'], './e2e/terminal-hidden/tsconfig.json'),
  project(['e2e/chat-mobile/*.ts'], './e2e/chat-mobile/tsconfig.json'),
  project(['e2e/agent-store-count/*.ts'], './e2e/agent-store-count/tsconfig.json'),
  project(['src/client/terminal-*.test.ts'], './src/client/tsconfig.terminal-tests.json'),
  project(
    [
      'src/client/agent-store.test.ts',
      'src/client/agent-store-feed.test.ts',
      'src/client/agent-store-probe.test.ts',
      'src/client/paginate-all.test.ts',
      'src/client/state.test.ts',
      'src/client/__fixtures__/agent-store-harness.ts',
    ],
    './src/client/tsconfig.client-tests.json'
  ),
  // Explicit lists, not globs: only these files are lint-clean against
  // their project. Other files in the same directories are not.
  project(
    [
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
    './src/components/tsconfig.component-tests.json'
  ),
  project(
    [
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
    './e2e/chat-palette/tsconfig.json'
  ),

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
    ignores: [
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
  // benchmark) isn't part of the tsconfig.json TS program, so it is
  // parsed without type information and gets the non-type-checked
  // rules. (The old config used espree here; under ESLint 10 the
  // typescript-eslint rules misread espree's scope data and report
  // every variable as used only as a type.) Mixes Node-side
  // orchestration code with inline functions passed to Playwright's
  // page.evaluate()/addInitScript(), which run in the browser -- both
  // sets of globals are legitimately used in this one file.
  {
    files: ['e2e-perf/**/*.mjs'],
    extends: [
      js.configs.recommended,
      tseslint.configs.recommended,
      tseslint.configs.disableTypeChecked,
      prettierRecommended,
    ],
    languageOptions: {
      ecmaVersion: 'latest',
      sourceType: 'module',
      globals: { ...globals.node, ...globals.browser, ...globals.es2022 },
    },
    rules: {
      ...sharedRules,
      'no-console': 'off',
      // Return-type annotations aren't meaningful in plain (non-TS) JS.
      '@typescript-eslint/explicit-function-return-type': 'off',
    },
  },
]);
