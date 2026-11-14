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
 * Shared Chromium launch configuration for Playwright E2E suites.
 *
 * Precedence for choosing a system Chromium executable:
 *   1. CHROMIUM_EXECUTABLE — explicit repo-wide override (also set by
 *      .github/workflows/web-e2e.yml for full-Chromium CI runs).
 *   2. TERMINAL_OWNER_CHROMIUM — compatibility alias kept for callers of
 *      e2e/terminal-owner; checked before the ambient container path so an
 *      explicit per-command override still wins inside a container.
 *   3. PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH — set automatically in Scion
 *      agent containers (/usr/bin/chromium), where Playwright's downloaded
 *      browser bundle is absent.
 *
 * When none of these variables is set (or non-empty), executablePath is
 * omitted so Playwright uses its own installed browser bundle (for example
 * in ci.yml's --only-shell jobs and on developer workstations).
 */

import type { LaunchOptions } from '@playwright/test';

/** Default Chromium flags required in containerized sandboxes. */
export const CHROMIUM_CONTAINER_ARGS = ['--no-sandbox', '--disable-setuid-sandbox'] as const;

/** Environment variables checked in priority order for a Chromium binary. */
export const CHROMIUM_EXECUTABLE_ENV_VARS = [
  'CHROMIUM_EXECUTABLE',
  'TERMINAL_OWNER_CHROMIUM',
  'PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH',
] as const;

export type EnvMap = Readonly<Record<string, string | undefined>>;

/**
 * Resolves the Chromium executable path from environment variables, or
 * returns undefined so Playwright falls back to its managed browser bundle.
 */
export function chromiumExecutablePath(env: EnvMap = process.env): string | undefined {
  for (const key of CHROMIUM_EXECUTABLE_ENV_VARS) {
    const value = env[key]?.trim();
    if (value) return value;
  }
  return undefined;
}

export interface ChromiumLaunchOptionsInput {
  args?: readonly string[];
  env?: EnvMap;
}

/**
 * Builds Playwright LaunchOptions for Chromium: container-safe sandbox flags
 * plus executablePath only when an environment variable names one.
 */
export function chromiumLaunchOptions(input: ChromiumLaunchOptionsInput = {}): LaunchOptions {
  const { args = CHROMIUM_CONTAINER_ARGS, env = process.env } = input;
  const executablePath = chromiumExecutablePath(env);
  return {
    ...(executablePath !== undefined ? { executablePath } : {}),
    ...(args.length > 0 ? { args: [...args] } : {}),
  };
}
