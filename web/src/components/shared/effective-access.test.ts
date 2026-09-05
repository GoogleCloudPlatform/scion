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
 * Effective Access — Acceptance Gate Invariant Tests (R2)
 *
 * Tests proving the three acceptance gate invariants:
 *
 *   1. Overlapping boundaries are shown as "removed by both" (not "won by")
 *   2. No priority/override/winner terminology in any user-facing string
 *   3. No redaction bypass — redacted boundaries show placeholder, no
 *      secondary name-fetch occurs
 *
 * Additionally tests all 5 DeniedPermission denial categories and the
 * _explainLoaded guard fix (R1).
 */

import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';
import { readFileSync } from 'fs';
import { resolve } from 'path';

// Mock confirm-dialog at module level so showConfirm auto-confirms in all tests.
vi.mock('./confirm-dialog.js', () => ({
  showConfirm: vi.fn(() => Promise.resolve(true)),
}));

import type {
  BoundaryLayer,
  IntrinsicRestriction,
  DeniedPermission,
  PermissionDenialReason,
} from './authorization-layer-stack.js';

/* -------------------------------------------------------------------------- */
/* Helper: extract html`` template content from a source file                 */
/* -------------------------------------------------------------------------- */

function extractTemplateContent(source: string): string {
  const htmlTemplates: string[] = [];
  const regex = /html`([\s\S]*?)`/g;
  let match;
  while ((match = regex.exec(source)) !== null) {
    htmlTemplates.push(match[1]);
  }
  return htmlTemplates.join('\n');
}

/**
 * Extract all user-facing string literals from html`` templates.
 * Strips tags and template expressions, leaving only visible text.
 */
function extractUserFacingText(source: string): string {
  const templates = extractTemplateContent(source);
  // Remove HTML tags
  let text = templates.replace(/<[^>]+>/g, ' ');
  // Remove template expressions ${...} — simple nesting only
  text = text.replace(/\$\{[^}]*\}/g, ' ');
  return text;
}

/* -------------------------------------------------------------------------- */
/* 1. Overlapping boundaries — "removed by both" (not "won by")               */
/* -------------------------------------------------------------------------- */

describe('Invariant 1: Overlapping boundaries shown as "removed by both"', () => {
  it('renderBoundaryRow shows overlap count with "removed by both" text', () => {
    const source = readFileSync(resolve(__dirname, './authorization-layer-stack.ts'), 'utf-8');
    const templates = extractTemplateContent(source);

    // The overlap text MUST use "removed by both"
    expect(templates).toContain('removed by both');
  });

  it('overlap text does NOT use "won by" language', () => {
    const source = readFileSync(resolve(__dirname, './authorization-layer-stack.ts'), 'utf-8');
    const templates = extractTemplateContent(source);

    expect(templates).not.toMatch(/\bwon by\b/i);
    expect(templates).not.toMatch(/\bwins\b/i);
    expect(templates).not.toMatch(/\bwinning\b/i);
  });

  it('BoundaryLayer type includes overlapCount field', () => {
    // Verify the type contract by constructing a valid BoundaryLayer
    const boundary: BoundaryLayer = {
      id: 'b1',
      name: 'Test Boundary',
      status: 'active',
      removedCount: 5,
      overlapCount: 2,
    };

    expect(boundary.overlapCount).toBe(2);
    expect(boundary.removedCount).toBe(5);
  });

  it('overlap rendering is conditional on overlapCount > 0', () => {
    const source = readFileSync(resolve(__dirname, './authorization-layer-stack.ts'), 'utf-8');

    // Verify the guard: overlapCount > 0 before showing the overlap note
    expect(source).toContain('overlapCount > 0');
  });
});

/* -------------------------------------------------------------------------- */
/* 2. No priority/override/winner terminology in user-facing strings          */
/* -------------------------------------------------------------------------- */

describe('Invariant 2: No priority/override/winner terminology', () => {
  const componentsToCheck = [
    {
      name: 'authorization-layer-stack.ts',
      path: './authorization-layer-stack.ts',
    },
    {
      name: 'effective-role-provenance.ts',
      path: './effective-role-provenance.ts',
    },
    {
      name: 'effective-access-boundary-notice.ts',
      path: './effective-access-boundary-notice.ts',
    },
  ];

  const prohibitedTerms = [
    /\bpriority\b/i,
    /\boverride\b/i,
    /\boverrides\b/i,
    /\boverriding\b/i,
    /\bwinner\b/i,
    /\bwinning\b/i,
    /\bwon by\b/i,
    /\bwins\b/i,
  ];

  for (const component of componentsToCheck) {
    describe(`${component.name}`, () => {
      it('has no prohibited terminology in user-facing template strings', () => {
        const source = readFileSync(resolve(__dirname, component.path), 'utf-8');
        const userText = extractUserFacingText(source);

        for (const term of prohibitedTerms) {
          expect(
            userText,
            `Found prohibited term ${term} in ${component.name} template text`
          ).not.toMatch(term);
        }
      });
    });
  }

  it('terminology guard comments are present in all F5 components', () => {
    for (const component of componentsToCheck) {
      const source = readFileSync(resolve(__dirname, component.path), 'utf-8');

      // Each file should have a terminology comment warning against
      // priority/override/winner language
      expect(source, `${component.name} should contain terminology guard comment`).toMatch(
        /TERMINOLOGY.*never.*(?:priority|override|winner)/is
      );
    }
  });
});

