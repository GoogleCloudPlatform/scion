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
