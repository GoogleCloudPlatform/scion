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
 * Creates the Milkdown editor of the review editor: the schemas and
 * Markdown plugins of the commonmark and gfm presets (PRESET_PARTS),
 * history, the critic schema and safe rendering. No input rules run
 * (typing "# " or "**" never reformats text), and the given ProseMirror
 * plugins run before every other plugin, so their paste, drop and
 * transaction filters come first.
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
import {
  blockContainerTypes,
  hardbreakFilterNodes,
  hardbreakFilterPlugin,
  remarkAddOrderInListPlugin,
  remarkHtmlTransformer,
  remarkInlineLinkPlugin,
  remarkLineBreak,
  remarkMarker,
  remarkPreserveEmptyLinePlugin,
  schema as commonmarkSchema,
} from '@milkdown/preset-commonmark';
import { autoInsertSpanPlugin, remarkGFMPlugin, schema as gfmSchema } from '@milkdown/preset-gfm';
import type { MilkdownPlugin } from '@milkdown/ctx';
import { ParserState } from '@milkdown/transformer';
import type { MarkdownNode } from '@milkdown/transformer';
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

/** One reading of Markdown, for the checks that need its trees and its document. */
export interface MdRead {
  /**
   * The link reference definitions at any depth, as the parser reads them
   * before its transforms (which copy them onto references and drop them).
   */
  definitions: MdNode[];
  /** The parser's tree after its transforms (what parseMd gives). */
  tree: MdRoot;
  /** The document built from that tree, as parse builds it. */
  doc: Node;
}

/**
 * The link reference definitions of a tree read before the transforms, at
 * any depth, in document order (a definition's own children are not read).
 */
export function definitionNodes(root: MdNode): MdNode[] {
  const out: MdNode[] = [];
  const walk = (n: MdNode): void => {
    if (n.type === 'definition') {
      out.push(n);
      return;
    }
    for (const k of n.children ?? []) walk(k);
  };
  walk(root);
  return out;
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
  /**
   * parse, parseMd and the definitions of parseMdRaw from one parse of md:
   * the tree is transformed and the document built from it exactly as
   * parse does.
   */
  read(md: string): MdRead;
  destroy(): Promise<void>;
}

export interface CoreOptions {
  root: HTMLElement;
  render: () => RenderContext;
  /** ProseMirror plugins placed before the presets' plugins. */
  plugins?: (schema: Schema) => Plugin[];
}

/**
 * The parts of the commonmark and gfm presets the editor uses: their
 * schemas and the Markdown parser and serializer plugins, so every
 * Markdown syntax reads and writes as in the presets. Left out, so the
 * editor chunk does not carry them: the presets' commands, keymaps, input
 * and paste rules (suggest mode refuses formatting, list and table
 * commands; the review keymap in controller.ts handles their shortcuts),
 * and the preset plugins that change the document on their own (a view
 * update or an appended transaction), which in a review would be an edit
 * the reviewer did not make: heading ids (the DOM never shows them; links
 * to headings use the text), ordered-list labels, table alignment, table
 * editing and repair, marks cleared around line breaks, and typing
 * between two inline atoms (the browser's own input path records it).
 * Every plugin the editor ends up with is listed in KEPT_PLUGINS
 * (track.test.ts), so an upgrade that adds one fails a test.
 */
export const PRESET_PARTS: MilkdownPlugin[] = [
  commonmarkSchema,
  hardbreakFilterNodes,
  hardbreakFilterPlugin,
  blockContainerTypes,
  remarkAddOrderInListPlugin,
  remarkInlineLinkPlugin,
  remarkLineBreak,
  remarkHtmlTransformer,
  remarkMarker,
  remarkPreserveEmptyLinePlugin,
  gfmSchema,
  autoInsertSpanPlugin,
  remarkGFMPlugin,
].flat();

export async function createCore(opts: CoreOptions): Promise<Core> {
  const editor = await Editor.make()
    .config((ctx) => {
      ctx.set(rootCtx, opts.root);
      ctx.update(remarkStringifyOptionsCtx, (o) => ({ ...o, bullet: '-' as const }));
      // Table rows are written without padding to a common column width,
      // so the text of a row depends on that row alone and an edit in one
      // row lands in that row's source (preserve.ts).
      ctx.set(remarkGFMPlugin.options.key, { tablePipeAlign: false });
      ctx.update(editorStateOptionsCtx, (prev) => (options): ReturnType<typeof prev> => {
        const o = prev(options);
        const ours = opts.plugins ? opts.plugins(o.schema ?? ctx.get(schemaCtx)) : [];
        // Milkdown's core adds an input rules plugin (empty without the
        // presets' rules); it is left out too.
        const rest = (o.plugins ?? []).filter(
          (p) => !(p.spec as { isInputRules?: boolean }).isInputRules
        );
        return { ...o, plugins: [...ours, ...rest] };
      });
    })
    .use(PRESET_PARTS)
    .use(history)
    .use(criticSchema)
    .use(safeRender(opts.render))
    .create();
  let core: Core | null = null;
  editor.action((ctx) => {
    const remark = ctx.get(remarkCtx);
    const parser = ctx.get(parserCtx);
    const serializer = ctx.get(serializerCtx);
    const schema = ctx.get(schemaCtx);
    core = {
      view: ctx.get(editorViewCtx),
      schema,
      parse: (md: string): Node => parser(md),
      serialize: (doc: Node): string => serializer(doc),
      // A leading word keeps line-start syntax (list markers, headings,
      // quotes) inline, as it is inside a mark.
      plain: (md: string): string => parser(`a ${md}`).textContent.slice(2),
      parseMd: (md: string): MdRoot => remark.runSync(remark.parse(md), md) as unknown as MdRoot,
      parseMdRaw: (md: string): MdNode => remark.parse(md) as unknown as MdNode,
      read: (md: string): MdRead => {
        // As Milkdown's parser: runSync(parse(md)), then ParserState builds
        // the document from that tree. The definitions are read first, as
        // the transforms drop them from the tree.
        const raw = remark.parse(md);
        const definitions = definitionNodes(raw as unknown as MdNode);
        const tree = remark.runSync(raw, md);
        const doc = new ParserState(schema).next(tree as MarkdownNode).toDoc();
        return { definitions, tree: tree as unknown as MdRoot, doc };
      },
      destroy: async (): Promise<void> => {
        await editor.destroy();
      },
    };
  });
  return core!;
}
