import { useEffect, useRef, useState } from 'react';
import { Bell } from 'lucide-react';
import { api } from '../api';
import { t } from '../i18n';
import { useMenuDismiss } from '../modal';
import { PageIcon } from '../pageIcon';
import { revealBlock } from '../revealBlock';

type Notice = {
  pageId: string;
  blockId: string;
  title: string;
  icon: string;
  at: string;
  seen: boolean;
};

// Where the mention chip finally pays off: without this, "@name" was decoration
// — the person named never learned about it. The bell is the one surface that
// answers "did anybody need me?".
//
// Refreshed on the same `pages` event stream everything else listens to, rather
// than on a timer: a mention only ever appears as the result of a page write,
// so there is nothing to poll for in between.
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
    // Ordered deliberately: ask for the reveal AFTER navigating, so the poll
    // inside it is waiting for the incoming page's blocks rather than racing
    // the outgoing page's.
    revealBlock(n.blockId);
    // Opening the page IS reading the notification. Optimistic locally so the
    // badge drops immediately; the reload afterwards is what makes it true.
    setItems((prev) => prev.map((i) => (i.pageId === n.pageId ? { ...i, seen: true } : i)));
    api.markNotificationsRead(n.pageId).then(load).catch(() => {});
  };

  const markAll = () => {
    setItems((prev) => prev.map((i) => ({ ...i, seen: true })));
    api.markNotificationsRead().then(load).catch(() => {});
  };

  return (
    <div className="share-wrap" ref={wrapRef}>
      <button
        className={'icon-btn' + (unread > 0 ? ' active-star' : '')}
        title={unread > 0 ? t('You were mentioned') : t('Notifications')}
        aria-label={t('Notifications')}
        onClick={() => setOpen((o) => !o)}
      >
        <Bell size={17} />
        {unread > 0 && <span className="badge-count">{unread}</span>}
      </button>
      {open && (
        <div className="menu notif-menu">
          <div className="notif-head">
            <span>{t('Mentions')}</span>
            {unread > 0 && (
              <button className="btn-sm" onClick={markAll}>
                {t('Mark all as read')}
              </button>
            )}
          </div>
          {items.length === 0 && (
            <div className="notif-empty">{t('Nobody has mentioned you yet.')}</div>
          )}
          {items.map((n) => (
            <button
              key={n.pageId}
              className={'notif-item' + (n.seen ? '' : ' unread')}
              onClick={() => openItem(n)}
            >
              <PageIcon icon={n.icon} size={15} fallback={<span>📄</span>} />
              <span className="notif-title">{n.title}</span>
            </button>
          ))}
        </div>
      )}
    </div>
  );
}
