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
 * Shared client helpers for the Project GitHub read model
 * (`GET /api/v1/projects/{id}/github`, M1.5).
 */

import type { ProjectGitHubInfo } from '../shared/types.js';
import { apiFetch } from './api.js';

/**
 * Dispatched on `window` whenever a project's GitHub visibility is resolved
 * (true = remote is on App host and App is configured; false = hidden).
 */
export const PROJECT_GITHUB_VISIBILITY_EVENT = 'scion:project-github-visibility';

export interface ProjectGitHubVisibilityDetail {
  projectId: string;
  visible: boolean;
}

/**
 * In-flight single-flight map keyed by projectId so concurrent shell/page
 * readers share one request. Cleared on settlement.
 */
const inflightRequests = new Map<string, Promise<ProjectGitHubInfo | null>>();

/** Last resolved visibility per projectId for cross-component sync (e.g. scion-nav). */
const resolvedVisibility = new Map<string, boolean>();

/**
 * Whether the project GitHub page, nav entry, header button, and Settings link
 * should be shown: requires both a configured GitHub App and a project remote
 * on the App's host.
 */
export function isProjectGitHubVisible(info: ProjectGitHubInfo | null | undefined): boolean {
  return Boolean(info && info.app?.configured && info.remote?.on_app_host);
}

/**
 * Whether the project currently has a connected GitHub App installation.
 */
export function isProjectGitHubConnected(info: ProjectGitHubInfo | null | undefined): boolean {
  return Boolean(info && info.association?.installation_id != null);
}

/**
 * Record and broadcast a project's resolved GitHub visibility so persistent
 * shell components (such as `scion-nav`) stay in sync without issuing
 * redundant requests on hub-managed projects that have no git remote.
 */
export function setProjectGitHubVisibility(projectId: string, visible: boolean): void {
  if (!projectId) return;
  const prev = resolvedVisibility.get(projectId);
  resolvedVisibility.set(projectId, visible);
  if (prev !== visible && typeof window !== 'undefined') {
    window.dispatchEvent(
      new CustomEvent<ProjectGitHubVisibilityDetail>(PROJECT_GITHUB_VISIBILITY_EVENT, {
        detail: { projectId, visible },
      })
    );
  }
}

/** Read the last resolved visibility for a projectId, or undefined if unknown. */
export function getProjectGitHubVisibility(projectId: string): boolean | undefined {
  return resolvedVisibility.get(projectId);
}

/**
 * Fetch `GET /api/v1/projects/{id}/github`, deduplicating concurrent in-flight
 * calls for the same projectId. Returns null on non-OK responses or network
 * errors so callers can fail closed (hide entry points) without throwing.
 */
export function fetchProjectGitHub(projectId: string): Promise<ProjectGitHubInfo | null> {
  if (!projectId) return Promise.resolve(null);

  const existing = inflightRequests.get(projectId);
  if (existing) return existing;

  const req = (async (): Promise<ProjectGitHubInfo | null> => {
    try {
      const res = await apiFetch(`/api/v1/projects/${encodeURIComponent(projectId)}/github`, {
        suppressAccessDeniedToast: true,
      });
      if (!res.ok) {
        setProjectGitHubVisibility(projectId, false);
        return null;
      }
      const info = (await res.json()) as ProjectGitHubInfo;
      setProjectGitHubVisibility(projectId, isProjectGitHubVisible(info));
      return info;
    } catch {
      setProjectGitHubVisibility(projectId, false);
      return null;
    } finally {
      inflightRequests.delete(projectId);
    }
  })();

  inflightRequests.set(projectId, req);
  return req;
}

/**
 * Clear in-flight tracking and resolved visibility state (for unit tests).
 */
export function resetProjectGitHubRequestsForTests(): void {
  inflightRequests.clear();
  resolvedVisibility.clear();
}
