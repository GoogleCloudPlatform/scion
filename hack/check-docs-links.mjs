#!/usr/bin/env node
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

// Checks internal links, #heading anchors, /scion/ base prefixes, and legacy
// redirect targets across docs-site/src/content/docs/ using Node built-ins
// only (zero node_modules, zero d2 binary dependency).
//
// Rules:
//   1. Internal page links must resolve to a docs-site page or static file in
//      docs-site/public/.
//   2. #heading anchors must match a heading slug (github-slugger compatible,
//      preserving underscores inside inline `code` spans), explicit {#id}, or
//      HTML id="..." on the target page. Bold text is not a heading.
//   3. Root-relative internal links must carry the /scion/ base prefix.
//   4. Links must not target deprecated legacy redirect paths listed in
//      legacyRedirectTargets in docs-site/astro.config.mjs.
//   5. Inline suppressions require a non-empty reason:
//        <!-- check-docs-links-ignore: <reason> -->   (Markdown)
//        {/* check-docs-links-ignore: <reason> */}    (MDX)
//      May appear on the same line or the immediately preceding line. Bare
//      suppressions without a reason are reported as violations.
//
// Limitations:
//   Does not check external https:// URLs, routes or anchors generated
//   dynamically by Astro/MDX components, or HTML rendering quirks (covered by
//   the full docs-site build in .github/workflows/docs.yml).
//
// Usage:
//   node hack/check-docs-links.mjs              scan docs-site/
//   node hack/check-docs-links.mjs --self-test  run built-in fixture tests
//
// Severity: FAIL-CLOSED ON EMPTY SCAN (see hack/LINT-CONVENTIONS.md)
//   0  analysed, no violations (or --self-test passed)
//   1  analysed, violations found (listed on stderr) or --self-test failed
//   3  could not analyse: required tool missing (e.g. node absent in wrapper)
//   4  could not analyse: scan root or astro.config.mjs missing, or 0 files matched
//   5  could not analyse: usage error (unknown argument)

import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const NAME = 'check-docs-links';
const REPO_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');