/* -------------------------------------------------------------------------- */
/* 3. No redaction bypass                                                     */
/* -------------------------------------------------------------------------- */

describe('Invariant 3: No redaction bypass', () => {
  it('redacted boundaries render as "Access constraint (details unavailable)"', () => {
    const source = readFileSync(resolve(__dirname, './authorization-layer-stack.ts'), 'utf-8');

    // The source MUST contain the redacted placeholder text in a template
    expect(source).toContain('Access constraint (details unavailable)');
  });

  it('redacted boundary check covers both redacted flag and null name', () => {
    const source = readFileSync(resolve(__dirname, './authorization-layer-stack.ts'), 'utf-8');

    // The guard must check both boundary.redacted and boundary.name === null
    expect(source).toMatch(
      /boundary\.redacted.*boundary\.name === null|boundary\.name === null.*boundary\.redacted/
    );
  });

  it('redacted boundary does not trigger any name-fetch or link rendering', () => {
    const source = readFileSync(resolve(__dirname, './authorization-layer-stack.ts'), 'utf-8');

    // When redacted, the template should show the span.boundary-redacted
    // and NOT render a link. Verify the structure: redacted check comes
    // BEFORE the link rendering in a conditional (ternary).
    expect(source).toMatch(/boundary-redacted.*Access constraint \(details unavailable\)/s);

    // In the renderBoundaryRow method, the redacted branch (truthy)
    // renders BEFORE the link branch (falsy) in the ternary. Extract
    // just the render method to verify ordering.
    const renderMethod = source.match(
      /renderBoundaryRow[\s\S]*?boundary-redacted[\s\S]*?boundary-link/
    );
    expect(renderMethod).not.toBeNull();
  });

  it('BoundaryLayer type supports null name for redacted boundaries', () => {
    const redactedBoundary: BoundaryLayer = {
      id: 'b-redacted',
      name: null,
      status: 'active',
      removedCount: 3,
      overlapCount: 0,
      redacted: { message: 'Insufficient permissions', reason: 'access_denied' },
    };

    expect(redactedBoundary.name).toBeNull();
    expect(redactedBoundary.redacted).toBeDefined();
    expect(redactedBoundary.redacted!.message).toBe('Insufficient permissions');
  });

  it('denial reason renderer handles null boundary names in removed_by_boundaries', () => {
    const source = readFileSync(resolve(__dirname, './authorization-layer-stack.ts'), 'utf-8');

    // The denial reason renderer must map null boundary names to the
    // placeholder text, not attempt a fetch
    expect(source).toMatch(/\.map\(\(n\).*n \?\? ['"]access constraint \(details unavailable\)['"]/i);
  });

  it('no secondary apiFetch call exists in authorization-layer-stack for boundary names', () => {
    const source = readFileSync(resolve(__dirname, './authorization-layer-stack.ts'), 'utf-8');

    // The layer stack component should NOT import or call apiFetch — it
    // receives all data via properties. Any fetch here would be a
    // redaction bypass.
    expect(source).not.toContain('apiFetch');
    expect(source).not.toContain('import.*api.js');
  });
});

/* -------------------------------------------------------------------------- */
/* 4. All 5 denial categories are represented                                 */
/* -------------------------------------------------------------------------- */

describe('All 5 DeniedPermission denial categories', () => {
  const denialCategories: PermissionDenialReason[] = [
    { type: 'never_granted' },
    { type: 'inactive_grant', grantStatus: 'expired' },
    { type: 'removed_by_boundaries', boundaryNames: ['Boundary A'] },
    { type: 'removed_by_restriction', restrictionLabel: 'Credential scope' },
    { type: 'evaluation_failed', correlationId: 'corr-123' },
  ];

  it('PermissionDenialReason union type covers all 5 categories', () => {
    // Construct a DeniedPermission with all 5 reason types
    const dp: DeniedPermission = {
      permissionId: 'test.permission',
      reasons: denialCategories,
    };

    expect(dp.reasons).toHaveLength(5);
    expect(dp.reasons.map((r) => r.type)).toEqual([
      'never_granted',
      'inactive_grant',
      'removed_by_boundaries',
      'removed_by_restriction',
      'evaluation_failed',
    ]);
  });

  it('renderDenialReason has a case for each of the 5 categories', () => {
    const source = readFileSync(resolve(__dirname, './authorization-layer-stack.ts'), 'utf-8');

    for (const reason of denialCategories) {
      expect(source, `Missing case for denial type '${reason.type}'`).toContain(
        `case '${reason.type}'`
      );
    }
  });

  it('each denial category renders a distinct reason-tag class', () => {
    const source = readFileSync(resolve(__dirname, './authorization-layer-stack.ts'), 'utf-8');

    // Each category has a corresponding CSS class on the reason-tag
    const expectedClasses = [
      'reason-tag never-granted',
      'reason-tag inactive',
      'reason-tag boundary',
      'reason-tag restriction',
      'reason-tag failed',
    ];

    for (const cls of expectedClasses) {
      expect(source, `Missing CSS class '${cls}'`).toContain(cls);
    }
  });

  it('each denial category has user-facing label text', () => {
    const source = readFileSync(resolve(__dirname, './authorization-layer-stack.ts'), 'utf-8');
    const templates = extractTemplateContent(source);

    // Verify each category has descriptive label text
    expect(templates).toContain('Never granted');
    expect(templates).toContain('Grant');
    expect(templates).toContain('Removed by');
    expect(templates).toContain('Evaluation failed');
  });
});

/* -------------------------------------------------------------------------- */
/* 5. R1 fix: _explainLoaded guard prevents re-fetch on potentialCount=0      */
/* -------------------------------------------------------------------------- */

describe('R1 fix: _explainLoaded guard', () => {
  it('handleLayersToggle uses _explainLoaded instead of potentialCount===0', () => {
    const source = readFileSync(resolve(__dirname, './effective-role-provenance.ts'), 'utf-8');

    // The guard MUST use _explainLoaded, not potentialCount
    expect(source).toContain('!this._explainLoaded');

    // The old incorrect guard must NOT be present
    expect(source).not.toMatch(/this\.potentialCount === 0/);
  });

  it('_explainLoaded is declared as a @state() field', () => {
    const source = readFileSync(resolve(__dirname, './effective-role-provenance.ts'), 'utf-8');

    // Must be a reactive state property
    expect(source).toMatch(/@state\(\)\s+private\s+_explainLoaded/);
  });

  it('_explainLoaded is set to true in loadExplainLayers after data mapping', () => {
    const source = readFileSync(resolve(__dirname, './effective-role-provenance.ts'), 'utf-8');

    // Must be set in the try block of loadExplainLayers, after data mapping
    // and before the catch block
    const loadMethod = source.match(/async loadExplainLayers\(\)[\s\S]*?finally/);
    expect(loadMethod).not.toBeNull();

    const methodBody = loadMethod![0];
    // _explainLoaded = true must appear after the data mapping and before catch
    expect(methodBody).toContain('this._explainLoaded = true');

    // Verify it appears AFTER the boundary/restriction mapping
    const loadedIdx = methodBody.indexOf('this._explainLoaded = true');
    const boundariesIdx = methodBody.indexOf('this.boundaries =');
    const restrictionsIdx = methodBody.indexOf('this.restrictions =');

    expect(loadedIdx).toBeGreaterThan(boundariesIdx);
    expect(loadedIdx).toBeGreaterThan(restrictionsIdx);
  });

  it('sibling component effective-access-boundary-notice uses the same loaded-flag pattern', () => {
    const source = readFileSync(
      resolve(__dirname, './effective-access-boundary-notice.ts'),
      'utf-8'
    );

    // The sibling already uses the correct pattern — verify consistency
    expect(source).toMatch(/@state\(\)\s+private\s+loaded\s*=\s*false/);
    expect(source).toContain('this.loaded = true');
  });
});

/* -------------------------------------------------------------------------- */
/* 6. Role card displays role name, not UUID                                   */
/* -------------------------------------------------------------------------- */

describe('Role cards display role names, not UUIDs', () => {
  it('renderRoleCard uses roleName with roleDefinitionId as fallback', () => {
    const source = readFileSync(resolve(__dirname, './effective-role-provenance.ts'), 'utf-8');

    // The template should use roleName || roleDefinitionId as fallback
    expect(source).toContain('binding.roleName || binding.roleDefinitionId');
  });

  it('EffectiveRoleBinding interface includes roleName field', () => {
    const source = readFileSync(resolve(__dirname, './effective-role-provenance.ts'), 'utf-8');

    // The interface must include a roleName field
    expect(source).toMatch(/roleName:\s*string/);
  });
});

/* -------------------------------------------------------------------------- */
/* 7. Composition expand calls the real effective-access endpoint               */
/* -------------------------------------------------------------------------- */

describe('Composition expand targets the real effective-access endpoint', () => {
  it('loadExplainLayers calls /api/v1/admin/effective-access, not /admin/access-explain', () => {
    const source = readFileSync(resolve(__dirname, './effective-role-provenance.ts'), 'utf-8');

    expect(source).toContain('/api/v1/admin/effective-access');
    expect(source).not.toContain('/api/v1/admin/access-explain');
  });

  it('effective-access-boundary-notice does not target /admin/access-explain', () => {
    const source = readFileSync(
      resolve(__dirname, './effective-access-boundary-notice.ts'),
      'utf-8'
    );

    expect(source).not.toContain('/api/v1/admin/access-explain');
    expect(source).not.toContain("'/admin/access-explain");
    expect(source).not.toContain('"/admin/access-explain');
  });

  it('no file references nonexistent /admin/access-explain route', () => {
    const files = [
      './effective-role-provenance.ts',
      './effective-access-boundary-notice.ts',
      './authorization-layer-stack.ts',
    ];

    for (const file of files) {
      const source = readFileSync(resolve(__dirname, file), 'utf-8');
      expect(source, `${file} should not reference /admin/access-explain`).not.toContain(
        '/admin/access-explain'
      );
    }
  });
});

/* -------------------------------------------------------------------------- */
/* 8. R6: Pre-click capability gating for effective-access composition        */
/* -------------------------------------------------------------------------- */

describe('Pre-click capability gating for effective-access (R6)', () => {
  it('preCheckExplainAccess fires during loadEffectiveRoles, not on toggle click', () => {
    const source = readFileSync(resolve(__dirname, './effective-role-provenance.ts'), 'utf-8');

    // The pre-check must be called inside loadEffectiveRoles, which runs at
    // mount time — before any user interaction.
    expect(source).toContain('void this.preCheckExplainAccess()');

    // It should be in loadEffectiveRoles, not in handleLayersToggle.
    const loadFnMatch = source.match(
      /async loadEffectiveRoles\(\)[\s\S]*?(?=private async|private [a-z]|^\s*\/\*\*)/m
    );
    expect(loadFnMatch).toBeTruthy();
    expect(loadFnMatch![0]).toContain('preCheckExplainAccess');
  });

  it('preCheckExplainAccess uses HEAD method with suppressed 403 toast', () => {
    const source = readFileSync(resolve(__dirname, './effective-role-provenance.ts'), 'utf-8');
    // The pre-check should use HEAD to minimize data transfer.
    expect(source).toContain("method: 'HEAD'");
    // The 403 toast must be suppressed — this is an expected authorization
    // probe, not an unexpected denial.
    expect(source).toContain('suppressAccessDeniedToast: true');
  });

  it('preCheckExplainAccess sets _explainForbidden on 403 response', () => {
    const source = readFileSync(resolve(__dirname, './effective-role-provenance.ts'), 'utf-8');
    const preCheckMatch = source.match(
      /async preCheckExplainAccess\(\)[\s\S]*?(?=\n\s*(?:private|public|protected|override|\/\*\*))/m
    );
    expect(preCheckMatch).toBeTruthy();
    const body = preCheckMatch![0];
    expect(body).toContain('res.status === 403');
    expect(body).toContain('this._explainForbidden = true');
  });

  it('toggle is hidden until pre-check resolves (_explainPreChecked)', () => {
    const source = readFileSync(resolve(__dirname, './effective-role-provenance.ts'), 'utf-8');
    // The _explainPreChecked flag must be declared as a @state() field.
    expect(source).toMatch(/@state\(\)\s+private\s+_explainPreChecked/);
    // preCheckExplainAccess must set it to true in the finally block.
    const preCheckMatch = source.match(
      /async preCheckExplainAccess\(\)[\s\S]*?(?=\n\s*(?:private|public|protected|override|\/\*\*))/m
    );
    expect(preCheckMatch).toBeTruthy();
    expect(preCheckMatch![0]).toContain('this._explainPreChecked = true');
    // renderLayersSection must check !this._explainPreChecked.
    const renderMatch = source.match(
      /private renderLayersSection\(\)[\s\S]*?(?=\n\s*private\s)/m
    );
    expect(renderMatch).toBeTruthy();
    expect(renderMatch![0]).toContain('_explainPreChecked');
  });

  it('renderLayersSection returns nothing when _explainForbidden is true', () => {
    const source = readFileSync(resolve(__dirname, './effective-role-provenance.ts'), 'utf-8');
    // The renderLayersSection method must check _explainForbidden and return
    // `nothing` when true. Match the full method body up to the next method.
    const renderMatch = source.match(
      /private renderLayersSection\(\)[\s\S]*?(?=\n\s*private\s)/m
    );
    expect(renderMatch).toBeTruthy();
    const body = renderMatch![0];
    expect(body).toContain('_explainForbidden');
    expect(body).toContain('nothing');
  });
});

/* ========================================================================== */
/* Behavioral Component Tests (R2 correction O2)                              */
/*                                                                            */
/* These tests mount the real component, mock fetch, and exercise rendered     */
/* buttons/events rather than asserting on source strings.                     */
/* ========================================================================== */

// eslint-disable-next-line @typescript-eslint/no-explicit-any
let EffectiveRoleProvenance: any;

/** Wait for async loads + Lit update cycle. */
async function tick(el: { updateComplete: Promise<boolean> }, ms = 250): Promise<void> {
  await new Promise((r) => setTimeout(r, ms));
  await el.updateComplete;
}

function query(el: HTMLElement, sel: string): Element | null {
  return el.shadowRoot?.querySelector(sel) ?? null;
}

function queryAll(el: HTMLElement, sel: string): Element[] {
  return Array.from(el.shadowRoot?.querySelectorAll(sel) ?? []);
}

/** Standard mock bindings: one direct, one group-derived. */
function makeBindings() {
  return {
    items: [
      {
        id: 'b-direct',
        roleDefinitionId: 'role-1',
        roleName: 'Editor',
        principalType: 'user',
        principalId: 'user-1',
        scopeType: 'system',
        scopeId: '',
        createdAt: '2026-01-01T00:00:00Z',
        source: 'direct',
      },
      {
        id: 'b-group',
        roleDefinitionId: 'role-2',
        roleName: 'Viewer',
        principalType: 'group',
        principalId: 'group-1',
        scopeType: 'system',
        scopeId: '',
        createdAt: '2026-01-02T00:00:00Z',
        source: 'group-1',
        sourceGroupName: 'Eng Team',
      },
    ],
    totalCount: 2,
  };
}

/**
 * Build a fetch handler that responds to API probes and binding loads.
 *
 * @param opts.createStatus  HTTP status for the POST probe (400=allowed, 403=denied)
 * @param opts.deleteStatus  HTTP status for the DELETE probe (404=allowed, 403=denied)
 * @param opts.explainStatus HTTP status for HEAD effective-access probe
 */
function makeFetchHandler(opts?: {
  createStatus?: number;
  deleteStatus?: number;
  explainStatus?: number;
}) {
  const createStatus = opts?.createStatus ?? 400;
  const deleteStatus = opts?.deleteStatus ?? 404;
  const explainStatus = opts?.explainStatus ?? 200;
  const calls: { url: string; method: string; body?: string }[] = [];

  const handler = async (
    url: string | URL | Request,
    init?: RequestInit
  ): Promise<Response> => {
    const path = typeof url === 'string' ? url : url instanceof URL ? url.pathname : url.url;
    const method = init?.method ?? 'GET';
    const body = typeof init?.body === 'string' ? init.body : undefined;
    calls.push({ url: path, method, body });

    // HEAD effective-access probe
    if (path.includes('/api/v1/admin/effective-access') && method === 'HEAD') {
      return new Response('', { status: explainStatus });
    }

    // Mutation pre-check: POST probe (create)
    if (path === '/api/v1/admin/role-bindings' && method === 'POST' && body === '{}') {
      return new Response('{}', { status: createStatus });
    }

    // Mutation pre-check: DELETE probe (delete sentinel)
    if (path.includes('/api/v1/admin/role-bindings/00000000') && method === 'DELETE') {
      return new Response('{}', { status: deleteStatus });
    }

    // Actual POST to create a binding
    if (path === '/api/v1/admin/role-bindings' && method === 'POST' && body !== '{}') {
      return new Response(JSON.stringify({ id: 'new-binding' }), { status: 201 });
    }

    // Actual DELETE to remove a binding
    if (path.includes('/api/v1/admin/role-bindings/b-direct') && method === 'DELETE') {
      return new Response('', { status: 204 });
    }

    // Binding list
    if (path.includes('/api/v1/admin/role-bindings') && method === 'GET') {
      return new Response(JSON.stringify(makeBindings()), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      });
    }

    // Role definitions (for add dialog)
    if (path.includes('/api/v1/admin/roles') && method === 'GET') {
      return new Response(
        JSON.stringify({
          items: [
            { id: 'role-1', name: 'Editor', scopeType: 'system' },
            { id: 'role-2', name: 'Viewer', scopeType: 'system' },
          ],
        }),
        { status: 200, headers: { 'Content-Type': 'application/json' } }
      );
    }

    return new Response('{}', { status: 200 });
  };
  return { handler, calls };
}

async function createEl(
  fetchHandler: (url: string | URL | Request, init?: RequestInit) => Promise<Response>,
  props?: { principalType?: string; principalId?: string }
) {
  vi.stubGlobal('fetch', vi.fn(fetchHandler));
  const el = document.createElement('scion-effective-role-provenance') as InstanceType<
    typeof EffectiveRoleProvenance
  >;
  el.principalType = (props?.principalType ?? 'user') as 'user' | 'agent';
  el.principalId = props?.principalId ?? 'user-1';
  document.body.appendChild(el);
  await tick(el, 400);
  return el;
}

beforeAll(async () => {
  const mod = await import('./effective-role-provenance.js');
  EffectiveRoleProvenance = mod.ScionEffectiveRoleProvenance;
});

afterEach(() => {
  document.body.innerHTML = '';
  vi.restoreAllMocks();
});

/* -------------------------------------------------------------------------- */
/* 9. Behavioral: create-only permission (can create, cannot delete)          */
/* -------------------------------------------------------------------------- */

describe('Behavioral: create-only permission', () => {
  it('shows Add Binding button but no trash icons', async () => {
    const { handler } = makeFetchHandler({ createStatus: 400, deleteStatus: 403 });
    const el = await createEl(handler);

    // Add Binding button should be visible
    const addBtn = query(el, 'sl-button[variant="primary"]');
    expect(addBtn).toBeTruthy();
    expect(addBtn?.textContent?.trim()).toContain('Add Binding');

    // Trash icons should NOT be present (delete denied)
    const trashBtns = queryAll(el, 'sl-icon-button[name="trash"]');
    expect(trashBtns.length).toBe(0);
  });
});

/* -------------------------------------------------------------------------- */
/* 10. Behavioral: delete-only permission (can delete, cannot create)         */
/* -------------------------------------------------------------------------- */

describe('Behavioral: delete-only permission', () => {
  it('shows trash icon on direct binding but no Add Binding button', async () => {
    const { handler } = makeFetchHandler({ createStatus: 403, deleteStatus: 404 });
    const el = await createEl(handler);

    // Add Binding button should NOT be visible
    const addBtns = queryAll(el, 'sl-button[variant="primary"]');
    const addBtn = addBtns.find((b) => b.textContent?.trim().includes('Add Binding'));
    expect(addBtn).toBeUndefined();

    // Trash icon should be present on the direct binding
    const trashBtns = queryAll(el, 'sl-icon-button[name="trash"]');
    expect(trashBtns.length).toBe(1); // Only on the direct binding, not the group one
  });
});

/* -------------------------------------------------------------------------- */
/* 11. Behavioral: neither permission (cannot create, cannot delete)          */
/* -------------------------------------------------------------------------- */

describe('Behavioral: neither create nor delete permission', () => {
  it('shows no Add Binding button and no trash icons', async () => {
    const { handler } = makeFetchHandler({ createStatus: 403, deleteStatus: 403 });
    const el = await createEl(handler);

    const addBtns = queryAll(el, 'sl-button[variant="primary"]');
    const addBtn = addBtns.find((b) => b.textContent?.trim().includes('Add Binding'));
    expect(addBtn).toBeUndefined();

    const trashBtns = queryAll(el, 'sl-icon-button[name="trash"]');
    expect(trashBtns.length).toBe(0);
  });
});

/* -------------------------------------------------------------------------- */
/* 12. Behavioral: both permissions (can create and delete)                   */
/* -------------------------------------------------------------------------- */

describe('Behavioral: both create and delete permissions', () => {
  it('shows Add Binding button and trash icon on direct binding', async () => {
    const { handler } = makeFetchHandler({ createStatus: 400, deleteStatus: 404 });
    const el = await createEl(handler);

    // Add Binding button present
    const addBtns = queryAll(el, 'sl-button[variant="primary"]');
    const addBtn = addBtns.find((b) => b.textContent?.trim().includes('Add Binding'));
    expect(addBtn).toBeTruthy();

    // Trash icon on direct binding
    const trashBtns = queryAll(el, 'sl-icon-button[name="trash"]');
    expect(trashBtns.length).toBe(1);
  });

  it('trash icon is only on direct bindings, not group-derived', async () => {
    const { handler } = makeFetchHandler({ createStatus: 400, deleteStatus: 404 });
    const el = await createEl(handler);

    // There are 2 role cards total
    const cards = queryAll(el, '.role-card');
    expect(cards.length).toBe(2);

    // Only the direct binding card has a trash icon
    const directCard = cards.find((c) => c.querySelector('.provenance.direct'));
    const groupCard = cards.find((c) => c.querySelector('.provenance.group'));

    expect(directCard?.querySelector('sl-icon-button[name="trash"]')).toBeTruthy();
    expect(groupCard?.querySelector('sl-icon-button[name="trash"]')).toBeFalsy();
  });
});

/* -------------------------------------------------------------------------- */
/* 13. Behavioral: pending probes hide action controls                        */
/* -------------------------------------------------------------------------- */

describe('Behavioral: pending probes', () => {
  it('Add Binding button and trash icons are hidden before probes resolve', async () => {
    const neverResolve = (
      _url: string | URL | Request,
      init?: RequestInit
    ): Promise<Response> => {
      const method = init?.method ?? 'GET';
      const path =
        typeof _url === 'string' ? _url : _url instanceof URL ? _url.pathname : _url.url;

      // Binding list responds immediately so we have content
      if (path.includes('/api/v1/admin/role-bindings') && method === 'GET') {
        return Promise.resolve(
          new Response(JSON.stringify(makeBindings()), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          })
        );
      }

      // Mutation probes and explain probes: hang forever
      if (
        (method === 'POST' || method === 'DELETE' || method === 'HEAD') &&
        (path.includes('role-bindings') || path.includes('effective-access'))
      ) {
        return new Promise(() => {}); // never resolves
      }

      return Promise.resolve(new Response('{}', { status: 200 }));
    };

    vi.stubGlobal('fetch', vi.fn(neverResolve));
    const el = document.createElement('scion-effective-role-provenance') as InstanceType<
      typeof EffectiveRoleProvenance
    >;
    el.principalType = 'user';
    el.principalId = 'user-1';
    document.body.appendChild(el);
    await tick(el, 300);

    // While probes are pending, neither Add Binding nor trash should render
    const addBtns = queryAll(el, 'sl-button[variant="primary"]');
    const addBtn = addBtns.find((b) => b.textContent?.trim().includes('Add Binding'));
    expect(addBtn).toBeUndefined();

    const trashBtns = queryAll(el, 'sl-icon-button[name="trash"]');
    expect(trashBtns.length).toBe(0);
  });
});

/* -------------------------------------------------------------------------- */
/* 14. Behavioral: direct vs inherited binding provenance labels              */
/* -------------------------------------------------------------------------- */

describe('Behavioral: direct vs inherited binding provenance', () => {
  it('renders "Direct" label on direct binding and "Via group:" on inherited', async () => {
    const { handler } = makeFetchHandler();
    const el = await createEl(handler);

    const provenances = queryAll(el, '.provenance');
    expect(provenances.length).toBe(2);

    const directProv = provenances.find((p) => p.classList.contains('direct'));
    expect(directProv).toBeTruthy();
    expect(directProv?.textContent).toContain('Direct');

    const groupProv = provenances.find((p) => p.classList.contains('group'));
    expect(groupProv).toBeTruthy();
    expect(groupProv?.textContent).toContain('Via group:');
    expect(groupProv?.textContent).toContain('Eng Team');
  });
});

/* -------------------------------------------------------------------------- */
/* 15. Behavioral: delete confirmation flow                                   */
/* -------------------------------------------------------------------------- */

describe('Behavioral: delete confirmation and request', () => {
  it('clicking trash triggers DELETE request for direct binding', async () => {
    const { handler, calls } = makeFetchHandler({ createStatus: 400, deleteStatus: 404 });
    const el = await createEl(handler);

    const trashBtn = query(el, 'sl-icon-button[name="trash"]') as HTMLElement;
    expect(trashBtn).toBeTruthy();
    trashBtn.click();
    await tick(el, 400);

    const deleteCalls = calls.filter(
      (c) => c.method === 'DELETE' && c.url.includes('b-direct')
    );
    expect(deleteCalls.length).toBe(1);
  });

  it('sends DELETE and triggers refresh on success', async () => {
    const { handler, calls } = makeFetchHandler({ createStatus: 400, deleteStatus: 404 });
    const el = await createEl(handler);
    const initialGetCalls = calls.filter(
      (c) => c.method === 'GET' && c.url.includes('role-bindings')
    ).length;

    const trashBtn = query(el, 'sl-icon-button[name="trash"]') as HTMLElement;
    trashBtn?.click();
    await tick(el, 600);

    // Verify DELETE was sent for the direct binding
    const deleteCalls = calls.filter(
      (c) => c.method === 'DELETE' && c.url.includes('b-direct')
    );
    expect(deleteCalls.length).toBe(1);

    // Verify a refresh GET was triggered after the delete
    const postDeleteGetCalls = calls.filter(
      (c) => c.method === 'GET' && c.url.includes('role-bindings')
    ).length;
    expect(postDeleteGetCalls).toBeGreaterThan(initialGetCalls);
  });
});

/* -------------------------------------------------------------------------- */
/* 16. Behavioral: create binding dialog with locked principal                */
/* -------------------------------------------------------------------------- */

describe('Behavioral: create binding request payload', () => {
  it('Add Binding dialog shows locked principal with lock icon', async () => {
    const { handler } = makeFetchHandler({ createStatus: 400, deleteStatus: 404 });
    const el = await createEl(handler);

    const addBtns = queryAll(el, 'sl-button[variant="primary"]');
    const addBtn = addBtns.find((b) =>
      b.textContent?.trim().includes('Add Binding')
    ) as HTMLElement;
    expect(addBtn).toBeTruthy();
    addBtn.click();
    await tick(el, 300);

    const dialog = query(el, 'sl-dialog[open]');
    expect(dialog).toBeTruthy();

    const lockedPrincipal = query(el, '.locked-principal');
    expect(lockedPrincipal).toBeTruthy();
    expect(lockedPrincipal?.textContent).toContain('user');
    expect(lockedPrincipal?.textContent).toContain('user-1');

    const lockIcon = lockedPrincipal?.querySelector('sl-icon[name="lock"]');
    expect(lockIcon).toBeTruthy();
  });
});

/* -------------------------------------------------------------------------- */
/* 17. Behavioral: error feedback on failed mutation                          */
/* -------------------------------------------------------------------------- */

describe('Behavioral: error feedback on failed mutation', () => {
  it('shows error feedback when DELETE fails', async () => {
    const calls: { url: string; method: string; body?: string }[] = [];
    const failingHandler = async (
      url: string | URL | Request,
      init?: RequestInit
    ): Promise<Response> => {
      const path = typeof url === 'string' ? url : url instanceof URL ? url.pathname : url.url;
      const method = init?.method ?? 'GET';
      const body = typeof init?.body === 'string' ? init.body : undefined;
      calls.push({ url: path, method, body });

      if (path.includes('/api/v1/admin/role-bindings/b-direct') && method === 'DELETE') {
        return new Response(JSON.stringify({ error: 'forbidden' }), { status: 403 });
      }
      if (path.includes('/api/v1/admin/role-bindings') && method === 'GET') {
        return new Response(JSON.stringify(makeBindings()), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        });
      }
      if (path === '/api/v1/admin/role-bindings' && method === 'POST') {
        return new Response('{}', { status: 400 });
      }
      if (path.includes('00000000') && method === 'DELETE') {
        return new Response('{}', { status: 404 });
      }
      if (method === 'HEAD') {
        return new Response('', { status: 200 });
      }
      return new Response('{}', { status: 200 });
    };

    const el = await createEl(failingHandler);

    const trashBtn = query(el, 'sl-icon-button[name="trash"]') as HTMLElement;
    trashBtn?.click();
    await tick(el, 400);

    const feedback = query(el, '.mutation-feedback.danger');
    expect(feedback).toBeTruthy();
  });
});

