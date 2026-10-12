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

/**
 * Credential guard for the local hub: the hub process inherits every
 * non-SCION_* variable of the runner, so a harness credential in the
 * runner's environment would reach the co-located broker's auth gathering.
 * The journey's running path is the credential-free no-auth fallback
 * (drop-to-shell), so the setup refuses to start while any environment
 * variable named in a shipped harness's auth metadata is set.
 */

import * as fs from 'node:fs';
import * as path from 'node:path';
import { fileURLToPath } from 'node:url';

import yaml from 'js-yaml';

const HARNESSES_DIR = path.resolve(
  path.dirname(fileURLToPath(import.meta.url)),
  '..',
  '..',
  '..',
  'harnesses'
);

interface HarnessAuthYaml {
  auth?: {
    types?: Record<
      string,
      {
        required_env?: Array<{ any_of?: string[] }>;
        required_files?: Array<{ alternative_env_keys?: string[] }>;
      }
    >;
    autodetect?: { env?: Record<string, string> };
  };
}

/**
 * Every environment variable named in the auth metadata of the shipped
 * harnesses (harnesses/<name>/config.yaml): required_env any_of names,
 * alternative_env_keys of required files, and autodetect env keys.
 */
export function harnessCredentialEnvNames(harnessesDir = HARNESSES_DIR): string[] {
  const names = new Set<string>();
  for (const entry of fs.readdirSync(harnessesDir, { withFileTypes: true })) {
    const file = path.join(harnessesDir, entry.name, 'config.yaml');
    if (!entry.isDirectory() || !fs.existsSync(file)) continue;
    const doc = (yaml.load(fs.readFileSync(file, 'utf-8')) ?? {}) as HarnessAuthYaml;
    for (const type of Object.values(doc.auth?.types ?? {})) {
      for (const req of type.required_env ?? []) for (const n of req.any_of ?? []) names.add(n);
      for (const req of type.required_files ?? []) {
        for (const n of req.alternative_env_keys ?? []) names.add(n);
      }
    }
    for (const n of Object.keys(doc.auth?.autodetect?.env ?? {})) names.add(n);
  }
  return [...names].sort();
}

/** Throws, naming the variables (never their values), if any is set. */
export function assertNoHarnessCredentialEnv(env: NodeJS.ProcessEnv = process.env): void {
  const names = harnessCredentialEnvNames();
  if (names.length === 0) {
    throw new Error(`credential guard: no harness auth metadata found under ${HARNESSES_DIR}`);
  }
  const set = names.filter((n) => (env[n] ?? '') !== '');
  if (set.length > 0) {
    throw new Error(
      `credential guard: unset these harness credential variables before running ` +
        `(the journey must run without credentials): ${set.join(', ')}`
    );
  }
  console.info(
    `[setup] Credential guard: none of ${names.length} harness credential variables is set.`
  );
}
