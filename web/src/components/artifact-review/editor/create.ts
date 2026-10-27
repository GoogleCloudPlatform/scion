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
 * Creates the Milkdown editor of the review editor: the commonmark and gfm
 * presets, history, the critic schema and safe rendering. The presets'
 * input rules are left out (typing "# " or "**" never reformats text), and
 * the given ProseMirror plugins run before every preset plugin, so their
 * paste, drop and transaction filters come first.
 */

import {
  Editor,
  editorStateOptionsCtx,
  editorViewCtx,
  parserCtx,
  remarkCtx,
  remarkStringifyOptionsCtx,
  rootCtx,
  schemaCtx,
  serializerCtx,
} from '@milkdown/core';
import { commonmark } from '@milkdown/preset-commonmark';
import { gfm } from '@milkdown/preset-gfm';
import { history } from '@milkdown/plugin-history';
import type { Node, Schema } from '@milkdown/prose/model';
import type { Plugin } from '@milkdown/prose/state';
import type { EditorView } from '@milkdown/prose/view';

import { safeRender } from './safe-render.js';
import type { RenderContext } from './safe-render.js';
import { criticSchema } from './schema.js';

/** A position-carrying Markdown tree, as far as preserve.ts reads it. */
export interface MdRoot {
  children: { position?: { start: { offset?: number }; end: { offset?: number } } }[];
}

/** A node of the Markdown parser's tree, with its type and children. */
export interface MdNode {
  type: string;
  position?: { start: { offset?: number }; end: { offset?: number } };
  children?: MdNode[];
}

export interface Core {
  view: EditorView;
  schema: Schema;
  parse(md: string): Node;
  serialize(doc: Node): string;
  /** The text a fragment of inline Markdown shows (marks compare by it). */
  plain(md: string): string;
  parseMd(md: string): MdRoot;
  /** The Markdown parser's tree before its transforms (link reference definitions still in place). */
  parseMdRaw(md: string): MdNode;
  destroy(): Promise<void>;
}

export interface CoreOptions {
  root: HTMLElement;
  render: () => RenderContext;
  /** ProseMirror plugins placed before the presets' plugins. */
  plugins?: (schema: Schema) => Plugin[];
}

/**
 * Preset plugins left out: each changes the document on its own (a view
 * update or an appended transaction), which in a review would be an edit
 * the reviewer did not make. Heading ids (the DOM never shows them; links
 * to headings use the text), ordered-list labels, table alignment and
 * table repair (tables are not edited in this version), marks cleared
 * around line breaks, and typing between two inline atoms (the browser's
 * own input path records it). Every other preset plugin is listed in
 * KEPT_PLUGINS (track.test.ts), so an upgrade that adds one fails a test.
 */
export const DROPPED_PLUGIN =
  /^(MILKDOWN_HEADING_ID|MILKDOWN_KEEP_LIST_ORDER|MILKDOWN_KEEP_TABLE_ALIGN_PLUGIN|MILKDOWN_HARDBREAK_MARKS|MILKDOWN_INLINE_NODES_CURSOR|selectingCells)\$\d*$/;

export async function createCore(opts: CoreOptions): Promise<Core> {
  const editor = await Editor.make()
    .config((ctx) => {
      ctx.set(rootCtx, opts.root);
      ctx.update(remarkStringifyOptionsCtx, (o) => ({ ...o, bullet: '-' as const }));
      ctx.update(editorStateOptionsCtx, (prev) => (options): ReturnType<typeof prev> => {
        const o = prev(options);
        const ours = opts.plugins ? opts.plugins(o.schema ?? ctx.get(schemaCtx)) : [];
        const rest = (o.plugins ?? []).filter(
          (p) =>
            !(p.spec as { isInputRules?: boolean }).isInputRules &&
            !DROPPED_PLUGIN.test((p as unknown as { key: string }).key)
        );
        return { ...o, plugins: [...ours, ...rest] };
      });
    })
    .use(commonmark)
    .use(gfm)
    .use(history)
    .use(criticSchema)
    .use(safeRender(opts.render))
    .create();
  let core: Core | null = null;
  editor.action((ctx) => {
    const remark = ctx.get(remarkCtx);
    const parser = ctx.get(parserCtx);
    const serializer = ctx.get(serializerCtx);
    core = {
      view: ctx.get(editorViewCtx),
      schema: ctx.get(schemaCtx),
      parse: (md: string): Node => parser(md),
      serialize: (doc: Node): string => serializer(doc),
      // A leading word keeps line-start syntax (list markers, headings,
      // quotes) inline, as it is inside a mark.
      plain: (md: string): string => parser(`a ${md}`).textContent.slice(2),
      parseMd: (md: string): MdRoot => remark.runSync(remark.parse(md), md) as unknown as MdRoot,
      parseMdRaw: (md: string): MdNode => remark.parse(md) as unknown as MdNode,
      destroy: async (): Promise<void> => {
        await editor.destroy();
      },
    };
  });
  return core!;
}
