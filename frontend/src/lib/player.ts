import { createStore, type Store } from './store';

/**
 * Glue between the <video> element and everything that follows or drives the
 * playhead (note citations, transcript, `?t=`). Seeking works before the media
 * is loaded and even when there is no playable file: the position is kept and
 * applied as soon as an element with metadata is attached.
 */
export interface PlayerController {
  /** Current position in milliseconds. */
  readonly time: Store<number>;
  seek(ms: number, opts?: { play?: boolean }): void;
  attach(el: HTMLMediaElement | null): void;
  /** Call when the element fires `loadedmetadata`. */
  ready(): void;
  /** Call on `timeupdate` / `seeked`. */
  sync(): void;
  /** Remember the position so a reloaded source resumes where it was. */
  hold(): void;
}

export function createPlayerController(): PlayerController {
  const time = createStore(0);
  let el: HTMLMediaElement | null = null;
  let pending: { ms: number; play: boolean } | null = null;

  function apply() {
    if (!el || !pending || el.readyState < 1) return;
    const { ms, play } = pending;
    pending = null;
    const max = Number.isFinite(el.duration) ? Math.max(0, el.duration - 0.05) : Infinity;
    el.currentTime = Math.min(ms / 1000, max);
    // Autoplay can be refused when the seek did not come from a user gesture.
    if (play) void el.play().catch(() => undefined);
  }

  return {
    time,
    seek(ms, opts) {
      const target = Math.max(0, Math.floor(ms));
      pending = { ms: target, play: opts?.play ?? false };
      time.set(target);
      apply();
    },
    attach(next) {
      el = next;
      apply();
    },
    ready: apply,
    sync() {
      // While a seek is waiting for metadata the element still reports 0.
      if (el && !pending) time.set(Math.round(el.currentTime * 1000));
    },
    hold() {
      if (!pending && time.get() > 0) pending = { ms: time.get(), play: false };
    },
  };
}
