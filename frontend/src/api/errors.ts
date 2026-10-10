import type { Error as WireError } from './types';

/** A failed API call, carrying the engine's error object. */
export class ApiError extends Error {
  readonly code: string;
  readonly status: number;
  readonly stage: string | null;
  readonly retryable: boolean;
  readonly userActionRequired: boolean;
  readonly hint: string | null;
  readonly details: Record<string, unknown> | null;

  constructor(wire: Partial<WireError> & { code: string; message: string }, status: number) {
    super(wire.message);
    this.name = 'ApiError';
    this.code = wire.code;
    this.status = status;
    this.stage = wire.stage ?? null;
    this.retryable = wire.retryable ?? false;
    this.userActionRequired = wire.user_action_required ?? false;
    this.hint = wire.hint ?? null;
    this.details = (wire.details as Record<string, unknown> | null | undefined) ?? null;
  }

  get unauthorized(): boolean {
    return this.code === 'UNAUTHORIZED' || this.status === 401;
  }
}

/** The request never produced an HTTP response (engine down, CORS, offline). */
export class NetworkError extends Error {
  constructor(cause?: unknown) {
    super('network error');
    this.name = 'NetworkError';
    this.cause = cause;
  }
}

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}

/**
 * Builds an ApiError from a non-2xx response body. Bodies that are not the
 * `{"error": {...}}` envelope (proxy pages, empty bodies) map to a synthetic
 * code so callers always get the same shape.
 */
export function decodeApiError(status: number, bodyText: string): ApiError {
  let parsed: unknown;
  try {
    parsed = bodyText ? JSON.parse(bodyText) : undefined;
  } catch {
    parsed = undefined;
  }
  const e = isRecord(parsed) ? parsed.error : undefined;
  if (isRecord(e) && typeof e.code === 'string') {
    return new ApiError(
      {
        code: e.code,
        message: typeof e.message === 'string' && e.message ? e.message : e.code,
        stage: typeof e.stage === 'string' ? e.stage : null,
        retryable: e.retryable === true,
        user_action_required: e.user_action_required === true,
        hint: typeof e.hint === 'string' ? e.hint : null,
        details: isRecord(e.details) ? e.details : null,
      },
      status,
    );
  }
  const code = status === 401 ? 'UNAUTHORIZED' : status === 404 ? 'NOT_FOUND' : 'INTERNAL';
  return new ApiError({ code, message: `HTTP ${status}`, retryable: status >= 500 }, status);
}
