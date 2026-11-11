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

import { describe, it, expect, vi } from 'vitest';
import { deleteGCPServiceAccount, forceDeleteUrl } from './gcp-service-account-delete.js';

function json(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

const inUse = (clearable: boolean): Response =>
  json(409, {
    error: {
      code: 'sa_in_use',
      message: 'service account is in use by the default of project p1.',
      details: { impact: { defaults: [{ tier: clearable ? 'project' : 'hub', clearable }] } },
    },
  });

describe('deleteGCPServiceAccount', () => {
  it('deletes without asking when nothing references the account', async () => {
    const doDelete = vi.fn().mockResolvedValue(json(200, { deleted: true }));
    const confirm = vi.fn();
    expect(await deleteGCPServiceAccount('/sa/1', doDelete, confirm)).toEqual({
      status: 'deleted',
    });
    expect(confirm).not.toHaveBeenCalled();
  });

  it('accepts a 204 from an older hub', async () => {
    const doDelete = vi.fn().mockResolvedValue(new Response(null, { status: 204 }));
    expect(await deleteGCPServiceAccount('/sa/1', doDelete, vi.fn())).toEqual({
      status: 'deleted',
    });
  });

  it('asks, then retries with force when every default is clearable', async () => {
    const doDelete = vi
      .fn()
      .mockResolvedValueOnce(inUse(true))
      .mockResolvedValueOnce(json(200, { deleted: true }));
    const confirm = vi.fn().mockResolvedValue(true);
    expect(await deleteGCPServiceAccount('/sa/1', doDelete, confirm)).toEqual({
      status: 'deleted',
    });
    expect(confirm).toHaveBeenCalledOnce();
    expect(doDelete).toHaveBeenLastCalledWith('/sa/1?force=true');
  });

  it('stops when the user declines', async () => {
    const doDelete = vi.fn().mockResolvedValueOnce(inUse(true));
    const confirm = vi.fn().mockResolvedValue(false);
    expect(await deleteGCPServiceAccount('/sa/1', doDelete, confirm)).toEqual({
      status: 'cancelled',
    });
    expect(doDelete).toHaveBeenCalledOnce();
  });

  it('reports the hub default as an error without offering force', async () => {
    const doDelete = vi.fn().mockResolvedValueOnce(inUse(false));
    const confirm = vi.fn();
    const out = await deleteGCPServiceAccount('/sa/1', doDelete, confirm);
    expect(out.status).toBe('failed');
    expect(confirm).not.toHaveBeenCalled();
  });

  it('surfaces other errors', async () => {
    const doDelete = vi
      .fn()
      .mockResolvedValue(json(403, { error: { code: 'forbidden', message: 'nope' } }));
    expect(await deleteGCPServiceAccount('/sa/1', doDelete, vi.fn())).toEqual({
      status: 'failed',
      message: 'nope',
    });
  });
});

describe('forceDeleteUrl', () => {
  it('appends force to a URL with or without a query', () => {
    expect(forceDeleteUrl('/a')).toBe('/a?force=true');
    expect(forceDeleteUrl('/a?x=1')).toBe('/a?x=1&force=true');
  });
});
