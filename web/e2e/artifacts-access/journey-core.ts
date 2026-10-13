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
 * Shared machinery of an artifact journey: identities in their own browser
 * contexts, API calls in an identity's session, the admin experiment toggle,
 * graded rows with soft assertions, findings, fixture projects and the
 * cleanup primitives.
 *
 * It follows the access journey (artifacts-access.pw.ts), which keeps its
 * own copy for now; consolidating that file onto this module is a follow-up
 * once it is on main. Used by the render journey (artifacts-render.pw.ts).
 */

import * as fs from 'node:fs';
import * as path from 'node:path';

import { test, type BrowserContext, type Page } from '@playwright/test';

import { assertTestIdentity, redeemTestIdentity } from '../harness/auth.js';
import { readHubState, runScionCLI } from '../harness/hub.js';
import {
  annotationsOf,
  firstLine,
  recordRequests,
  redactPath,
  type RequestRecord,
} from './journey-lib.js';
import { isRemoteTarget, stateFile, type JourneyTarget } from './state.js';

export const EXPERIMENT = 'hub.artifacts';

/** Reason a seeded row keeps when nothing reached it. */
export const NOT_REACHED = 'not reached';

/** One identity of a journey. */
export interface Identity<K extends string = string> {
  key: K;
  id: string;
  email: string;
  displayName: string;
  context: BrowserContext;
  page?: Page;
  requests: RequestRecord[];
}

/** One API call as recorded in the evidence. No bodies. */
export interface ApiRecord {
  as: string;
  method: string;
  path: string;
  status: number;
  code: string | null;
}

export type RowStatus = 'passed' | 'failed' | 'not run' | 'observed';

/** One row in the evidence. */
export interface RowRecord {
  id: string;
  role: string;
  title: string;
  graded: boolean;
  status: RowStatus;
  reason?: string;
  ui: Record<string, unknown>;
  api: ApiRecord[];
  errors: string[];
  /** A leg of the row could not run: a row with a skipped leg cannot pass. */
  legNotRun?: string;
}

/** A row of the spec: who acts, what it checks, whether it is graded. */
export interface RowSpec {
  role: string;
  title: string;
  graded?: boolean;
  /** Where the row runs, for the oracle row map. */
  where?: string;
  /**
   * Generic text that replaces every error message of this row in the
   * evidence errors list (the restricted structured fields keep the detail).
   */
  genericError?: string;
}

export interface ExperimentState {
  revision: number;
  enabled: boolean | null;
  override: boolean | null;
}

/** A response body as JSON, or null when it is empty or not JSON. */
export function parseJson(text: string): unknown {
  if (!text) return null;
  try {
    return JSON.parse(text) as unknown;
  } catch {
    return null;
  }
}

/** The error code of a hub error body. */
export function errorCode(body: unknown): string | null {
  const b = (body ?? {}) as { error?: { code?: unknown } | string; code?: unknown };
  if (b.error && typeof b.error === 'object' && typeof b.error.code === 'string') {
    return b.error.code;
  }
  return typeof b.code === 'string' ? b.code : null;
}

/** Visible and present: a hidden check must follow a positive anchor. */
export async function isShown(locator: ReturnType<Page['locator']>): Promise<boolean> {
  return (await locator.count()) > 0 && (await locator.first().isVisible());
}

/**
 * The remote target from the environment: E2E_TARGET_URL, and per identity
 * key `${prefix}_<KEY>_CODE_FILE` and `${prefix}_<KEY>_EMAIL`, with the
 * optional experiment toggle (E2E_ARTIFACTS_TOGGLE_EXPERIMENT=1 and
 * E2E_ADMIN_TOKEN_FILE).
 */
