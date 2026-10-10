import { describe, expect, it } from 'vitest';
import { ApiError, decodeApiError } from './errors';

describe('decodeApiError', () => {
  it('decodes the engine error envelope', () => {
    const body = JSON.stringify({
      error: {
        code: 'MODEL_AUTH_FAILED',
        message: '在线模型鉴权失败（HTTP 401）',
        stage: 'note_map',
        retryable: true,
        user_action_required: true,
        hint: '在设置中更新 API Key 后重试该任务',
        details: { http_status: 401 },
      },
    });
    const e = decodeApiError(502, body);
    expect(e).toBeInstanceOf(ApiError);
    expect(e).toBeInstanceOf(Error);
    expect(e).toMatchObject({
      code: 'MODEL_AUTH_FAILED',
      message: '在线模型鉴权失败（HTTP 401）',
      status: 502,
      stage: 'note_map',
      retryable: true,
      userActionRequired: true,
      hint: '在设置中更新 API Key 后重试该任务',
      details: { http_status: 401 },
      unauthorized: false,
    });
  });

  it('defaults optional fields', () => {
    const e = decodeApiError(409, '{"error":{"code":"CONFLICT","message":"x","retryable":false,"user_action_required":true}}');
    expect(e).toMatchObject({ stage: null, hint: null, details: null, retryable: false });
  });

  it('flags 401 / UNAUTHORIZED', () => {
    expect(decodeApiError(401, '{"error":{"code":"UNAUTHORIZED","message":"no"}}').unauthorized).toBe(true);
    expect(decodeApiError(401, '').unauthorized).toBe(true);
    expect(decodeApiError(401, '').code).toBe('UNAUTHORIZED');
  });

  it.each([
    [500, '<html>Bad Gateway</html>', 'INTERNAL', true],
    [404, '', 'NOT_FOUND', false],
    [400, '{"error":"plain string"}', 'INTERNAL', false],
    [503, '{"error":{"message":"no code"}}', 'INTERNAL', true],
    [500, 'null', 'INTERNAL', true],
  ])('falls back for HTTP %i with body %s', (status, body, code, retryable) => {
    const e = decodeApiError(status, body);
    expect(e.code).toBe(code);
    expect(e.retryable).toBe(retryable);
    expect(e.message).toBe(`HTTP ${status}`);
  });
});
