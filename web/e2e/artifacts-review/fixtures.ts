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
 * Fixtures of the review editor suite: a project, artifacts published
 * through the hub API as the dev-auth admin, and the two experiments the
 * suite needs (hub.artifacts and web.artifact_review_editor), switched on
 * for the suite's throwaway hub only and reset afterwards.
 */

import { createHash } from 'node:crypto';

import { expect } from '@playwright/test';
import type { Page } from '@playwright/test';

import { devSignInURL } from '../harness/auth.js';
import { readHubState } from '../harness/hub.js';
import type { HubState } from '../harness/hub.js';
import { solidPng } from '../artifacts-access/render-fixtures.js';
import { stateFile } from './state.js';

export const EXPERIMENTS = ['hub.artifacts', 'web.artifact_review_editor'] as const;

export function hub(): HubState {
  const state = readHubState(stateFile());
  if (!state) throw new Error('no hub state; did global-setup run?');
  return state;
}

export interface ApiResult {
  status: number;
  body: unknown;
}

/** A request as the hub admin (the dev token). */
export async function admin(
  method: string,
  urlPath: string,
  data?: unknown,
  raw?: { body: Buffer; headers: Record<string, string> }
): Promise<ApiResult> {
  const h = hub();
  const headers: Record<string, string> = { Authorization: `Bearer ${h.devToken}` };
  let body: string | Buffer | undefined;
  if (raw) {
    Object.assign(headers, raw.headers);
    body = raw.body;
  } else if (data !== undefined) {
    headers['Content-Type'] = 'application/json';
    body = JSON.stringify(data);
  }
  const res = await fetch(new URL(urlPath, h.baseURL), {
    method,
    headers,
    ...(body !== undefined ? { body } : {}),
  });
  const text = await res.text();
  let parsed: unknown = text;
  try {
    parsed = JSON.parse(text);
  } catch {
    // not JSON
  }
  return { status: res.status, body: parsed };
}

async function ok(r: Promise<ApiResult>, what: string): Promise<ApiResult> {
  const res = await r;
  if (res.status < 200 || res.status >= 300) {
    throw new Error(`${what} answered ${res.status}: ${JSON.stringify(res.body).slice(0, 300)}`);
  }
  return res;
}

/** Switches the suite's experiments on (value true) or resets them (null). */
export async function setExperiments(value: boolean | null): Promise<void> {
  const read = await ok(admin('GET', '/api/v1/admin/experiments'), 'GET experiments');
  const revision = (read.body as { revision: number }).revision;
  const overrides = Object.fromEntries(EXPERIMENTS.map((n) => [n, value]));
  await ok(
    admin('PUT', '/api/v1/admin/experiments', { overrides, expected_revision: revision }),
    'PUT experiments'
  );
}

export async function createProject(name: string): Promise<string> {
  const res = await ok(admin('POST', '/api/v1/projects', { name }), 'create project');
  const body = res.body as { id?: string; project?: { id?: string } };
  const id = body.project?.id ?? body.id;
  if (!id) throw new Error(`create project: no id in ${JSON.stringify(body).slice(0, 200)}`);
  return id;
}

export interface Published {
  id: string;
  seq: number;
}

/**
 * Publishes files as a new artifact in project (no artifactId) or a new
 * version of artifactId, through the hub's create, upload and finalize.
 */
export async function publish(opts: {
  project?: string;
  artifactId?: string;
  title?: string;
  entry: string;
  files: Record<string, Buffer>;
}): Promise<Published> {
  const manifest = Object.entries(opts.files).map(([path, data]) => ({
    path,
    size: data.length,
    sha256: createHash('sha256').update(data).digest('hex'),
  }));
  const req: Record<string, unknown> = { entry: opts.entry, files: manifest };
  if (opts.title) req.title = opts.title;
  if (!opts.artifactId && opts.project) req.scope = opts.project;
  const created = await ok(
    admin(
      'POST',
      opts.artifactId
        ? `/api/v1/artifacts/${encodeURIComponent(opts.artifactId)}/versions`
        : '/api/v1/artifacts',
      req
    ),
    'create version'
  );
  const c = created.body as {
    artifact: { id: string };
    version: { seq: number };
    upload: { required?: string[] };
  };
  for (const path of c.upload.required ?? []) {
    const data = opts.files[path];
    const m = manifest.find((x) => x.path === path)!;
    await ok(
      admin(
        'PUT',
        `/api/v1/artifacts/${encodeURIComponent(c.artifact.id)}/versions/${c.version.seq}/files/${path
          .split('/')
          .map(encodeURIComponent)
          .join('/')}`,
        undefined,
        {
          body: data,
          headers: { 'Content-Type': 'application/octet-stream', 'X-Content-SHA256': m.sha256 },
        }
      ),
      `upload ${path}`
    );
  }
  await ok(
    admin(
      'POST',
      `/api/v1/artifacts/${encodeURIComponent(c.artifact.id)}/versions/${c.version.seq}/finalize`
    ),
    'finalize'
  );
  return { id: c.artifact.id, seq: c.version.seq };
}

/** The entry text of an artifact's current version, as the hub serves it. */
export async function currentText(artifactId: string, entry: string): Promise<string> {
  const res = await ok(
    admin('GET', `/api/v1/artifacts/${encodeURIComponent(artifactId)}/files/${entry}`),
    'read entry'
  );
  return typeof res.body === 'string' ? res.body : JSON.stringify(res.body);
}

/** The kind and seq of an artifact's current version. */
export async function currentVersion(artifactId: string): Promise<{ seq: number; kind: string }> {
  const res = await ok(admin('GET', `/api/v1/artifacts/${encodeURIComponent(artifactId)}`), 'get');
  const v = (res.body as { version: { seq: number; kind: string } }).version;
  return { seq: v.seq, kind: v.kind };
}

/** A small PNG built from bytes in code (nothing binary is committed). */
export const DOT_PNG = solidPng(3, 2, [200, 40, 40]);

/** The artifact page of an artifact. */
export function artifactPage(project: string, id: string): string {
  return `/projects/${encodeURIComponent(project)}/artifacts/${encodeURIComponent(id)}`;
}

/**
 * Signs the browser in as the dev admin from a dev sign-in link, which then
 * opens the artifact page (a dev-auth hub signs no browser in without one).
 */
export async function openArtifact(page: Page, project: string, id: string): Promise<void> {
  const h = hub();
  const signInURL = await devSignInURL(h.baseURL, h.devToken);
  await page.goto(`${signInURL}&next=${encodeURIComponent(artifactPage(project, id))}`, {
    waitUntil: 'domcontentloaded',
  });
  await expect(page.locator('scion-page-artifact-detail')).toBeAttached({ timeout: 30_000 });
  await expect(page.locator('scion-page-artifact-detail scion-detail-header').first()).toBeVisible({
    timeout: 30_000,
  });
}

/** Starts Review on the artifact page and waits for the graphical editor. */
export async function startGraphicalReview(page: Page): Promise<void> {
  await page
    .locator('scion-page-artifact-detail .header-actions sl-button', { hasText: 'Review' })
    .click();
  await expect(page.locator('scion-review-editor .ProseMirror')).toBeVisible({ timeout: 30_000 });
}

/** The editor's current text (what Save review would publish). */
export async function editorText(page: Page): Promise<string> {
  return page
    .locator('scion-review-editor')
    .evaluate((el) => (el as HTMLElement & { flush(): string | null }).flush() ?? '');
}