export function remoteTarget<K extends string>(
  keys: readonly K[],
  prefix: string,
  env: NodeJS.ProcessEnv = process.env
): JourneyTarget & { remoteIdentities: Record<K, { codeFile: string; email: string }> } {
  const missing: string[] = [];
  const identities = {} as Record<K, { codeFile: string; email: string }>;
  for (const key of keys) {
    const upper = key.toUpperCase();
    const codeFile = env[`${prefix}_${upper}_CODE_FILE`] ?? '';
    const email = env[`${prefix}_${upper}_EMAIL`] ?? '';
    if (!codeFile) missing.push(`${prefix}_${upper}_CODE_FILE`);
    if (!email) missing.push(`${prefix}_${upper}_EMAIL`);
    identities[key] = { codeFile, email };
  }
  if (missing.length > 0) {
    throw new Error(`E2E_TARGET_URL is set; also set ${missing.join(', ')}`);
  }
  let adminToken: string | undefined;
  if (env.E2E_ARTIFACTS_TOGGLE_EXPERIMENT === '1') {
    const file = env.E2E_ADMIN_TOKEN_FILE ?? '';
    if (!file) throw new Error('E2E_ARTIFACTS_TOGGLE_EXPERIMENT=1 needs E2E_ADMIN_TOKEN_FILE');
    adminToken = fs.readFileSync(file, 'utf-8').trim();
  }
  return {
    mode: 'remote',
    baseURL: new URL(env.E2E_TARGET_URL ?? '').origin,
    remoteIdentities: identities,
    ...(adminToken ? { adminToken } : {}),
  };
}

/** Options of a journey. */
export interface JourneyOptions<K extends string> {
  keys: readonly K[];
  rows: Record<string, RowSpec>;
  /** Env prefix of the remote identities, e.g. E2E_R2. */
  remotePrefix: string;
  /** Purpose label of issued identities, e.g. "e2e artifact render". */
  purpose: string;
}

/**
 * One journey's shared state and helpers. Each spec file creates one; all
 * state lives on the returned object.
 */
