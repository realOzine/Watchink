import { ApiError, NetworkError } from '../api/errors';
import type { Error as WireError } from '../api/types';
import { zh } from '../i18n/zh';

export interface ErrorView {
  code: string | null;
  /** Short label for the error code. */
  title: string;
  message: string;
  hint: string | null;
  userActionRequired: boolean;
  retryable: boolean;
  stage: string | null;
}

/** Normalises thrown errors and wire error objects into what the UI shows. */
export function describeError(e: unknown): ErrorView {
  if (e instanceof ApiError) {
    return {
      code: e.code,
      title: zh.errors.codes[e.code] ?? e.code,
      message: e.message,
      hint: e.hint,
      userActionRequired: e.userActionRequired,
      retryable: e.retryable,
      stage: e.stage,
    };
  }
  if (e instanceof NetworkError) {
    return {
      code: null,
      title: zh.errors.codes.ENGINE_UNAVAILABLE ?? '',
      message: zh.errors.network,
      hint: null,
      userActionRequired: false,
      retryable: true,
      stage: null,
    };
  }
  if (typeof e === 'object' && e !== null && 'code' in e && 'message' in e) {
    const w = e as WireError;
    return {
      code: w.code,
      title: zh.errors.codes[w.code] ?? w.code,
      message: w.message,
      hint: w.hint ?? null,
      userActionRequired: w.user_action_required === true,
      retryable: w.retryable === true,
      stage: w.stage ?? null,
    };
  }
  return {
    code: null,
    title: zh.errors.unknown,
    message: e instanceof Error && e.message ? e.message : zh.errors.unknown,
    hint: null,
    userActionRequired: false,
    retryable: false,
    stage: null,
  };
}

/** Errors a user fixes in settings: model configuration and missing tools. */
export function needsSettings(view: ErrorView): boolean {
  if (!view.userActionRequired || !view.code) return false;
  return view.code.startsWith('MODEL_') || view.code === 'TOOL_MISSING';
}
