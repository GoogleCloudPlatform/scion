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
 * Precompresses the client build so the hub can serve br/gzip without
 * compressing per request (pkg/webassets pairs each file with its siblings).
 *
 * Usage: node scripts/precompress.mjs <dir>   (npm run build passes dist/client)
 *
 * For every file under <dir> whose extension is in COMPRESSIBLE, that is at
 * least MIN_SIZE bytes and is not a source map, writes file.br (brotli,
 * quality 11) and file.gz (gzip -9, header mtime 0, so builds are
 * reproducible). A variant is kept only if it is smaller than KEEP_RATIO of
 * the raw size; otherwise any existing sibling of that kind is removed so a
 * stale one can never be served.
 */

import { readdirSync, readFileSync, writeFileSync, rmSync, statSync } from 'node:fs';
import { join, extname, relative } from 'node:path';
import { brotliCompressSync, gzipSync, constants } from 'node:zlib';
import { fileURLToPath } from 'node:url';

export const COMPRESSIBLE = new Set(['.js', '.mjs', '.css', '.svg', '.json', '.webmanifest', '.html', '.txt']);
export const MIN_SIZE = 1024;
export const KEEP_RATIO = 0.95;

/** True if path names a file this script compresses (by name only). */
export function isCandidateName(path) {
  if (path.endsWith('.br') || path.endsWith('.gz') || path.endsWith('.map')) return false;
  return COMPRESSIBLE.has(extname(path).toLowerCase());
}

export function brotli(buf) {
  return brotliCompressSync(buf, {
    params: {
      [constants.BROTLI_PARAM_QUALITY]: constants.BROTLI_MAX_QUALITY,
      [constants.BROTLI_PARAM_MODE]: constants.BROTLI_MODE_TEXT,
      [constants.BROTLI_PARAM_SIZE_HINT]: buf.length,
    },
  });
}

export function gzip(buf) {
  // Node writes a zero MTIME in the gzip header, so output is deterministic.
  return gzipSync(buf, { level: constants.Z_BEST_COMPRESSION });
}

function* walk(dir) {
  for (const ent of readdirSync(dir, { withFileTypes: true })) {
    const p = join(dir, ent.name);
    if (ent.isDirectory()) yield* walk(p);
    else if (ent.isFile()) yield p;
  }
}

/**
 * Precompresses every candidate file under dir. Returns a summary:
 * { files, br, gz, rawBytes, brBytes, gzBytes }.
 */
export function precompressDir(dir) {
  const summary = { files: 0, br: 0, gz: 0, rawBytes: 0, brBytes: 0, gzBytes: 0 };
  // Collect first so the variants written below are not walked.
  const candidates = [...walk(dir)].filter(isCandidateName);
  for (const file of candidates) {
    const size = statSync(file).size;
    if (size < MIN_SIZE) {
      rmSync(`${file}.br`, { force: true });
      rmSync(`${file}.gz`, { force: true });
      continue;
    }
    const raw = readFileSync(file);
    summary.files++;
    summary.rawBytes += raw.length;
    for (const [ext, compress, key] of [['.br', brotli, 'br'], ['.gz', gzip, 'gz']]) {
      const out = compress(raw);
      if (out.length < raw.length * KEEP_RATIO) {
        writeFileSync(file + ext, out);
        summary[key]++;
        summary[`${key}Bytes`] += out.length;
      } else {
        rmSync(file + ext, { force: true });
      }
    }
  }
  return summary;
}

if (process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]) {
  const dir = process.argv[2];
  if (!dir) {
    console.error('usage: node scripts/precompress.mjs <dir>');
    process.exit(2);
  }
  const start = Date.now();
  const s = precompressDir(dir);
  const kb = (n) => `${Math.round(n / 1024)} KB`;
  console.log(
    `precompress: ${relative(process.cwd(), dir) || dir}: ${s.files} files, ${kb(s.rawBytes)} raw -> ` +
      `${s.br} br (${kb(s.brBytes)}), ${s.gz} gz (${kb(s.gzBytes)}) in ${Date.now() - start} ms`,
  );
}
