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
 * markdown — unit tests.
 *
 * Validates the shared markdown renderer, particularly the raw HTML
 * escaping fix for miller79/scion#10.
 */

import { describe, it, expect, beforeAll } from 'vitest';
import { getMarkdownRenderer, type MarkdownRenderer } from './markdown.js';

let renderer: MarkdownRenderer;

beforeAll(async () => {
  renderer = await getMarkdownRenderer();
});

// ---------------------------------------------------------------------------
// Raw HTML escaping (miller79/scion#10)
// ---------------------------------------------------------------------------

describe('markdown renderer: raw HTML escaping', () => {
  it('escapes <template> in prose so it is visible', () => {
    const result = renderer.render('Use -t <template> to begin');
    expect(result).toContain('&lt;template&gt;');
    expect(result).not.toContain('<template>');
  });

  it('preserves content after a <template> tag in prose', () => {
    const result = renderer.render(
      'First line\n\nUse -t <template> for the template flag.\n\nThird paragraph.'
    );
    expect(result).toContain('Third paragraph');
    expect(result).toContain('&lt;template&gt;');
  });

  it('escapes other HTML tags in prose', () => {
    const result = renderer.render('Use <div> and <span> carefully');
    expect(result).toContain('&lt;div&gt;');
    expect(result).toContain('&lt;span&gt;');
    expect(result).not.toContain('<div>');
    expect(result).not.toContain('<span>');
  });

  it('keeps <template> in inline code escaped (already handled by marked)', () => {
    const result = renderer.render('The `<template>` flag');
    expect(result).toContain('<code>');
    expect(result).toContain('&lt;template&gt;');
  });

  it('keeps <template> in fenced code blocks escaped (already handled by marked)', () => {
    const result = renderer.render(
      '```\nUse -t <template> here\n```'
    );
    expect(result).toContain('&lt;template&gt;');
  });
});

// ---------------------------------------------------------------------------
// General rendering
// ---------------------------------------------------------------------------

describe('markdown renderer: general rendering', () => {
  it('renders basic markdown to HTML', () => {
    const result = renderer.render('**bold** and *italic*');
    expect(result).toContain('<strong>bold</strong>');
    expect(result).toContain('<em>italic</em>');
  });
});