/* -------------------------------------------------------------------------- */
/* 18. Behavioral: successful refresh after mutation                          */
/* -------------------------------------------------------------------------- */

describe('Behavioral: successful refresh after mutation', () => {
  it('refreshes binding list after successful delete', async () => {
    const { handler, calls } = makeFetchHandler({ createStatus: 400, deleteStatus: 404 });
    const el = await createEl(handler);

    const initialGetCalls = calls.filter(
      (c) => c.method === 'GET' && c.url.includes('role-bindings')
    ).length;

    const trashBtn = query(el, 'sl-icon-button[name="trash"]') as HTMLElement;
    trashBtn?.click();
    await tick(el, 500);

    const postDeleteGetCalls = calls.filter(
      (c) => c.method === 'GET' && c.url.includes('role-bindings')
    ).length;
    expect(postDeleteGetCalls).toBeGreaterThan(initialGetCalls);
  });
});

/* -------------------------------------------------------------------------- */
/* 19. Behavioral: independent probe results (R2 correction)                  */
/* -------------------------------------------------------------------------- */

describe('Behavioral: independent create/delete probes (R2)', () => {
  it('probes POST and DELETE endpoints separately', async () => {
    const { handler, calls } = makeFetchHandler({ createStatus: 400, deleteStatus: 404 });
    await createEl(handler);

    const postProbe = calls.find(
      (c) => c.method === 'POST' && c.url === '/api/v1/admin/role-bindings' && c.body === '{}'
    );
    expect(postProbe).toBeTruthy();

    const deleteProbe = calls.find(
      (c) => c.method === 'DELETE' && c.url.includes('00000000')
    );
    expect(deleteProbe).toBeTruthy();
  });

  it('sets _canCreate=true and _canDelete=false when only POST succeeds', async () => {
    const { handler } = makeFetchHandler({ createStatus: 400, deleteStatus: 403 });
    const el = await createEl(handler);

    const addBtns = queryAll(el, 'sl-button[variant="primary"]');
    const addBtn = addBtns.find((b) => b.textContent?.trim().includes('Add Binding'));
    expect(addBtn).toBeTruthy();

    expect(queryAll(el, 'sl-icon-button[name="trash"]').length).toBe(0);
  });

  it('sets _canCreate=false and _canDelete=true when only DELETE succeeds', async () => {
    const { handler } = makeFetchHandler({ createStatus: 403, deleteStatus: 404 });
    const el = await createEl(handler);

    const addBtns = queryAll(el, 'sl-button[variant="primary"]');
    const addBtn = addBtns.find((b) => b.textContent?.trim().includes('Add Binding'));
    expect(addBtn).toBeUndefined();

    expect(queryAll(el, 'sl-icon-button[name="trash"]').length).toBe(1);
  });
});