export function createJourney<K extends string>(opts: JourneyOptions<K>) {
  const state = {
    target: undefined as
      | (JourneyTarget & { remoteIdentities?: Record<K, { codeFile: string; email: string }> })
      | undefined,
    tmpDir: undefined as string | undefined,
    ids: {} as Partial<Record<K, Identity<K>>>,
    projectId: undefined as string | undefined,
    bindings: [] as Array<{ projectId: string; key: K; id: string }>,
    stopped: undefined as string | undefined,
    setupError: undefined as string | undefined,
    testError: undefined as string | undefined,
    rows: {} as Record<string, RowRecord>,
    hardFailures: [] as string[],
    findings: [] as Array<{ id: string; text: string }>,
    evidence: {} as Record<string, unknown>,
  };

  function target(): NonNullable<typeof state.target> {
    if (!state.target) throw new Error('journey target not set up');
    return state.target;
  }

  function resolveTarget(): NonNullable<typeof state.target> {
    if (isRemoteTarget()) return remoteTarget(opts.keys, opts.remotePrefix);
    const hub = readHubState(stateFile());
    if (!hub?.homeDir) throw new Error(`No local hub state at ${stateFile()}`);
    return { mode: 'local', baseURL: hub.baseURL, hub };
  }

  function who(key: K): Identity<K> {
    const identity = state.ids[key];
    if (!identity) throw new Error(`identity ${key} not set up`);
    return identity;
  }

  /** An API call in an identity's session (its browser context's cookies). */
  async function api(
    key: K,
    method: 'GET' | 'POST' | 'DELETE' | 'PUT',
    urlPath: string,
    data?: unknown,
    rec?: RowRecord
  ): Promise<{
    status: number;
    code: string | null;
    body: unknown;
    headers: Record<string, string>;
  }> {
    const res = await who(key).context.request.fetch(urlPath, {
      method,
      ...(data !== undefined ? { data } : {}),
      maxRedirects: 0,
    });
    const text = await res.text();
    const body = parseJson(text);
    const headers = res.headers();
    await res.dispose();
    const out = { status: res.status(), code: errorCode(body), body, headers };
    rec?.api.push({
      as: key,
      method,
      path: redactPath(new URL(urlPath, target().baseURL).pathname),
      status: out.status,
      code: out.code,
    });
    return out;
  }

  /** The admin bearer token: the local hub's dev token, or the remote admin token. */
  function adminToken(): string | undefined {
    const t = target();
    return t.hub?.devToken ?? t.adminToken;
  }

  /** A request as the hub admin (fixtures only: the experiment toggle). */
  async function adminRequest(
    method: string,
    urlPath: string,
    data?: unknown
  ): Promise<{ status: number; body: unknown }> {
    const token = adminToken();
    if (!token) throw new Error('no admin token for this target');
    const res = await fetch(new URL(urlPath, target().baseURL), {
      method,
      headers: {
        Authorization: `Bearer ${token}`,
        ...(data !== undefined ? { 'Content-Type': 'application/json' } : {}),
      },
      ...(data !== undefined ? { body: JSON.stringify(data) } : {}),
    });
    return { status: res.status, body: parseJson(await res.text()) };
  }

  async function readExperiment(): Promise<ExperimentState> {
    const res = await adminRequest('GET', '/api/v1/admin/experiments');
    if (res.status !== 200) throw new Error(`GET /api/v1/admin/experiments answered ${res.status}`);
    const body = res.body as {
      revision: number;
      experiments: Array<{ name: string; enabled: boolean; override: boolean | null }>;
    };
    const entry = body.experiments.find((e) => e.name === EXPERIMENT);
    return {
      revision: body.revision,
      enabled: entry ? entry.enabled : null,
      override: entry ? entry.override : null,
    };
  }

  async function setExperiment(value: boolean | null): Promise<ExperimentState> {
    const before = await readExperiment();
    const res = await adminRequest('PUT', '/api/v1/admin/experiments', {
      overrides: { [EXPERIMENT]: value },
      expected_revision: before.revision,
    });
    if (res.status !== 200) throw new Error(`PUT /api/v1/admin/experiments answered ${res.status}`);
    return readExperiment();
  }

  function rowRecord(id: string): RowRecord {
    const spec = opts.rows[id];
    if (!spec) throw new Error(`row ${id} is not in the row table`);
    state.rows[id] ??= {
      id,
      role: spec.role,
      title: spec.title,
      graded: spec.graded ?? true,
      status: 'not run',
      reason: NOT_REACHED,
      ui: {},
      api: [],
      errors: [],
    };
    return state.rows[id];
  }

  /** Seeds every row as "not run" (not reached), so none can go missing. */
  function seedRows(): void {
    for (const id of Object.keys(opts.rows)) rowRecord(id);
  }

  /** Marks rows "not run" with a reason (a failed precondition or a skip). */
  function notRun(ids: string[], reason: string): void {
    for (const id of ids) {
      const r = rowRecord(id);
      r.status = 'not run';
      r.reason = reason;
    }
  }

  /**
   * Runs one row as a test step. Its assertions are soft: a failed row is
   * recorded and the journey goes on. A throw fails the row and is
   * collected for the final check. A row whose precondition (a fact, such
   * as "the bundle was published") does not hold, or that runs after a
   * stop, is "not run" with the reason. An ungraded row that ran is
   * "observed", never "passed".
   */
  async function row(
    id: string,
    fn: (r: RowRecord) => Promise<void> | void,
    rowOpts: { pre?: () => string | undefined } = {}
  ): Promise<boolean> {
    const r = rowRecord(id);
    const spec = opts.rows[id];
    if (state.stopped) {
      r.status = 'not run';
      r.reason = state.stopped;
      return false;
    }
    const unmet = rowOpts.pre?.();
    if (unmet) {
      r.status = 'not run';
      r.reason = unmet;
      return false;
    }
    delete r.reason;
    const info = test.info();
    const before = info.errors.length;
    let threw: unknown = null;
    try {
      await test.step(`${id}: ${r.title}`, () => fn(r));
    } catch (err) {
      threw = err;
    }
    const generic = spec?.genericError;
    const softErrors = info.errors
      .slice(before)
      .map((e) => (generic ? generic : firstLine(e.message ?? '')));
    r.errors.push(...softErrors);
    if (threw) {
      const msg = generic ?? firstLine(threw);
      r.errors.push(msg);
      if (r.graded) state.hardFailures.push(`${id}: ${msg}`);
    }
    if (threw || softErrors.length > 0) {
      r.status = 'failed';
    } else if (r.legNotRun) {
      r.status = 'not run';
      r.reason = r.legNotRun;
    } else {
      r.status = r.graded ? 'passed' : 'observed';
    }
    return r.status === 'passed' || r.status === 'observed';
  }

  /** Records a FINDING: in the evidence and as a Playwright annotation. */
  function finding(id: string, text: string, annotation?: string): void {
    state.findings.push({ id, text });
    test.info().annotations.push({ type: 'FINDING', description: `${id}: ${annotation ?? text}` });
  }

  /** The identity's page, opened once. */
  async function pageOf(key: K): Promise<Page> {
    const identity = who(key);
    if (!identity.page || identity.page.isClosed()) {
      identity.page = await identity.context.newPage();
    }
    return identity.page;
  }

  /** Issues the identities (local) or reads them from the environment, and redeems each. */
  async function setUpIdentities(
    browserNewContext: () => Promise<BrowserContext>
  ): Promise<void> {
    const t = target();
    for (const key of opts.keys) {
      let codeFile: string;
      let email: string;
      let displayName = '';
      if (t.mode === 'local') {
        const hub = t.hub!;
        const tokenFile = path.join(state.tmpDir!, `${key}.token`);
        const issued = JSON.parse(
          runScionCLI(hub, [
            'hub',
            'test-identity',
            'issue',
            '--role',
            'member',
            '--purpose',
            `${opts.purpose} (${key})`,
            '--out',
            tokenFile,
            '--json',
          ])
        ) as { identity: { id: string; email: string; displayName?: string } };
        // The browser path needs no API token.
        fs.rmSync(tokenFile, { force: true });
        email = issued.identity.email;
        displayName = issued.identity.displayName ?? '';
        codeFile = path.join(state.tmpDir!, `${key}.code`);
        runScionCLI(hub, [
          'hub',
          'test-identity',
          'web-code',
          issued.identity.id,
          '--out',
          codeFile,
        ]);
      } else {
        ({ codeFile, email } = t.remoteIdentities![key]);
      }
      const context = await browserNewContext();
      const requests = recordRequests(context);
      await redeemTestIdentity(context, { codeFile });
      await assertTestIdentity(context.request, { email, role: 'member' });
      const me = await context.request.get('/api/v1/auth/me');
      const meBody = (await me.json()) as { id?: string; displayName?: string };
      await me.dispose();
      if (!meBody.id) throw new Error(`no user id for identity ${key}`);
      state.ids[key] = {
        key,
        id: meBody.id,
        email,
        displayName: displayName || meBody.displayName || '',
        context,
        requests,
      };
    }
  }

  /** Creates the hub-managed fixture project as key. */
  async function createProject(key: K, name: string): Promise<string> {
    const res = await api(key, 'POST', '/api/v1/projects', { name });
    if (res.status !== 201) throw new Error(`${key} creates project ${name}: ${res.status}`);
    state.projectId = (res.body as { id: string }).id;
    return state.projectId;
  }

  /** Adds member to the fixture project as project-member, acting as owner (fixture). */
  async function addMember(owner: K, member: K, fixtures: Record<string, unknown>): Promise<void> {
    const projectId = state.projectId!;
    const roles = await api(owner, 'GET', `/api/v1/projects/${projectId}/members/assignable-roles`);
    const items =
      ((roles.body ?? {}) as { items?: Array<{ id: string; name: string }> }).items ?? [];
    const role = items.find((r) => r.name === 'project-member');
    if (!role) throw new Error(`no assignable role project-member (status ${roles.status})`);
    const res = await api(owner, 'POST', `/api/v1/projects/${projectId}/members`, {
      roleDefinitionId: role.id,
      principalType: 'user',
      principalId: who(member).email,
    });
    fixtures[member] = { role: 'project-member', status: res.status };
    if (res.status !== 201 && res.status !== 200) {
      throw new Error(`add ${member} as project-member: ${res.status} ${String(res.code)}`);
    }
    state.bindings.push({ projectId, key: member, id: (res.body as { id: string }).id });
  }

  /** Asserts each identity still holds its own session; returns key → 'ok' or the error. */
  async function identityChecks(): Promise<Record<string, string>> {
    const out: Record<string, string> = {};
    for (const key of opts.keys) {
      const identity = state.ids[key];
      if (!identity) continue;
      try {
        await assertTestIdentity(identity.context.request, {
          email: identity.email,
          role: 'member',
        });
        out[key] = 'ok';
      } catch (err) {
        out[key] = firstLine(err);
      }
    }
    return out;
  }

  /**
   * Deletes the identities (local hub) and checks absence: /auth/me 401
   * and not in the identity list. Residue lines are appended to residue.
   */
  async function deleteIdentities(
    residue: string[],
    states: Record<string, string>
  ): Promise<Record<string, unknown>> {
    const t = state.target;
    const hub = t?.hub;
    if (!hub) {
      return {
        identity:
          t?.mode === 'remote'
            ? 'remote target: NOT verified here; the runner deletes the identities out of band, ' +
              'and this evidence is written before that'
            : 'no target',
      };
    }
    const authMe: Record<string, number> = {};
    const deleted: Record<string, string> = {};
    for (const identity of Object.values(state.ids) as Array<Identity<K> | undefined>) {
      if (!identity) continue;
      try {
        runScionCLI(hub, ['hub', 'test-identity', 'delete', identity.id]);
        deleted[identity.key] = 'deleted';
      } catch (err) {
        deleted[identity.key] = `delete failed: ${firstLine(err)}`;
        residue.push(`identity ${identity.key}: ${firstLine(err)}`);
      }
      try {
        const me = await identity.context.request.get('/auth/me', { maxRedirects: 0 });
        authMe[identity.key] = me.status();
        if (me.status() !== 401) {
          residue.push(`identity ${identity.key}: session still answers /auth/me`);
        }
        await me.dispose();
      } catch (err) {
        residue.push(`cleanup error (identity ${identity.key} /auth/me): ${firstLine(err)}`);
      }
    }
    try {
      const listed = runScionCLI(hub, [
        'hub',
        'test-identity',
        'list',
        '--include-expired',
        '--json',
      ]);
      for (const identity of Object.values(state.ids) as Array<Identity<K> | undefined>) {
        if (!identity) continue;
        states[`identity ${identity.key}`] = listed.includes(identity.id) ? 'present' : 'absent';
      }
    } catch (err) {
      residue.push(`cleanup error (identity list): ${firstLine(err)}`);
    }
    return { identity: deleted, authMeAfterDelete: authMe };
  }

  /** Results of a finished test, for the evidence. */
  function recordResult(info: {
    title: string;
    status?: string;
    annotations: ReadonlyArray<{ type: string; description?: string }>;
    duration: number;
  }): void {
    const results = (state.evidence.results ??= {}) as Record<string, unknown>;
    results[info.title] = {
      status: info.status,
      skipReason: info.annotations.find((a) => a.type === 'skip')?.description ?? null,
      durationMs: info.duration,
      annotations: annotationsOf(info.annotations),
    };
  }

  return {
    state,
    target,
    resolveTarget,
    who,
    api,
    adminToken,
    adminRequest,
    readExperiment,
    setExperiment,
    rowRecord,
    seedRows,
    notRun,
    row,
    finding,
    pageOf,
    setUpIdentities,
    createProject,
    addMember,
    identityChecks,
    deleteIdentities,
    recordResult,
  };
}

export type Journey<K extends string> = ReturnType<typeof createJourney<K>>;
