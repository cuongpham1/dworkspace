import { useEffect, useRef, useState } from 'react';
import { Bell, MessageSquareText, Rss } from 'lucide-react';
import { api } from '../api';
import type { Notice } from '../types';
import { t } from '../i18n';
import { useMenuDismiss } from '../modal';
import { PageIcon } from '../pageIcon';
import { revealBlock } from '../revealBlock';

// A comment mention's body still carries the raw @[Name](userId) token (see
// CommentsPanel's renderCommentBody) — the bell only needs the readable
// "@Name" form, short enough to sit on one line under the page title.
function commentSnippet(body: string): string {
  const plain = body.replace(/@\[([^\]]*)\]\([A-Za-z0-9_-]+\)/g, '@$1');
  return plain.length > 90 ? plain.slice(0, 90) + '…' : plain;
}

// Where the mention chip and the "Follow" button finally pay off: without
// this, "@name" was decoration and following a page was a promise nobody
// kept — the person or the follower never learned anything happened. The
// bell is the one surface that answers "did anybody need me?" AND "did
// something I follow just change?", merged into one feed so nobody has to
// check two separate places for one kind of question ("is there anything
// waiting for me").
//
// Refreshed on the same `pages` event stream everything else listens to,
// rather than on a timer: both a mention and a subscription notice only ever
// appear as the result of a page write, so there is nothing to poll for in
// between.
export default function NotificationBell({
  onNavigate,
}: {
  onNavigate: (id: string) => void;
}) {
  const [items, setItems] = useState<Notice[]>([]);
  const [open, setOpen] = useState(false);
  const wrapRef = useRef<HTMLDivElement>(null);
  useMenuDismiss(open, wrapRef, () => setOpen(false));

  const load = () => {
    api
      .notifications()
      .then(setItems)
      .catch(() => {});
  };

  useEffect(() => {
    load();
    const onPages = () => load();
    window.addEventListener('dworkspace:pages', onPages);
    return () => window.removeEventListener('dworkspace:pages', onPages);
  }, []);

  const unread = items.filter((i) => !i.seen).length;

  const openItem = (n: Notice) => {
    setOpen(false);
    onNavigate(n.pageId);
    if (n.kind === 'mention' || n.kind === 'comment_mention') {
      // Ordered deliberately: ask for the reveal AFTER navigating, so the poll
      // inside it is waiting for the incoming page's blocks rather than racing
      // the outgoing page's. For a comment mention this lands on the block the
      // comment is attached to — not inside the comments panel itself, which
      // is one click away and not worth the extra plumbing to force open.
      revealBlock(n.blockId ?? '');
    }
    // Opening the item IS reading the notification. Optimistic locally so the
    // badge drops immediately; the reload afterwards is what makes it true.
    setItems((prev) => prev.map((i) => (i.id === n.id ? { ...i, seen: true } : i)));
    api.markNotificationsRead(n.id).then(load).catch(() => {});
  };

  const markAll = () => {
    setItems((prev) => prev.map((i) => ({ ...i, seen: true })));
    api.markNotificationsRead().then(load).catch(() => {});
  };

  return (
    <div className="share-wrap" ref={wrapRef}>
      <button
        className={'icon-btn' + (unread > 0 ? ' active-star' : '')}
        title={unread > 0 ? t('You have unread notifications') : t('Notifications')}
        aria-label={t('Notifications')}
        onClick={() => setOpen((o) => !o)}
      >
        <Bell size={17} />
        {unread > 0 && <span className="badge-count">{unread}</span>}
      </button>
      {open && (
        <div className="menu notif-menu">
          <div className="notif-head">
            <span>{t('Notifications')}</span>
            {unread > 0 && (
              <button className="btn-sm" onClick={markAll}>
                {t('Mark all as read')}
              </button>
            )}
          </div>
          {items.length === 0 && (
            <div className="notif-empty">{t('Nothing here yet.')}</div>
          )}
          {items.map((n) => (
            <button
              key={n.id}
              className={'notif-item' + (n.seen ? '' : ' unread')}
              onClick={() => openItem(n)}
            >
              {n.kind === 'subscription' ? (
                <Rss size={15} className="notif-kind-icon" />
              ) : n.kind === 'comment_mention' ? (
                <MessageSquareText size={15} className="notif-kind-icon" />
              ) : (
                <PageIcon icon={n.icon} size={15} fallback={<span>📄</span>} />
              )}
              <span className="notif-copy">
                <span className="notif-title">{n.title}</span>
                {/* A subscription notice is about a NEW document — the line
                    above already says which one. This second line is the
                    reason it showed up at all: which followed page it
                    relates to, so the notice reads as an answer rather than
                    a name out of nowhere. */}
                {n.kind === 'subscription' && n.followedTitle && (
                  <span className="notif-context">{t('New link to “{name}”').replace('{name}', n.followedTitle)}</span>
                )}
                {/* A comment mention: who named you, and roughly what they
                    said — the title alone only says which page, not why. */}
                {n.kind === 'comment_mention' && (
                  <span className="notif-context">
                    {t('{name} in a comment: {body}', {
                      name: n.authorName || t('Someone'),
                      body: commentSnippet(n.body || ''),
                    })}
                  </span>
                )}
              </span>
            </button>
          ))}
        </div>
      )}
    </div>
  );
}
