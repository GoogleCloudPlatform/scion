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
 * Resolution of a thread's stored default agent.
 *
 * The hub keeps whatever reference set the default: an agent ID (a DM
 * promoted to a thread stores the UUID), a slug, or a display name (the
 * composer menu sends the name). Every place that shows or matches the
 * default resolves it here so they agree.
 */

/** The fields an agent roster entry needs for default-agent matching. */
export interface DefaultAgentCandidate {
  id: string;
  slug?: string;
}

/**
 * Find the agent a stored default names, matching by ID, then slug, then
 * display name. An exact ID or slug match wins over a name match, so a
 * slug equal to another agent's name picks the slug's agent.
 */
export function findDefaultAgent<T extends DefaultAgentCandidate>(
  ref: string,
  agents: readonly T[],
  nameOf: (agent: T) => string | undefined
): T | undefined {
  if (!ref) return undefined;
  return (
    agents.find((a) => a.id === ref) ||
    agents.find((a) => !!a.slug && a.slug === ref) ||
    agents.find((a) => nameOf(a) === ref)
  );
}
