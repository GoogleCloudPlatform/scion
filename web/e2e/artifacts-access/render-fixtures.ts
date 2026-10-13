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
 * Fixtures of the artifact render journey: an HTML bundle in three
 * versions and a markdown bundle with raw HTML and script. They are written
 * at run time into a private directory; nothing binary is committed (the
 * PNGs are built here from bytes).
 *
 * The bundle's index.html carries a harmless isolation probe: it only
 * reads, attempts and writes its own #probe element, as JSON of outcome
 * labels (never a value read from anywhere).
 */

import * as fs from 'node:fs';
import * as path from 'node:path';
import * as zlib from 'node:zlib';

/** Paths the frame probes would navigate or open; none may be requested. */
export const PROBE_PATHS = {
  topNavScript: '/r2-top-nav-script',
  topNavLink: '/r2-top-nav-link',
  popup: '/r2-popup-probe',
} as const;

/** The hub API path the frame's fetch and its #api-nav link try. */
export const API_PROBE_PATH = '/api/v1/artifacts';

/** Remote image host of the bundle (blocked by the view CSP before any fetch). */
export const REMOTE_IMAGE_HOST = 'remote-image.invalid';

/** Pixel sizes of the fixture images: a load is proven by naturalWidth. */
export const CHART_SIZE = { width: 7, height: 5 } as const;
export const DOT_SIZE = { width: 3, height: 2 } as const;

/** Expected CSS effect of css/site.css on #css-probe. */
export const CSS_PROBE = { width: '123px', color: 'rgb(0, 128, 0)' } as const;

const CRC_TABLE = (() => {
  const table = new Uint32Array(256);
  for (let n = 0; n < 256; n++) {
    let c = n;
    for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
    table[n] = c >>> 0;
  }
  return table;
})();

function crc32(buf: Buffer): number {
  let c = 0xffffffff;
  for (const b of buf) c = CRC_TABLE[(c ^ b) & 0xff] ^ (c >>> 8);
  return (c ^ 0xffffffff) >>> 0;
}

function pngChunk(type: string, data: Buffer): Buffer {
  const len = Buffer.alloc(4);
  len.writeUInt32BE(data.length);
  const body = Buffer.concat([Buffer.from(type, 'ascii'), data]);
  const crc = Buffer.alloc(4);
  crc.writeUInt32BE(crc32(body));
  return Buffer.concat([len, body, crc]);
}

/** A solid-colour 8-bit RGB PNG of width x height. */
export function solidPng(width: number, height: number, rgb: [number, number, number]): Buffer {
  const ihdr = Buffer.alloc(13);
  ihdr.writeUInt32BE(width, 0);
  ihdr.writeUInt32BE(height, 4);
  ihdr[8] = 8; // bit depth
  ihdr[9] = 2; // colour type RGB
  ihdr[10] = 0;
  ihdr[11] = 0;
  ihdr[12] = 0;
  const rowLen = 1 + width * 3;
  const raw = Buffer.alloc(rowLen * height);
  for (let y = 0; y < height; y++) {
    raw[y * rowLen] = 0; // filter: none
    for (let x = 0; x < width; x++) {
      raw.set(rgb, y * rowLen + 1 + x * 3);
    }
  }
  return Buffer.concat([
    Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
    pngChunk('IHDR', ihdr),
    pngChunk('IDAT', zlib.deflateSync(raw)),
    pngChunk('IEND', Buffer.alloc(0)),
  ]);
}

/** The probe script of index.html (inline; the view CSP allows inline script). */
const PROBE_SCRIPT = `
(function () {
  var r = {};
  var out = document.getElementById('probe');
  function name(e) { return 'blocked:' + ((e && e.name) || 'Error'); }
  function write() { r.done = true; out.textContent = JSON.stringify(r); }
  r.frame = window.parent !== window ? 'framed' : 'top-level';
  r.origin = String(self.origin);
  if (r.frame === 'framed') {
    try { void parent.document.title; r.parentDom = 'readable'; } catch (e) { r.parentDom = name(e); }
    try { parent.__r2probe = 1; r.parentWrite = parent.__r2probe === 1 ? 'written' : 'not-written'; }
    catch (e) { r.parentWrite = name(e); }
  } else {
    r.parentDom = 'n/a';
    r.parentWrite = 'n/a';
  }
  try { r.cookie = document.cookie === '' ? 'empty' : 'readable'; } catch (e) { r.cookie = name(e); }
  try { void window.localStorage.length; r.storage = 'readable'; } catch (e) { r.storage = name(e); }
  try {
    var w = window.open('${PROBE_PATHS.popup}');
    r.popup = w ? 'opened' : 'blocked:null';
  } catch (e) { r.popup = name(e); }
  if (r.frame === 'framed') {
    try { top.location.href = '${PROBE_PATHS.topNavScript}'; r.topNav = 'no-throw'; }
    catch (e) { r.topNav = name(e); }
  } else {
    r.topNav = 'n/a';
  }
  var settled = false;
  function settle(v) { if (settled) return; settled = true; r.fetch = v; write(); }
  try {
    fetch('${API_PROBE_PATH}?mine=1', { credentials: 'include' }).then(
      function (res) { settle('status:' + res.status); },
      function (e) { settle(name(e)); }
    );
  } catch (e) { settle(name(e)); }
  setTimeout(function () { settle('timeout'); }, 5000);
})();
`;

