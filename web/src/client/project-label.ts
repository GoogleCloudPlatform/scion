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

import { apiFetch } from './api.js';
import { stateManager } from './state.js';
import type { Project } from '../shared/types.js';

export interface ProjectNameSource {
  name?: string | null | undefined;
  slug?: string | null | undefined;
}

export interface ProjectLabel {
  id: string;
  label: string;
  pending: boolean;
}

const inflightResolves = new Map<string, Promise<ProjectLabel>>();
const failedProjectIds = new Set<string>();

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

/**
 * Synchronous first-paint project label: cache hit returns the display name
 * with `pending: false`; cache miss returns `{ id, label: id, pending: true }`.
 */
export function projectLabelNow(id: string): ProjectLabel {
  const key = id.trim();
  if (!key) return { id: '', label: '', pending: false };
  const cached = findCachedProject(key);
  if (cached) {
    return { id: key, label: projectDisplayName(cached, key), pending: false };
  }
  return { id: key, label: key, pending: true };
}

/**
 * Resolves a project's display label cache-first. On a cache miss, issues at
 * most one concurrent `GET /api/v1/projects/:id` per ID, seeding `stateManager`
 * via `seedProjects([project])` on success so subsequent navigations and SSE
 * consumers see it. On 403/404/error, resolves `{ id, label: id, pending: false }`
 * and does not retry for the rest of the session.
 */
export function resolveProjectLabel(id: string): Promise<ProjectLabel> {
  const key = id.trim();
  if (!key) return Promise.resolve({ id: '', label: '', pending: false });

  const cached = findCachedProject(key);
  if (cached) {
    return Promise.resolve({ id: key, label: projectDisplayName(cached, key), pending: false });
  }

  if (failedProjectIds.has(key)) {
    return Promise.resolve({ id: key, label: key, pending: false });
  }

  const existing = inflightResolves.get(key);
  if (existing) return existing;

  const promise = (async (): Promise<ProjectLabel> => {
    try {
      const response = await apiFetch(`/api/v1/projects/${encodeURIComponent(key)}`, {
        authErrors: 'caller',
        suppressAccessDeniedToast: true,
      });
      if (!response.ok) {
        failedProjectIds.add(key);
        return { id: key, label: key, pending: false };
      }
      const data = (await response.json()) as Project | { project?: Project };
      const project =
        data && typeof data === 'object' && 'project' in data && data.project
          ? data.project
          : (data as Project);
      if (project && typeof project.id === 'string' && project.id) {
        stateManager.seedProjects([project]);
        return { id: key, label: projectDisplayName(project, key), pending: false };
      }
      failedProjectIds.add(key);
      return { id: key, label: key, pending: false };
    } catch {
      failedProjectIds.add(key);
      return { id: key, label: key, pending: false };
    } finally {
      inflightResolves.delete(key);
    }
  })();

  inflightResolves.set(key, promise);
  return promise;
}

/** Test-only helper to reset in-flight and negative-cache state between cases. */
export function resetProjectLabelResolverForTests(): void {
  inflightResolves.clear();
  failedProjectIds.clear();
}
