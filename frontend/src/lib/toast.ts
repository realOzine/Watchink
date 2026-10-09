import { createStore } from './store';

export type ToastKind = 'error' | 'success' | 'info';

export interface Toast {
  id: number;
  kind: ToastKind;
  message: string;
  detail?: string;
  action?: { label: string; to?: string; onClick?: () => void };
}

export const toastStore = createStore<Toast[]>([]);
let seq = 0;

export function dismissToast(id: number): void {
  toastStore.set((list) => list.filter((t) => t.id !== id));
}

export function pushToast(toast: Omit<Toast, 'id'>): number {
  // The same failure often arrives several times in a row (retries, several
  // components); one toast is enough.
  const dup = toastStore.get().find((t) => t.kind === toast.kind && t.message === toast.message);
  if (dup) return dup.id;
  const id = ++seq;
  toastStore.set((list) => [...list.slice(-3), { ...toast, id }]);
  const ttl = toast.kind === 'error' ? 9000 : 4500;
  setTimeout(() => dismissToast(id), ttl);
  return id;
}
