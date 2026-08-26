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

const FLASH_CLASS = 'block-flash';
const POLL_MS = 60;
const GIVE_UP_MS = 4000;

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
  let timer = 0;
  let stopped = false;
  cancel = () => {
    stopped = true;
    window.clearTimeout(timer);
  };

  const tick = () => {
    if (stopped) return;
    const el = document.querySelector(`[data-id="${blockId}"]`);
    if (el) {
      cancel = null;
      el.scrollIntoView({ behavior: 'smooth', block: 'center' });
      // Re-adding the class has to be preceded by removing it, or a second
      // visit to the same block never restarts the animation.
      el.classList.remove(FLASH_CLASS);
      void (el as HTMLElement).offsetWidth; // force a reflow so the restart takes
      el.classList.add(FLASH_CLASS);
      window.setTimeout(() => el.classList.remove(FLASH_CLASS), 1100);
      return;
    }
    if (Date.now() - started > GIVE_UP_MS) {
      // The page opened; only the jump failed. Leaving the reader at the top of
      // a page they can read beats any error message here.
      cancel = null;
      return;
    }
    timer = window.setTimeout(tick, POLL_MS);
  };
  tick();
}
