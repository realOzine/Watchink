import { createDesktopPlatform, desktopBindings } from './desktop';
import type { Platform } from './types';
import { createWebPlatform } from './web';

export type * from './types';

function detect(): Platform {
  const app = desktopBindings();
  return app ? createDesktopPlatform(app) : createWebPlatform();
}

/** The Wails runtime injects its bindings before the app script runs, so detect once. */
export const platform: Platform = detect();
