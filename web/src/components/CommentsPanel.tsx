import { useEffect, useMemo, useRef, useState } from 'react';
import { api } from '../api';
import { toast } from '../toast';
import type { Comment } from '../types';
import { Check, MessageSquareText, Quote, Trash2, X } from 'lucide-react';
import { compare, formatRelative } from '../format';
import { t, plural } from '../i18n';
import { revealBlock } from '../revealBlock';

/** A comment body names someone as @[Display Name](userId) — the plain-text
 *  equivalent of the {"type":"mention"} node the main editor uses in its
 *  BlockNote content (see mentions.go). Kept as one shared pattern with the
 *  server's extractCommentMentionIDs, since a mismatch here would mean a
 *  chip renders but no notification fired, or the reverse. */
const MENTION_TOKEN = /@\[([^\]]*)\]\(([A-Za-z0-9_-]+)\)/g;

type Member = { userId: string; name: string; email: string; color: string; avatar: string };

/** Turns the raw @[Name](userId) tokens in a posted comment into readable
 *  "@Name" chips — composing is the only place the bracket/paren form is
 *  ever shown to a person. */
function renderCommentBody(body: string) {
  const out: React.ReactNode[] = [];
  let last = 0;
  let m: RegExpExecArray | null;
  const re = new RegExp(MENTION_TOKEN);
  let key = 0;
  while ((m = re.exec(body))) {
    if (m.index > last) out.push(body.slice(last, m.index));
    out.push(
      <span className="comment-mention-chip" key={`m${key++}`}>
        @{m[1]}
      </span>,
    );
    last = m.index + m[0].length;
  }
  if (last < body.length) out.push(body.slice(last));
  return out;
}

// Comments as a panel beside the document.
//
// This is the FOURTH shape, and the first one that came from watching people
// use it rather than from reasoning about it. His colleagues work in comments
// all day and reported the same two things: they hang at the very bottom, and
// there are two lines to write in.
//
// Both complaints are about the same mistake. The section at the foot of the
// document was modelled on how comments are READ — you finish the text, then
// you see the discussion. But these people are not reading, they are working:
// they write several, they answer, they go back and forth. For that the comment
// has to be visible AT THE SAME TIME as the passage it is about, and the box
// has to be big enough to hold a thought.
//
// So: a panel on the right, next to the text, that stays open. Six lines to
// write in, growing. And it can be closed, because somebody who is only reading
// should get the whole width.
//
// The count in the topbar does NOT come from here. Seeing that a page has three
// open comments is half the value and must work while the panel is closed, so
// the header fetches it itself and this panel only says when something changed
// (COMMENTS_CHANGED). Threading it through as a prop would have tied a number
// everybody needs to a component most people have shut.

/** Fired after anything about this page's comments changed, so the count in the
 *  topbar stays right without the panel owning it. */
export const COMMENTS_CHANGED = 'dworkspace:comments-changed';

const PANEL_KEY = 'dworkspace-comments-open';

export function commentsPanelOpen(): boolean {
  return localStorage.getItem(PANEL_KEY) === '1';
}

export function setCommentsPanelOpen(open: boolean): void {
  if (open) localStorage.setItem(PANEL_KEY, '1');
  else localStorage.removeItem(PANEL_KEY);
}

const when = (iso: string) => formatRelative(iso);

// Derive the colour from the name, so the same person always gets the same one.
export function nameColor(name: string): string {
  let h = 0;
  for (let i = 0; i < name.length; i++) h = (h * 31 + name.charCodeAt(i)) >>> 0;
  const hues = [210, 145, 275, 25, 340, 190, 95, 55];
  return `hsl(${hues[h % hues.length]} 55% 45%)`;
}

export function initials(name: string): string {
  const parts = name.trim().split(/\s+/).filter(Boolean);
  if (!parts.length) return '?';
  return (parts[0][0] + (parts[1]?.[0] ?? '')).toUpperCase();
}