function indexHtml(n: number): string {
  return `<!doctype html>
<html><head><meta charset="utf-8"><title>R2 bundle v${n}</title>
<link rel="stylesheet" href="css/site.css"></head>
<body>
<h1 id="ver">R2 bundle v${n}</h1>
<div id="css-probe">styled</div>
<p><img id="chart" src="img/chart.png" alt="chart"></p>
<p><img id="remote" src="https://${REMOTE_IMAGE_HOST}/r2.png" alt="remote"></p>
<p><a id="to-page2" href="page2.html">page two</a></p>
<p><a id="api-nav" href="${API_PROBE_PATH}?mine=1">api</a></p>
<p><a id="top-nav" href="${PROBE_PATHS.topNavLink}" target="_top">top</a></p>
<pre id="probe">pending</pre>
<script>${PROBE_SCRIPT}</script>
</body></html>
`;
}

function page2Html(n: number): string {
  return `<!doctype html>
<html><head><meta charset="utf-8"><title>R2 page two v${n}</title>
<link rel="stylesheet" href="css/site.css"></head>
<body>
<p id="page2">R2 page two v${n}</p>
<p><img id="chart2" src="img/chart.png" alt="chart"></p>
<p><a id="back" href="index.html">back</a></p>
</body></html>
`;
}

const SITE_CSS = `#css-probe { width: ${CSS_PROBE.width}; color: ${CSS_PROBE.color}; }\n`;

/** Files of bundle version n (1..3), by bundle path. */
export function bundleFiles(n: number): Record<string, Buffer> {
  const files: Record<string, Buffer> = {
    'index.html': Buffer.from(indexHtml(n)),
    'page2.html': Buffer.from(page2Html(n)),
    'css/site.css': Buffer.from(SITE_CSS),
    'img/chart.png': solidPng(CHART_SIZE.width, CHART_SIZE.height, [30, 120, 200]),
  };
  if (n === 3) files['notes/v3.txt'] = Buffer.from('Only in v3.\n');
  return files;
}

/** The markdown bundle's entry: renderable content plus active content that must stay inert. */
export const MARKDOWN_INDEX = [
  '# R2 markdown',
  '',
  'Plain text marker r2-md-ok.',
  '',
  '![dot](img/dot.png)',
  '',
  '[notes](notes.md)',
  '',
  "<script>parent.__r2md='script'</script>",
  '',
  `<img src="img/dot.png" onerror="parent.__r2md='onerror'" onload="parent.__r2md='onload'">`,
  '',
  `<a id="jslink" href="javascript:parent.__r2md='js'">js link</a>`,
  '',
  `<iframe srcdoc="<script>parent.__r2md='iframe'</script>"></iframe>`,
  '',
  `<svg onload="parent.__r2md='svg'"><circle r="1"/></svg>`,
  '',
  `<div onmouseover="parent.__r2md='hover'">hover marker</div>`,
  '',
  '<form action="/api/v1/artifacts"><button>go</button></form>',
  '',
  '<style>body{background:red}</style>',
  '',
].join('\n');

/** Files of the markdown bundle, by bundle path. */
export function markdownFiles(): Record<string, Buffer> {
  return {
    'index.md': Buffer.from(MARKDOWN_INDEX),
    'img/dot.png': solidPng(DOT_SIZE.width, DOT_SIZE.height, [200, 40, 40]),
    'notes.md': Buffer.from('# Notes\n\nThe notes page of the markdown bundle.\n'),
  };
}

/**
 * Writes files under root/folder and returns the folder path. The folder
 * name becomes the publish dialog's folder (its first path segment).
 */
export function writeFolder(root: string, folder: string, files: Record<string, Buffer>): string {
  const dir = path.join(root, folder);
  for (const [rel, data] of Object.entries(files)) {
    const file = path.join(dir, ...rel.split('/'));
    fs.mkdirSync(path.dirname(file), { recursive: true, mode: 0o700 });
    fs.writeFileSync(file, data, { mode: 0o600 });
  }
  return dir;
}
