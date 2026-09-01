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

import { describe, it, expect } from 'vitest';
import { readFileSync } from 'fs';
import { resolve } from 'path';

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
  it('redacted boundaries render as "Access boundary (details unavailable)"', () => {
    const source = readFileSync(resolve(__dirname, './authorization-layer-stack.ts'), 'utf-8');

    // The source MUST contain the redacted placeholder text in a template
    expect(source).toContain('Access boundary (details unavailable)');
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
    expect(source).toMatch(/boundary-redacted.*Access boundary \(details unavailable\)/s);

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
    expect(source).toMatch(/\.map\(\(n\).*n \?\? ['"]access boundary \(details unavailable\)['"]/i);
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
