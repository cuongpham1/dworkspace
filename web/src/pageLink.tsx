import type { CSSProperties } from 'react';
import { BlockNoteSchema, defaultBlockSpecs, defaultInlineContentSpecs } from '@blocknote/core';
import { createReactInlineContentSpec } from '@blocknote/react';
import { bookmarkSpec, databaseSpec, calloutSpec, tocSpec, columnsSpec, mermaidSpec } from './blocks';

// A "pageLink" is an inline mention of another dworkspace page. It stores the target
// page id and a display label. Clicking it dispatches a navigation event that
// App listens for (keeps BlockNote's render decoupled from React routing).
export const pageLinkSpec = createReactInlineContentSpec(
  {
    type: 'pageLink',
    propSchema: {
      pageId: { default: '' },
      label: { default: '' },
    },
    content: 'none',
  } as const,
  {
    render: (props) => (
      <button
        type="button"
        className="page-link"
        contentEditable={false}
        onClick={() =>
          window.dispatchEvent(
            new CustomEvent('dworkspace:navigate', { detail: props.inlineContent.props.pageId }),
          )
        }
      >
        <span className="page-link-icon">🔗</span>
        {props.inlineContent.props.label || 'Untitled'}
      </button>
    ),
  },
);

// A "mention" is an inline @-reference to a workspace member — a name, not a
// link. It carries the userId (so a rename doesn't orphan it) and a label
// snapshot (so the chip still reads right for someone who has since left the
// workspace and can no longer be resolved).
export const mentionSpec = createReactInlineContentSpec(
  {
    type: 'mention',
    propSchema: {
      userId: { default: '' },
      label: { default: '' },
      color: { default: '' },
    },
    content: 'none',
  } as const,
  {
    render: (props) => (
      <span
        className="user-mention"
        contentEditable={false}
        style={
          props.inlineContent.props.color
            ? ({ '--mention-color': props.inlineContent.props.color } as CSSProperties)
            : undefined
        }
      >
        @{props.inlineContent.props.label || 'Unknown'}
      </span>
    ),
  },
);

// Schema = the default blocks plus our own (callout, table of contents,
// bookmark/embed, embedded database, columns) and the pageLink inline content.
// createReactBlockSpec returns a factory in 0.51 — call it.
//
// Columns used to come from @blocknote/xl-multi-column, which is BlockNote's
// PAID tier ("GPL-3.0 OR PROPRIETARY"). Nobody used it — 0 of 1410 pages — and
// keeping it would have forced a licence decision the day any closed part
// exists. Ours is below in blocks.tsx.
export const dworkspaceSchema =
  BlockNoteSchema.create({
    blockSpecs: {
      ...defaultBlockSpecs,
      callout: calloutSpec(),
      toc: tocSpec(),
      bookmark: bookmarkSpec(),
      database: databaseSpec(),
      columns: columnsSpec(),
      mermaid: mermaidSpec(),
    },
    inlineContentSpecs: {
      ...defaultInlineContentSpecs,
      pageLink: pageLinkSpec,
      mention: mentionSpec,
    },
  });

export type DworkspaceEditor = typeof dworkspaceSchema.BlockNoteEditor;
