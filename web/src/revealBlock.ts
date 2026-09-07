// Scroll a specific block into view and flash it once.
//
// The awkward part is timing. Opening a notification navigates to another page,
// and that page's blocks are not in the DOM yet when the click handler returns:
// the editor mounts, then the collaborative document syncs, and only then does
// the paragraph exist. Anything that reaches for the element immediately finds
// nothing and silently does nothing — which reads exactly like a broken link.
//
// So this polls instead of guessing a delay. Block ids are UUIDs and unique
// across the instance, so finding the element is itself proof that the right
// page finished rendering; no separate "are we there yet" check is needed.
//
// The flash itself is a plain `<div>` this module owns, positioned over the
// block from its live bounding rect — NOT a class added to the block's own
// element. An earlier version did that, and lost: ProseMirror periodically
// rebuilds a block node's DOM from its own model (a remote collaborator's
// cursor merely passing through the block is enough to trigger it), and that
// rebuild strips any class or attribute this module added, often within
// milliseconds — sometimes before the class ever painted a single frame.
// Reading the block's position every frame is safe; writing into that tree
// is not, so the overlay tracks the block's rect from outside it instead.

const POLL_MS = 60;
const GIVE_UP_MS = 4000;
const FLASH_MS = 1000;
const OVERLAY_CLASS = 'block-flash-overlay';

let cancel: (() => void) | null = null;

/** Stop any reveal still waiting for its block — a newer one supersedes it. */
export function cancelReveal() {
  if (cancel) {
    cancel();
    cancel = null;
  }
}

export function revealBlock(blockId: string) {
  cancelReveal();
  if (!blockId) return;

  const started = Date.now();
  let pollTimer = 0;
  let raf = 0;
  let stopped = false;
  let scrolled = false;
  let flashEndAt = 0;
  let overlay: HTMLDivElement | null = null;

  const removeOverlay = () => {
    overlay?.remove();
    overlay = null;
  };
  cancel = () => {
    stopped = true;
    window.clearTimeout(pollTimer);
    cancelAnimationFrame(raf);
    removeOverlay();
  };

  const track = () => {
    if (stopped) return;
    const el = document.querySelector(`[data-id="${blockId}"]`);
    if (!el) {
      cancel = null;
      removeOverlay();
      return;
    }
    if (!scrolled) {
      scrolled = true;
      el.scrollIntoView({ behavior: 'smooth', block: 'center' });
    }
    if (!overlay) {
      overlay = document.createElement('div');
      overlay.className = OVERLAY_CLASS;
      document.body.appendChild(overlay);
      flashEndAt = Date.now() + FLASH_MS;
      // One frame at full opacity before the fade-out transition kicks in —
      // adding both classes in the same tick risks the browser coalescing
      // straight to the end state, so the flash never visibly appears at all.
      requestAnimationFrame(() => overlay?.classList.add('fading'));
    }
    const r = el.getBoundingClientRect();
    overlay.style.top = `${r.top}px`;
    overlay.style.left = `${r.left}px`;
    overlay.style.width = `${r.width}px`;
    overlay.style.height = `${r.height}px`;
    if (Date.now() < flashEndAt) {
      raf = requestAnimationFrame(track);
    } else {
      cancel = null;
      removeOverlay();
    }
  };

  const waitForBlock = () => {
    if (stopped) return;
    const el = document.querySelector(`[data-id="${blockId}"]`);
    if (el) {
      track();
      return;
    }
    if (Date.now() - started > GIVE_UP_MS) {
      // The page opened; only the jump failed. Leaving the reader at the top of
      // a page they can read beats any error message here.
      cancel = null;
      return;
    }
    pollTimer = window.setTimeout(waitForBlock, POLL_MS);
  };
  waitForBlock();
}
