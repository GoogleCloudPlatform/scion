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
 * Helpers for resolving a project's display label for back-navigation links
 * (ptone/scion#8731): prefer display name, fall back to slug, then fallback text.
 */

import { stateManager } from './state.js';
import type { Project } from '../shared/types.js';

export interface ProjectNameSource {
  name?: string | null | undefined;
  slug?: string | null | undefined;
}

/**
 * Returns the trimmed project display name, falling back to its trimmed slug,
 * and finally to `fallback` (default `''`).
 */
export function projectDisplayName(project?: ProjectNameSource | null, fallback = ''): string {
  const name = typeof project?.name === 'string' ? project.name.trim() : '';
  if (name) return name;
  const slug = typeof project?.slug === 'string' ? project.slug.trim() : '';
  if (slug) return slug;
  return fallback.trim();
}

/**
 * Looks up a cached project in `stateManager` by ID or slug.
 */
export function findCachedProject(idOrSlug: string): Project | undefined {
  const key = idOrSlug.trim();
  if (!key) return undefined;
  const direct = stateManager.getProject(key);
  if (direct) return direct;
  return stateManager.getProjects().find((p) => p.slug === key);
}

/**
 * Resolves a project's display label from `stateManager` without fetching:
 * cached display name -> cached slug -> `idOrSlug` -> `fallback`.
 */
export function cachedProjectLabel(idOrSlug: string, fallback = ''): string {
  const key = idOrSlug.trim();
  if (!key) return fallback.trim();
  const cached = findCachedProject(key);
  return projectDisplayName(cached, key);
}
