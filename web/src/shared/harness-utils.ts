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
 * Canonical harness identifiers, mirroring `harnesses/<name>/config.yaml`.
 *
 * The `harnesses/` directory in the repository root is the source of truth: each
 * subdirectory contains a `config.yaml` whose `harness:` key is the identifier
 * listed here. When a harness is added to or removed from `harnesses/`, update
 * this list to match.
 *
 * This list is only a *fallback*. At runtime the UI prefers the harness configs
 * returned by `GET /api/v1/harness-configs`; these names are used to populate
 * selects when that call returns nothing (empty result, or a failed request).
 *
 * ORDERING: this is also the order the names render in every fallback select.
 * The first five are the harnesses the UI already offered, kept alphabetical;
 * `antigravity` and `hermes` were appended when they were added. Append new
 * harnesses to the end rather than re-sorting, so that the position of the
 * existing options stays stable for people used to the list. Order carries no
 * meaning beyond display — nothing selects by index, and every select binds its
 * value to component state rather than defaulting to the first option.
 */
export const KNOWN_HARNESS_NAMES = [
  'claude',
  'codex',
  'copilot',
  'gemini-cli',
  'opencode',
  'antigravity',
  'hermes',
  'grok-build',
] as const;

/** A canonical harness identifier. */
export type KnownHarnessName = (typeof KNOWN_HARNESS_NAMES)[number];

/**
 * Human-readable labels for the canonical harnesses.
 *
 * DISPLAY ONLY. `KNOWN_HARNESS_NAMES` above is the source of truth for which
 * harnesses exist; this map only decides how each one is spelled in the UI.
 * Never branch on these strings.
 *
 * `harnesses/<name>/config.yaml` has no display-name field, so these labels
 * cannot be derived and are maintained here by hand. The values below are
 * transcribed from the markup they replaced, so there is no visual change.
 * `antigravity` and `hermes` had no prior rendering (they were missing from
 * every fallback list), so their labels are new.
 */
const HARNESS_DISPLAY_NAMES: Record<KnownHarnessName, string> = {
  claude: 'Claude',
  codex: 'Codex',
  copilot: 'Copilot',
  'gemini-cli': 'Gemini CLI',
  opencode: 'OpenCode',
  antigravity: 'Antigravity',
  hermes: 'Hermes',
  'grok-build': 'Grok Build',
};

/**
 * Returns a human-readable label for a harness identifier, falling back to the
 * identifier itself for names that are not in the canonical list.
 */
export function harnessDisplayName(name: string): string {
  return HARNESS_DISPLAY_NAMES[name as KnownHarnessName] ?? name;
}
