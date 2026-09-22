import { createContext, useContext } from 'react';
import type { PageMeta } from './types';

// Custom blocks render INSIDE the editor but have no access to its props. The
// database block needs exactly that: the page list (to pick a database and show
// its title), the tag colours and navigation. A context is the clean way to do
// it — module state would be coupled invisibly and would collide with two
// documents open at once.

export interface BlockCtx {
  pagesById: Map<string, PageMeta>;
  tagColors: Record<string, string>;
  onNavigate: (id: string | null) => void;
  onPagesChanged: () => void;
  // An embedded page opens in a tab rather than replacing what you are reading:
  // the whole point of embedding it was to have both at once, and navigating
  // away from the page that embeds it would undo that with one click.
  onOpenInNewTab: (id: string) => void;
}

const empty: BlockCtx = {
  pagesById: new Map(),
  tagColors: {},
  onNavigate: () => {},
  onPagesChanged: () => {},
  onOpenInNewTab: () => {},
};

export const BlockContext = createContext<BlockCtx>(empty);
export const useBlockCtx = () => useContext(BlockContext);

// How deep embeds may nest. An embedded page renders with the same schema as
// the page embedding it, so a page that embeds itself — or two that embed each
// other — is an infinite render, and it is not a mistake anybody has to make on
// purpose: moving a block can create the cycle. Past the limit the embed draws
// itself as a link instead, which still says what it points at.
//
// Two rather than one: a page embedded inside a page is a reasonable thing to
// want, and stopping at the first level would forbid it. Beyond that nothing is
// legible on screen anyway.
export const EMBED_MAX_DEPTH = 2;

// Counts embeds between here and the page, and names the pages on the way so a
// cycle is caught at the moment it closes rather than at the depth limit — A
// embedding A is wrong at the first hop, not the third.
export interface EmbedDepth {
  depth: number;
  ancestors: readonly string[];
}

export const EmbedDepthContext = createContext<EmbedDepth>({ depth: 0, ancestors: [] });
export const useEmbedDepth = () => useContext(EmbedDepthContext);