export default function CommentsPanel({
  pageId,
  workspaceId,
  myUserId,
  open,
  onClose,
  pendingBlockId,
  pendingSnippet,
  onClearPending,
  highlightTarget,
}: {
  pageId: string;
  workspaceId: string;
  myUserId: string;
  open: boolean;
  onClose: () => void;
  /** Set from the formatting toolbar's "Comment" button (see Editor.tsx):
   *  the next comment sent attaches to this block instead of the page. */
  pendingBlockId?: string;
  pendingSnippet?: string;
  onClearPending?: () => void;
  /** Set from clicking a comment marker dot in the document — scrolls to and
   *  highlights the comment(s) attached to that block. `at` is a timestamp
   *  so clicking the same dot twice in a row re-triggers the highlight
   *  instead of being a no-op React state update. */
  highlightTarget?: { blockId: string; at: number } | null;
}) {
  const [comments, setComments] = useState<Comment[]>([]);
  const [body, setBody] = useState('');
  const [showResolved, setShowResolved] = useState(false);
  const [highlightedIds, setHighlightedIds] = useState<Set<string>>(new Set());
  const listRef = useRef<HTMLDivElement>(null);
  const boxRef = useRef<HTMLTextAreaElement>(null);

  // Replying INTO an existing thread from its own "Reply" link, as opposed to
  // pendingBlockId (starting a brand NEW one from the toolbar's "Comment on
  // this section" button — see Editor.tsx). A fresh toolbar click always
  // wins over a reply that was mid-thought: it is a deliberate, later action.
  const [replyTo, setReplyTo] = useState<{ blockId: string; snippet: string } | null>(null);
  useEffect(() => {
    if (pendingBlockId) setReplyTo(null);
  }, [pendingBlockId]);
  const activeTarget = pendingBlockId
    ? { blockId: pendingBlockId, snippet: pendingSnippet }
    : replyTo
      ? { blockId: replyTo.blockId, snippet: replyTo.snippet }
      : null;
  const clearActiveTarget = () => {
    if (pendingBlockId) onClearPending?.();
    setReplyTo(null);
  };
  const replyToThread = (blockId: string) => {
    setReplyTo({ blockId, snippet: '' });
    requestAnimationFrame(() => boxRef.current?.focus());
  };

  // Workspace members for the "@" mention list — same source and shape the
  // main editor's own mention menu uses (see Editor.tsx), fetched once per
  // workspace rather than per keystroke.
  const [members, setMembers] = useState<Member[]>([]);
  useEffect(() => {
    if (!workspaceId) return;
    let alive = true;
    api
      .listMembers(workspaceId)
      .then((m) => alive && setMembers(m))
      .catch(() => {});
    return () => {
      alive = false;
    };
  }, [workspaceId]);

  // The "@" picker: null when not showing. `start` is the index of the "@"
  // itself, so a pick can splice the query text back out precisely.
  const [mentionAt, setMentionAt] = useState<{ start: number; query: string } | null>(null);
  const [mentionIndex, setMentionIndex] = useState(0);
  const mentionMatches = useMemo(() => {
    if (!mentionAt) return [];
    const q = mentionAt.query.toLowerCase();
    return members
      .filter((m) => m.userId !== myUserId)
      .filter((m) => m.name.toLowerCase().includes(q) || m.email.toLowerCase().includes(q))
      .slice(0, 6);
  }, [mentionAt, members, myUserId]);

  // Re-scan for an active "@query" ending at the caret on every keystroke —
  // cheaper than tracking it incrementally and correct even after a paste,
  // an arrow-key move, or deleting into the middle of a token.
  const scanMention = (text: string, caret: number) => {
    const upTo = text.slice(0, caret);
    const at = upTo.lastIndexOf('@');
    if (at === -1) return setMentionAt(null);
    const query = upTo.slice(at + 1);
    // A space (or a newline) ends the token; a token already containing the
    // closing bracket of a previous pick is not being composed any more.
    if (/[\s\])]/.test(query)) return setMentionAt(null);
    setMentionAt({ start: at, query });
    setMentionIndex(0);
  };

  const pickMention = (m: Member) => {
    if (!mentionAt || !boxRef.current) return;
    const before = body.slice(0, mentionAt.start);
    const after = body.slice(mentionAt.start + 1 + mentionAt.query.length);
    const token = `@[${m.name}](${m.userId}) `;
    const next = before + token + after;
    setBody(next);
    setMentionAt(null);
    const caret = before.length + token.length;
    requestAnimationFrame(() => {
      boxRef.current?.focus();
      boxRef.current?.setSelectionRange(caret, caret);
    });
  };

  const load = () =>
    void api
      .listComments(pageId)
      .then((list) => {
        setComments(list);
        window.dispatchEvent(new CustomEvent(COMMENTS_CHANGED));
      })
      .catch(() => {});
  // eslint-disable-next-line react-hooks/exhaustive-deps
  useEffect(load, [pageId]);

  // The topbar button opens the panel and puts the cursor in the box: the
  // reason somebody presses it is almost always that they want to write.
  useEffect(() => {
    if (open) requestAnimationFrame(() => boxRef.current?.focus());
  }, [open, pageId]);

  // A marker dot in the document was clicked — scroll to and briefly
  // highlight the comment(s) it points at. Depends on `comments` too: the
  // panel can mount (and start this effect) before its own fetch resolves,
  // so an empty match on the first pass is not the end of it — the effect
  // simply runs again once the real list arrives.
  useEffect(() => {
    if (!highlightTarget) return;
    const matches = comments.filter((c) => c.blockId === highlightTarget.blockId);
    if (matches.length === 0) return;
    const el = listRef.current?.querySelector(`[data-comment-id="${matches[0].id}"]`);
    el?.scrollIntoView({ behavior: 'smooth', block: 'center' });
    const ids = new Set(matches.map((c) => c.id));
    setHighlightedIds(ids);
    const timer = window.setTimeout(() => setHighlightedIds(new Set()), 1600);
    return () => window.clearTimeout(timer);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [highlightTarget?.blockId, highlightTarget?.at, comments]);

  const add = async (e?: React.FormEvent) => {
    e?.preventDefault();
    const text = body.trim();
    if (!text) return;
    const blockId = activeTarget?.blockId;
    setBody('');
    setMentionAt(null);
    if (boxRef.current) boxRef.current.style.height = '';
    try {
      await api.createComment(pageId, text, blockId);
      clearActiveTarget();
      load();
      // Your own contribution should be visible, not below the fold.
      requestAnimationFrame(() => listRef.current?.scrollTo({ top: 1e6, behavior: 'smooth' }));
    } catch (err) {
      setBody(text); // swallow nothing if sending fails
      toast((err as Error).message || t('Could not post the comment'));
    }
  };

  const toggleResolve = async (c: Comment) => {
    await api.resolveComment(c.id, !c.resolvedAt).catch(() => {});
    load();
  };
  const remove = async (c: Comment) => {
    await api.deleteComment(c.id).catch(() => {});
    load();
  };

  const openOnes = comments.filter((c) => !c.resolvedAt);
  const resolved = comments.filter((c) => c.resolvedAt);
  const visible = showResolved ? comments : openOnes;

  // Each selected section is its own thread, kept separate rather than
  // interleaved by time with every other section's — a reply belongs with
  // the comment it replies to, not wherever it happened to land in a single
  // shared timeline. Comments on the page as a whole (no blockId) are their
  // own thread too, ordered alongside the rest by when it started.
  const threads: { blockId: string; items: Comment[] }[] = [];
  const threadIndex = new Map<string, number>();
  for (const c of visible) {
    const key = c.blockId || '';
    let idx = threadIndex.get(key);
    if (idx === undefined) {
      idx = threads.length;
      threadIndex.set(key, idx);
      threads.push({ blockId: key, items: [] });
    }
    threads[idx].items.push(c);
  }
  threads.sort((a, b) => compare(a.items[0].createdAt, b.items[0].createdAt));

  // Mounted but invisible: the count above has to be right before anybody looks.
  if (!open) return null;

  // Grow with what is written. A textarea has no content height of its own, so
  // the height is reset to auto first: otherwise scrollHeight keeps reporting
  // the height the box already has, and it could only ever get taller, never
  // shrink back when text is deleted.
  const grow = (el: HTMLTextAreaElement) => {
    el.style.height = 'auto';
    el.style.height = el.scrollHeight + 'px';
  };

  return (
    <aside className="comments-panel" aria-label={t('Comments')}>
      <div className="cp-head">
        <span className="cp-head-title">
          <MessageSquareText size={15} />
          {t('Comments')}
          {openOnes.length > 0 && <span className="cp-count">{openOnes.length}</span>}
        </span>
        <button className="icon-btn" title={t('Close')} onClick={onClose}>
          <X size={15} />
        </button>
      </div>

      {resolved.length > 0 && (
        <label className="cp-toggle">
          <input
            type="checkbox"
            checked={showResolved}
            onChange={(e) => setShowResolved(e.target.checked)}
          />
          {t('Show {n} resolved', { n: resolved.length })}
        </label>
      )}

      <div className="cp-list" ref={listRef}>
        {visible.length === 0 ? (
          // Said once, quietly. The old bottom section showed "no comments yet"
          // as the most prominent thing on an empty page; here the box below is
          // already the invitation, so this only has to explain the silence.
          <p className="cp-empty">{t('Nothing here yet. Write the first one.')}</p>
        ) : (
          threads.map((thread) => (
            <section
              key={thread.blockId || 'page'}
              className={
                'cp-thread' +
                (thread.blockId ? ' cp-thread-section' : '') +
                (thread.items.some((c) => highlightedIds.has(c.id)) ? ' is-active' : '')
              }
            >
              {thread.blockId && (
                <div className="cp-thread-head">
                  <button
                    type="button"
                    className="cp-target"
                    onClick={() => revealBlock(thread.blockId)}
                    title={t('Jump to this section in the document')}
                  >
                    <Quote size={11} /> {t('On a section')}
                  </button>
                </div>
              )}
              {thread.items.map((c) => {
                const name = c.authorName || t('unknown');
                return (
                  <article
                    key={c.id}
                    data-comment-id={c.id}
                    className={
                      'cp-item' +
                      (c.resolvedAt ? ' is-resolved' : '') +
                      (highlightedIds.has(c.id) ? ' is-highlighted' : '')
                    }
                  >
                    <div className="cp-item-head">
                      <span
                        className="cp-avatar"
                        style={{ background: c.authorAvatar ? 'transparent' : c.authorColor || nameColor(name) }}
                      >
                        {c.authorAvatar ? <img src={c.authorAvatar} alt="" /> : initials(name)}
                      </span>
                      <span className="cp-author">{name}</span>
                      <time className="cp-time">{when(c.createdAt)}</time>
                    </div>
                    <div className="cp-body">{renderCommentBody(c.body)}</div>
                    <div className="cp-actions">
                      <button
                        className="cp-act"
                        title={c.resolvedAt ? t('Reopen') : t('Mark as resolved')}
                        onClick={() => void toggleResolve(c)}
                      >
                        <Check size={13} /> {c.resolvedAt ? t('Reopen') : t('Resolved')}
                      </button>
                      {c.authorId === myUserId && (
                        <button className="cp-act danger" title={t('Delete')} onClick={() => void remove(c)}>
                          <Trash2 size={13} />
                        </button>
                      )}
                    </div>
                  </article>
                );
              })}
              {thread.blockId && (
                <button type="button" className="cp-thread-reply" onClick={() => replyToThread(thread.blockId)}>
                  {t('Reply')}
                </button>
              )}
            </section>
          ))
        )}
      </div>

      <form className="cp-compose" onSubmit={add}>
        {activeTarget && (
          <div className="cp-pending-target">
            <Quote size={12} />
            <span className="cp-pending-target-text">
              {activeTarget.snippet ? `“${activeTarget.snippet}”` : t('This section')}
            </span>
            <button
              type="button"
              className="icon-btn cp-pending-target-clear"
              title={t('Comment on the whole page instead')}
              onClick={clearActiveTarget}
            >
              <X size={12} />
            </button>
          </div>
        )}
        {mentionAt && mentionMatches.length > 0 && (
          <div className="cp-mention-menu" role="listbox">
            {mentionMatches.map((m, i) => (
              <button
                type="button"
                key={m.userId}
                className={'cp-mention-item' + (i === mentionIndex ? ' active' : '')}
                onMouseDown={(e) => {
                  // mousedown, not click: firing before the textarea's blur
                  // keeps focus (and the caret position) in the box.
                  e.preventDefault();
                  pickMention(m);
                }}
                onMouseEnter={() => setMentionIndex(i)}
              >
                <span className="cp-avatar" style={{ background: m.avatar ? 'transparent' : m.color || nameColor(m.name) }}>
                  {m.avatar ? <img src={m.avatar} alt="" /> : initials(m.name)}
                </span>
                <span className="cp-mention-item-text">
                  <span className="cp-mention-item-name">{m.name}</span>
                  <span className="cp-mention-item-email">{m.email}</span>
                </span>
              </button>
            ))}
          </div>
        )}
        <textarea
          ref={boxRef}
          value={body}
          rows={2}
          placeholder={t('Write a comment… (@ to mention someone)')}
          onChange={(e) => {
            setBody(e.target.value);
            grow(e.target);
            scanMention(e.target.value, e.target.selectionStart ?? e.target.value.length);
          }}
          onClick={(e) => {
            const el = e.currentTarget;
            scanMention(el.value, el.selectionStart ?? el.value.length);
          }}
          onKeyDown={(e) => {
            if (mentionAt && mentionMatches.length > 0) {
              if (e.key === 'ArrowDown') {
                e.preventDefault();
                setMentionIndex((i) => (i + 1) % mentionMatches.length);
                return;
              }
              if (e.key === 'ArrowUp') {
                e.preventDefault();
                setMentionIndex((i) => (i - 1 + mentionMatches.length) % mentionMatches.length);
                return;
              }
              if (e.key === 'Enter' || e.key === 'Tab') {
                e.preventDefault();
                pickMention(mentionMatches[mentionIndex]);
                return;
              }
              if (e.key === 'Escape') {
                e.preventDefault();
                setMentionAt(null);
                return;
              }
            }
            // ⌘/Ctrl+Enter sends; Enter makes a paragraph. The other way round
            // would be faster for one-liners and would cost a half-written
            // thought every time somebody reaches for a new line — and this box
            // exists because people write more than one line here.
            if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) void add(e);
          }}
        />
        <div className="cp-compose-foot">
          <span className="cp-hint">{t('⌘↵ to send')}</span>
          <button className="btn primary btn-sm" type="submit" disabled={!body.trim()}>
            {t('Send')}
          </button>
        </div>
      </form>

      {resolved.length > 0 && !showResolved && (
        <p className="cp-foot">
          {plural(resolved.length, '{n} resolved comment', '{n} resolved comments')}
        </p>
      )}
    </aside>
  );
}