// GitHub / github-slugger compatible heading slugifier used by Astro/Starlight rehype-slug.
function createSlugger() {
  const seen = new Map();
  return function slug(rawHeading) {
    // Preserve literal underscores inside inline `code` spans while stripping emphasis markers outside code spans.
    const codeSpans = [];
    let text = rawHeading
      .replace(/\{#([A-Za-z0-9_-]+)\}\s*$/, '')
      .replace(/`([^`]*)`/g, (_, code) => {
        codeSpans.push(code);
        return `\u0000CODE${codeSpans.length - 1}\u0000`;
      })
      .replace(/!\[([^\]]*)\]\([^)]*\)/g, '$1')
      .replace(/\[([^\]]*)\]\([^)]*\)/g, '$1')
      .replace(/<[^>]+>/g, '')
      .replace(/(\*\*|__|[*~])/g, '')
      .replace(/(^|[\s(])_([^_]+)_(?=[\s).,;:!?]|$)/g, '$1$2')
      .replace(/\u0000CODE(\d+)\u0000/g, (_, idx) => codeSpans[Number(idx)])
      .trim()
      .toLowerCase();

    // github-slugger punctuation removal: keeps letters, marks, numbers, Connector_Punctuation (_), spaces, and hyphens.
    // HTML entities like &mdash; or literal —/– become empty string, so "Layer 1 — Operational" -> "layer-1--operational".
    text = text
      .replace(/&[a-zA-Z0-9#]+;/g, '')
      .replace(/[^\p{L}\p{M}\p{N}\p{Pc}\s-]/gu, '')
      .replace(/ /g, '-');

    const count = seen.get(text) ?? 0;
    seen.set(text, count + 1);
    return count === 0 ? text : `${text}-${count}`;
  };
}

function walkFiles(dir, exts) {
  const out = [];
  if (!fs.existsSync(dir)) return out;
  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    const full = path.join(dir, entry.name);
    if (entry.isDirectory()) out.push(...walkFiles(full, exts));
    else if (exts.some((e) => entry.name.endsWith(e))) out.push(full);
  }
  return out.sort();
}

function hasValidReason(reasonText) {
  return /[A-Za-z0-9]/.test(reasonText.trim());
}

// Extract and validate inline suppression directives on a single line (outside code spans/fences).
// Supports:
//   <!-- check-docs-links-ignore: <reason> -->
//   {/* check-docs-links-ignore: <reason> */}
//   # check-docs-links-ignore: <reason>  (frontmatter only)
function inspectSuppressions(line, lineNo, rel, inFrontmatter, suppressedLines, errors) {
  const withoutCode = line.replace(/`+[^`]*`+/g, '');
  const patterns = inFrontmatter
    ? [/#\s*check-docs-links-ignore\b([^\n]*)/g]
    : [
        /<!--\s*check-docs-links-ignore\b([\s\S]*?)-->/g,
        /\{\/\*\s*check-docs-links-ignore\b([\s\S]*?)\*\/\}/g,
      ];

  let foundValid = false;
  let stripped = withoutCode;

  for (const re of patterns) {
    for (const m of withoutCode.matchAll(re)) {
      const rawTail = (m[1] ?? '').trim();
      const reason = rawTail.replace(/^:\s*/, '').trim();
      if (!hasValidReason(reason)) {
        errors.push(
          `${rel}:${lineNo}: bare check-docs-links-ignore suppression requires a reason (use: <!-- check-docs-links-ignore: <reason> --> or {/* check-docs-links-ignore: <reason> */})`
        );
      } else {
        foundValid = true;
        suppressedLines.add(lineNo);
      }
      stripped = stripped.replace(m[0], '');
    }
  }

  // If the suppression comment was standalone on its own line, it applies to the immediately following line.
  if (foundValid && stripped.trim() === '') {
    suppressedLines.add(lineNo + 1);
  }
}

function stripDelimitedComments(line, openTok, closeTok, inComment) {
  if (inComment) {
    const endIdx = line.indexOf(closeTok);
    if (endIdx === -1) return { line: '', inComment: true };
    line = line.slice(endIdx + closeTok.length);
    inComment = false;
  }
  while (true) {
    const startIdx = line.indexOf(openTok);
    if (startIdx === -1) break;
    const endIdx = line.indexOf(closeTok, startIdx + openTok.length);
    if (endIdx === -1) {
      line = line.slice(0, startIdx);
      inComment = true;
      break;
    }
    line = line.slice(0, startIdx) + line.slice(endIdx + closeTok.length);
  }
  return { line, inComment };
}

function parseDocFile(filePath, docsRoot) {
  const raw = fs.readFileSync(filePath, 'utf8');
  const lines = raw.split(/\r?\n/);
  const rel = path.relative(docsRoot, filePath).replace(/\\/g, '/');

  let i = 0;
  let customSlug = null;
  const links = [];
  const suppressedLines = new Set();
  const parseErrors = [];

  // Parse YAML frontmatter if present.
  if (lines[0] === '---') {
    i = 1;
    while (i < lines.length && lines[i] !== '---') {
      const line = lines[i];
      const lineNo = i + 1;
      inspectSuppressions(line, lineNo, rel, true, suppressedLines, parseErrors);
      const slugMatch = line.match(/^slug:\s*['"]?([^'"#\s]+)['"]?/);
      if (slugMatch) customSlug = slugMatch[1].replace(/^\/+|\/+$/g, '');
      const linkMatch = line.match(/^\s*link:\s*['"]?([^'"#\s][^'"\s]*)['"]?/);
      if (linkMatch) {
        links.push({ line: lineNo, href: linkMatch[1], kind: 'frontmatter' });
      }
      i++;
    }
    if (i < lines.length && lines[i] === '---') i++;
  }

  let routeSlug = customSlug;
  if (routeSlug === null) {
    routeSlug = rel
      .replace(/\.(mdx|md)$/, '')
      .replace(/(^|\/)index$/, '');
  }
  const routePath = routeSlug === '' ? '/scion/' : `/scion/${routeSlug.replace(/\/+$/, '')}/`;

  const slugger = createSlugger();
  const anchors = new Set();

  let inFence = null; // { char: '`' | '~', len: number }
  let inHtmlComment = false;
  let inJsxComment = false;

  for (; i < lines.length; i++) {
    let line = lines[i];
    const lineNo = i + 1;

    const fenceMatch = !inHtmlComment && !inJsxComment ? line.match(/^\s*(`{3,}|~{3,})/) : null;
    if (fenceMatch) {
      const marker = fenceMatch[1];
      if (!inFence) {
        inFence = { char: marker[0], len: marker.length };
        continue;
      } else if (marker[0] === inFence.char && marker.length >= inFence.len && line.trim() === marker) {
        inFence = null;
        continue;
      }
    }
    if (inFence) continue;

    // Inspect inline suppressions before stripping comments.
    inspectSuppressions(line, lineNo, rel, false, suppressedLines, parseErrors);

    // Strip HTML comments <!-- ... --> and MDX/JSX comments {/* ... */} outside fences.
    ({ line, inComment: inHtmlComment } = stripDelimitedComments(line, '<!--', '-->', inHtmlComment));
    ({ line, inComment: inJsxComment } = stripDelimitedComments(line, '{/*', '*/}', inJsxComment));
    if (line === '') continue;

    // ATX Headings (# .. ######)
    const headingMatch = line.match(/^\s{0,3}(#{1,6})\s+(.+?)\s*#*\s*$/);
    if (headingMatch) {
      const rawText = headingMatch[2];
      const explicitId = rawText.match(/\{#([A-Za-z0-9_-]+)\}\s*$/);
      if (explicitId) anchors.add(explicitId[1]);
      else anchors.add(slugger(rawText));
    }
    // Explicit HTML id="..." anchors in MDX/HTML
    for (const idMatch of line.matchAll(/\bid=["']([A-Za-z0-9_-]+)["']/g)) {
      anchors.add(idMatch[1]);
    }

    // Strip inline code spans before scanning links
    const stripped = line.replace(/`+[^`]*`+/g, '');

    // Markdown links [text](href "optional title") and images ![alt](src)
    for (const m of stripped.matchAll(/!?\[[^\]]*\]\(([^)\s]+)(?:\s+["'][^"']*["'])?\)/g)) {
      links.push({ line: lineNo, href: m[1], kind: 'markdown' });
    }
    // JSX/HTML href="..."
    for (const m of stripped.matchAll(/\bhref=["']([^"']+)["']/g)) {
      links.push({ line: lineNo, href: m[1], kind: 'jsx-href' });
    }
  }

  return { filePath, rel, routePath, anchors, links, suppressedLines, parseErrors };
}

function parseAstroRedirects(astroConfigPath) {
  const redirects = new Map();
  const src = fs.readFileSync(astroConfigPath, 'utf8');
  const blockMatch = src.match(/legacyRedirectTargets\s*=\s*\{([\s\S]*?)\};/);
  if (blockMatch) {
    for (const m of blockMatch[1].matchAll(/['"](\/[^'"]+)['"]\s*:\s*['"](\/[^'"]+)['"]/g)) {
      const from = m[1].replace(/\/+$/, '');
      const to = m[2];
      redirects.set(`/scion${from}/`, to);
      redirects.set(`/scion${from}`, to);
    }
  }
  return redirects;
}

function checkDocsTree({ docsRoot, publicRoot, astroConfigPath }) {
  if (!fs.existsSync(docsRoot) || !fs.statSync(docsRoot).isDirectory()) {
    return { scanError: `docs root directory not found: ${docsRoot}` };
  }
  if (!fs.existsSync(astroConfigPath) || !fs.statSync(astroConfigPath).isFile()) {
    return { scanError: `astro config file not found: ${astroConfigPath}` };
  }

  const files = walkFiles(docsRoot, ['.md', '.mdx']);
  if (files.length === 0) {
    return { scanError: `no .md or .mdx files matched under ${docsRoot}` };
  }

  const pages = files.map((f) => parseDocFile(f, docsRoot));
  const routeMap = new Map();
  for (const p of pages) {
    routeMap.set(p.routePath, p);
    routeMap.set(p.routePath.replace(/\/$/, ''), p);
  }
  const redirects = parseAstroRedirects(astroConfigPath);
  const publicFiles = new Set();
  for (const pf of walkFiles(publicRoot, [''])) {
    const rel = '/' + path.relative(publicRoot, pf).replace(/\\/g, '/');
    publicFiles.add(`/scion${rel}`);
  }

  const errors = [];
  let checkedLinks = 0;
  let checkedAnchors = 0;
  let suppressedCount = 0;

  for (const page of pages) {
    errors.push(...page.parseErrors);

    for (const link of page.links) {
      const { href, line } = link;
      if (
        href.startsWith('http://') ||
        href.startsWith('https://') ||
        href.startsWith('mailto:') ||
        href.startsWith('tel:') ||
        href.startsWith('scion://') ||
        href.startsWith('data:') ||
        href.startsWith('javascript:') ||
        href.startsWith('//')
      ) {
        continue;
      }

      checkedLinks++;
      const recordViolation = (msg) => {
        if (page.suppressedLines.has(line)) {
          suppressedCount++;
        } else {
          errors.push(msg);
        }
      };

      let targetPath = page.routePath;
      let frag = '';

      if (href.startsWith('#')) {
        frag = href.slice(1);
      } else {
        const hashIdx = href.indexOf('#');
        const rawPath = (hashIdx === -1 ? href : href.slice(0, hashIdx)).split('?')[0];
        frag = hashIdx === -1 ? '' : href.slice(hashIdx + 1);

        if (rawPath.startsWith('/')) {
          if (!rawPath.startsWith('/scion/') && rawPath !== '/scion') {
            recordViolation(`${page.rel}:${line}: internal root link "${href}" missing /scion/ base prefix`);
            continue;
          }
          targetPath = rawPath;
        } else {
          // Resolve relative to current route directory.
          const resolved = path.posix.resolve(page.routePath, rawPath);
          targetPath = rawPath.endsWith('/') ? `${resolved}/` : resolved;
        }
      }

      const normTarget = targetPath.endsWith('/') ? targetPath : `${targetPath}/`;
      const targetPage = routeMap.get(normTarget) || routeMap.get(targetPath);

      if (!targetPage) {
        const redirectCanonical = redirects.get(normTarget) || redirects.get(targetPath);
        if (redirectCanonical) {
          recordViolation(
            `${page.rel}:${line}: link "${href}" targets legacy redirect "${normTarget}" (use "/scion${redirectCanonical}" instead)`
          );
          continue;
        }
        if (publicFiles.has(targetPath)) {
          continue;
        }
        recordViolation(`${page.rel}:${line}: broken internal route "${href}" (resolved "${normTarget}")`);
        continue;
      }

      if (frag) {
        checkedAnchors++;
        let decoded;
        try {
          decoded = decodeURIComponent(frag);
        } catch {
          recordViolation(
            `${page.rel}:${line}: malformed anchor "#${frag}" on target "${targetPage.rel}" (from "${href}")`
          );
          continue;
        }
        if (!targetPage.anchors.has(decoded)) {
          recordViolation(
            `${page.rel}:${line}: broken anchor "#${frag}" on target "${targetPage.rel}" (from "${href}")`
          );
        }
      }
    }
  }

  return {
    pageCount: pages.length,
    checkedLinks,
    checkedAnchors,
    suppressedCount,
    errors,
  };
}

function getProvenance(repoRoot) {
  try {
    const sha = execFileSync('git', ['rev-parse', '--short', 'HEAD'], {
      cwd: repoRoot,
      encoding: 'utf8',
      stdio: ['ignore', 'pipe', 'ignore'],
    }).trim();
    if (!sha) return 'unknown';
    const status = execFileSync('git', ['status', '--porcelain'], {
      cwd: repoRoot,
      encoding: 'utf8',
      stdio: ['ignore', 'pipe', 'ignore'],
    }).trim();
    return status ? `${sha}-dirty` : sha;
  } catch {
    return 'unknown';
  }
}

function runSelfTest() {
  const tmpRoot = fs.mkdtempSync(path.join(os.tmpdir(), 'check-docs-links-selftest-'));
  try {
    const docs = path.join(tmpRoot, 'src/content/docs');
    const pub = path.join(tmpRoot, 'public');
    fs.mkdirSync(path.join(docs, 'hosted'), { recursive: true });
    fs.mkdirSync(pub, { recursive: true });

    const astroCfg = path.join(tmpRoot, 'astro.config.mjs');
    fs.writeFileSync(
      astroCfg,
      `const legacyRedirectTargets = {\n  '/old-route': '/hosted/server-config/',\n};\n`
    );
    fs.writeFileSync(path.join(pub, 'diagram.svg'), '<svg></svg>\n');

    // 1. Tricky-negative (passing) fixture:
    //    - frontmatter hero link to duplicate heading (-1)
    //    - inline-code heading with underscores (`server.project_storage`)
    //    - em-dash heading ("Layer 1 — Operational")
    //    - explicit {#custom-id} and HTML id="..." anchors
    //    - static file in public/
    //    - d2 code fence, inline code, HTML comment, and JSX comment exclusions
    //    - valid reason-required suppressions (same-line HTML + preceding-line JSX)
    fs.writeFileSync(
      path.join(docs, 'index.mdx'),
      [
        '---',
        'title: Home',
        'slug: overview',
        'hero:',
        '  actions:',
        '    - text: Config',
        '      link: /scion/hosted/server-config/#examples-1',
        '---',
        '',
        '# Home',
        '',
        'See [Server Config](/scion/hosted/server-config/#examples),',
        '[Storage](/scion/hosted/server-config/#project-storage-serverproject_storage),',
        '[Layer 1](/scion/hosted/server-config/#layer-1--operational),',
        '[Custom](/scion/hosted/server-config/#explicit-anchor),',
        '[HTML](/scion/hosted/server-config/#html-anchor),',
        '[Diagram](/scion/diagram.svg),',
        'and [External](https://example.com/nope/#bad).',
        '',
        'Inline `[ignore](/scion/does-not-exist/#nope)` and `<!-- check-docs-links-ignore -->` are ignored.',
        '<!-- [commented-out](/scion/does-not-exist/) -->',
        '{/* [jsx-commented-out](/scion/does-not-exist/) */}',
        '',
        'Suppressed inline: [Dynamic](/scion/dynamic-route/) <!-- check-docs-links-ignore: generated by Astro integration -->',
        '{/* check-docs-links-ignore: intentional documentation of legacy redirect */}',
        'Suppressed next-line: [Legacy](/scion/old-route/)',
        '',
        '```d2',
        'x: "[fake](/scion/does-not-exist/#nope)"',
        '```',
        '',
      ].join('\n')
    );

    fs.writeFileSync(
      path.join(docs, 'hosted/server-config.md'),
      [
        '---',
        'title: Server Config',
        '---',
        '',
        '## Examples',
        'First.',
        '',
        '## Examples',
        'Second.',
        '',
        '### Project storage (`server.project_storage`)',
        'Preserves underscore inside inline code.',
        '',
        '## Layer 1 — Operational',
        'Em-dash becomes double hyphen.',
        '',
        '## Custom Heading {#explicit-anchor}',
        '<div id="html-anchor">HTML anchor</div>',
        '',
        '**Roles, role bindings, and access constraints**',
        'Bold line, not a heading.',
        '',
      ].join('\n')
    );

    const passRes = checkDocsTree({ docsRoot: docs, publicRoot: pub, astroConfigPath: astroCfg });
    assert.equal(passRes.scanError, undefined, `Unexpected scanError: ${passRes.scanError}`);
    assert.equal(
      passRes.errors.length,
      0,
      `Expected passing fixture 0 errors, got: ${passRes.errors.join('; ')}`
    );
    assert.equal(passRes.suppressedCount, 2, `Expected 2 suppressed links, got ${passRes.suppressedCount}`);

    // 2. Positive (failing) fixture covering every rule:
    //    - broken internal route
    //    - broken #heading anchor
    //    - bold text linked as #heading
    //    - internal root link missing /scion/ base prefix
    //    - legacy redirect target (must FAIL, not pass)
    //    - bare HTML suppression without a reason
    //    - bare JSX suppression without a reason
    fs.appendFileSync(
      path.join(docs, 'index.mdx'),
      [
        '',
        'Broken route: [Missing](/scion/no-such-page/)',
        'Broken anchor: [Base URL](/scion/hosted/server-config/#web-base-url)',
        'Bold-not-heading: [Roles](/scion/hosted/server-config/#roles-role-bindings-and-access-constraints)',
        'Missing prefix: [No Base](/hosted/server-config/)',
        'Legacy redirect: [Old Route](/scion/old-route/)',
        'Bare HTML ignore: [SuppressedBad](/scion/missing-1/) <!-- check-docs-links-ignore -->',
        'Bare JSX ignore: [SuppressedBad2](/scion/missing-2/) {/* check-docs-links-ignore:   */}',
        '',
      ].join('\n')
    );

    const failRes = checkDocsTree({ docsRoot: docs, publicRoot: pub, astroConfigPath: astroCfg });
    assert.equal(failRes.scanError, undefined, `Unexpected scanError: ${failRes.scanError}`);
    // 5 link violations + 2 bare suppression violations + 2 unsuppressed broken links on those bare-suppression lines = 9
    assert.equal(
      failRes.errors.length,
      9,
      `Expected 9 errors in failing fixture, got ${failRes.errors.length}:\n${failRes.errors.join('\n')}`
    );
    assert.ok(failRes.errors.some((e) => e.includes('broken internal route "/scion/no-such-page/"')));
    assert.ok(failRes.errors.some((e) => e.includes('broken anchor "#web-base-url"')));
    assert.ok(failRes.errors.some((e) => e.includes('#roles-role-bindings-and-access-constraints')));
    assert.ok(failRes.errors.some((e) => e.includes('missing /scion/ base prefix')));
    assert.ok(
      failRes.errors.some(
        (e) =>
          e.includes('targets legacy redirect "/scion/old-route/"') &&
          e.includes('/scion/hosted/server-config/')
      ),
      `Expected legacy redirect violation with canonical target suggestion, got:\n${failRes.errors.join('\n')}`
    );
    assert.equal(
      failRes.errors.filter((e) => e.includes('bare check-docs-links-ignore suppression requires a reason')).length,
      2,
      'Expected 2 bare suppression violations'
    );

    // 3. Empty / missing scan root checks (exit 4 contract)
    const missingDocs = checkDocsTree({
      docsRoot: path.join(tmpRoot, 'does-not-exist'),
      publicRoot: pub,
      astroConfigPath: astroCfg,
    });
    assert.ok(missingDocs.scanError, 'Expected scanError when docsRoot is missing');

    const emptyDocsDir = path.join(tmpRoot, 'empty-docs');
    fs.mkdirSync(emptyDocsDir, { recursive: true });
    const emptyDocs = checkDocsTree({
      docsRoot: emptyDocsDir,
      publicRoot: pub,
      astroConfigPath: astroCfg,
    });
    assert.ok(emptyDocs.scanError, 'Expected scanError when 0 files matched');

    const missingConfig = checkDocsTree({
      docsRoot: docs,
      publicRoot: pub,
      astroConfigPath: path.join(tmpRoot, 'missing-astro.config.mjs'),
    });
    assert.ok(missingConfig.scanError, 'Expected scanError when astro.config.mjs is missing');

    console.error(
      `${NAME}: self-test PASS (valid routes/anchors/inline-code underscores/suppressions, broken route/anchor/bold/prefix/legacy-redirect/bare-suppression, and empty/missing root exit-4 checks verified)`
    );
  } finally {
    fs.rmSync(tmpRoot, { recursive: true, force: true });
  }
}

function main() {
  const args = process.argv.slice(2);
  if (args.length > 1) {
    console.error(`${NAME}: usage: node hack/check-docs-links.mjs [--self-test]`);
    process.exit(5);
  }
  if (args.length === 1) {
    if (args[0] === '--help' || args[0] === '-h') {
      console.log(`Usage: node hack/check-docs-links.mjs [--self-test]`);
      process.exit(0);
    }
    if (args[0] === '--self-test') {
      try {
        runSelfTest();
        process.exit(0);
      } catch (err) {
        console.error(`${NAME}: self-test FAIL: ${err instanceof Error ? err.message : err}`);
        process.exit(1);
      }
    }
    console.error(`${NAME}: unknown argument: ${args[0]}`);
    process.exit(5);
  }

  const docsRoot = path.join(REPO_ROOT, 'docs-site/src/content/docs');
  const publicRoot = path.join(REPO_ROOT, 'docs-site/public');
  const astroConfigPath = path.join(REPO_ROOT, 'docs-site/astro.config.mjs');

  const sha = getProvenance(REPO_ROOT);
  const res = checkDocsTree({ docsRoot, publicRoot, astroConfigPath });

  if (res.scanError) {
    console.error(`${NAME}: analysed ${sha}: COULD NOT ANALYSE: ${res.scanError}`);
    process.exit(4);
  }

  for (const err of res.errors) {
    console.error(`docs-site/src/content/docs/${err}`);
  }
  console.error(
    `${NAME}: analysed ${sha}, ${res.pageCount} pages, ${res.checkedLinks} internal links, ${res.checkedAnchors} anchors, ${res.suppressedCount} suppressed, ${res.errors.length} violation(s)`
  );

  process.exit(res.errors.length > 0 ? 1 : 0);
}

main();
