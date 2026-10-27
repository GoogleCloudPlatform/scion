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
 * Paste and drop in the review editor.
 *
 * The editor takes over every paste: it reads the clipboard's Markdown or
 * plain text (never its HTML), reads it with the editor's own Markdown
 * parser, and inserts it as one edit, which the tracker records as a
 * suggestion. Copies from the editor itself paste the same way, from the
 * text the editor put on the clipboard. Pasted HTML that does reach
 * ProseMirror's own paste path is emptied first. Dropping files or content
 * is refused; image files get the hint that images are added by
 * publishing a new version.
 */

import { Fragment, Slice } from '@milkdown/prose/model';
import type { Node } from '@milkdown/prose/model';
import { Plugin, PluginKey } from '@milkdown/prose/state';
import type { EditorView } from '@milkdown/prose/view';

import { parseCritic } from '../../../utils/critic.js';
import type { HintKind } from './policy.js';
import { CRITIC_MARK_NAMES } from './schema.js';

export const pasteKey = new PluginKey('criticPaste');

function hasImageFile(data: DataTransfer | null): boolean {
  if (!data) return false;
  for (const item of Array.from(data.items ?? [])) {
    if (item.kind === 'file' && item.type.startsWith('image/')) return true;
  }
  for (const f of Array.from(data.files ?? [])) if (f.type.startsWith('image/')) return true;
  return false;
}

/** The clipboard text the editor reads: Markdown if offered, else plain text. */
export function clipboardText(data: DataTransfer | null): string {
  if (!data) return '';
  const md = data.getData('text/markdown');
  return (md || data.getData('text/plain') || '').replace(/\r\n?/g, '\n');
}

/**
 * The inline content of pasted text, read as Markdown, or a refusal hint:
 * text of more than one paragraph (the paste-multi hint), or text that is
 * not a paragraph (a heading, list, table or code; the generic hint), is
 * not pasted in this version; neither is
 * text carrying CriticMarkup, which would be read as marks.
 */
export function pasteContent(
  text: string,
  parse: (md: string) => Node
): { content: Fragment } | { refuse: HintKind } {
  const trimmed = text.replace(/^\n+|\n+$/g, '');
  if (trimmed === '') return { content: Fragment.empty };
  if (parseCritic(trimmed).some((s) => s.kind !== 'text')) return { refuse: 'generic' };
  const doc = parse(trimmed);
  if (doc.childCount !== 1) return { refuse: 'paste-multi' };
  const block = doc.firstChild!;
  // A single heading, list, table or code block is not paragraph text.
  if (block.type.name !== 'paragraph') return { refuse: 'generic' };
  const nodes: Node[] = [];
  let ok = true;
  block.forEach((n) => {
    if (n.type.name === 'html') ok = false;
    if (n.marks.some((m) => CRITIC_MARK_NAMES.has(m.type.name))) ok = false;
    nodes.push(n);
  });
  if (!ok) return { refuse: 'generic' };
  return { content: Fragment.from(nodes) };
}

/** Inserts what a paste carries (its text only), or refuses it with a hint. */
export function pasteInto(
  view: EditorView,
  data: DataTransfer | null,
  parse: (md: string) => Node,
  onRefuse: (hint: HintKind) => void
): void {
  if (hasImageFile(data)) {
    onRefuse('image');
    return;
  }
  const read = pasteContent(clipboardText(data), parse);
  if ('refuse' in read) {
    onRefuse(read.refuse);
    return;
  }
  if (read.content.size === 0) return;
  const { from, to } = view.state.selection;
  const tr = view.state.tr.replace(from, to, new Slice(read.content, 0, 0));
  view.dispatch(tr.scrollIntoView());
}

/**
 * Takes over paste, drop and drag on the element around the editor, in the
 * capture phase, so neither ProseMirror's own paths (some of which skip
 * the plugin props, for example while composing, or for a drop with no
 * position) nor the browser's default insertion ever see them. Returns a
 * function that removes the listeners.
 */
export function guardTransfers(
  root: HTMLElement,
  view: () => EditorView,
  parse: (md: string) => Node,
  onRefuse: (hint: HintKind) => void
): () => void {
  const onPaste = (e: Event): void => {
    e.preventDefault();
    e.stopPropagation();
    pasteInto(view(), (e as ClipboardEvent).clipboardData, parse, onRefuse);
  };
  const onDrop = (e: Event): void => {
    e.preventDefault();
    e.stopPropagation();
    if (hasImageFile((e as DragEvent).dataTransfer)) onRefuse('image');
  };
  const onDrag = (e: Event): void => {
    e.preventDefault();
    e.stopPropagation();
    const dt = (e as DragEvent).dataTransfer;
    if (dt) dt.dropEffect = 'none';
  };
  const events: [string, (e: Event) => void][] = [
    ['paste', onPaste],
    ['drop', onDrop],
    ['dragover', onDrag],
    ['dragenter', onDrag],
    ['dragstart', onDrag],
  ];
  for (const [name, fn] of events) root.addEventListener(name, fn, true);
  return (): void => {
    for (const [name, fn] of events) root.removeEventListener(name, fn, true);
  };
}

/** The paste and drop plugin (behind guardTransfers, which normally handles both). */
export function pastePlugin(
  parse: (md: string) => Node,
  onRefuse: (hint: HintKind) => void
): Plugin {
  return new Plugin({
    key: pasteKey,
    props: {
      handlePaste(view: EditorView, event: ClipboardEvent): boolean {
        pasteInto(view, event.clipboardData, parse, onRefuse);
        return true;
      },
      transformPastedHTML(): string {
        return '';
      },
      handleDrop(_view: EditorView, event: DragEvent): boolean {
        if (hasImageFile(event.dataTransfer)) onRefuse('image');
        return true;
      },
    },
  });
}
