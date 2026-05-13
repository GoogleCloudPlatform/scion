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
 * Copies only the Shoelace icons used by the app into public/shoelace/assets/icons/.
 *
 * This avoids bundling the full 2000+ icon set (8MB+) while ensuring icons
 * load correctly when served by the Go server (which doesn't serve node_modules).
 *
 * To add a new icon: add its Bootstrap Icons name to the USED_ICONS array below,
 * then run `npm run copy:shoelace-icons`.
 */

import { cpSync, mkdirSync, existsSync } from 'fs';
import { resolve, dirname } from 'path';
import { fileURLToPath } from 'url';

const __dirname = dirname(fileURLToPath(import.meta.url));
const ROOT = resolve(__dirname, '..');

const SRC = resolve(ROOT, 'node_modules/@shoelace-style/shoelace/dist/assets/icons');
const SRC_FALLBACK = resolve(ROOT, 'node_modules/bootstrap-icons/icons');
const DEST = resolve(ROOT, 'public/shoelace/assets/icons');

/** Icons referenced via <sl-icon name="..."> across the app. */
const USED_ICONS = [
  'arrow-clockwise',
  'arrow-counterclockwise',
  'bell-slash',
  'arrow-down-circle',
  'arrow-left',
  'arrow-right',
  'arrow-left-circle',
  'arrow-repeat',
  'bell',
  'box-arrow-in-right',
  'box-arrow-right',
  'box-arrow-up-right',
  'broadcast-pin',
  'chevron-down',
  'chevron-left',
  'chevron-right',
  'chat-dots',
  'check-circle',
  'check-circle-fill',
  'check-lg',
  'circle',
  'circle-fill',
  'clipboard',
  'clipboard-check',
  'clock',
  'clock-history',
  'code',
  'code-square',
  'cpu',
  'diagram-3',
  'download',
  'emoji-frown',
  'envelope',
  'envelope-open',
  'exclamation-circle-fill',
  'exclamation-octagon',
  'exclamation-triangle',
  'eye',
  'eye-slash',
  'file-earmark',
  'file-earmark-code',
  'file-earmark-zip',
  'file-text',
  'floppy',
  'folder',
  'folder-fill',
  'folder-plus',
  'folder2-open',
  'funnel',
  'gear',
  'github',
  'grid-3x3-gap',
  'google',
  'hdd-rack',
  'house',
  'hourglass-bottom',
  'hourglass-split',
  'info-circle',
  'info-circle-fill',
  'bell-fill',
  'key',
  'lightning-charge',
  'link-45deg',
  'list',
  'list-ul',
  'moon',
  'pencil',
  'people',
  'person',
  'person-circle',
  'person-plus',
  'play-circle',
  'plus-circle',
  'plus-lg',
  'question-circle',
  'robot',
  'pause-circle',
  'search',
  'send',
  'shield-check',
  'shield-lock',
  'shield-x',
  'slash-circle',
  'sliders',
  'star',
  'star-fill',
  'stop-circle',
  'sun',
  'terminal',
  'three-dots-vertical',
  'trash',
  'upload',
  'wifi-off',
  'wrench-adjustable',
  'x-circle',
  'x-circle-fill',
  'x-lg',
];

mkdirSync(DEST, { recursive: true });

let copied = 0;
let missing = 0;

for (const name of USED_ICONS) {
  const src = resolve(SRC, `${name}.svg`);
  const srcFallback = resolve(SRC_FALLBACK, `${name}.svg`);
  const dest = resolve(DEST, `${name}.svg`);
  if (existsSync(src)) {
    cpSync(src, dest);
    copied++;
  } else if (existsSync(srcFallback)) {
    cpSync(srcFallback, dest);
    copied++;
  } else {
    console.warn(`  warning: icon "${name}.svg" not found in Shoelace or Bootstrap Icons`);
    missing++;
  }
}

console.log(`Shoelace icons: ${copied} copied${missing ? `, ${missing} missing` : ''}`);
