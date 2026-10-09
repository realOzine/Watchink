import { describe, expect, it } from 'vitest';
import { findSegmentIndex, formatDuration, formatMs, parseTimeParam } from './time';

describe('formatMs', () => {
  it('uses MM:SS for videos shorter than an hour', () => {
    expect(formatMs(0, 600_000)).toBe('00:00');
    expect(formatMs(192_999, 600_000)).toBe('03:12');
    expect(formatMs(3_599_000, 3_599_999)).toBe('59:59');
  });
  it('uses H:MM:SS once the video is an hour or longer', () => {
    expect(formatMs(83_000, 3_600_000)).toBe('0:01:23');
    expect(formatMs(3_753_000, 3_753_000)).toBe('1:02:33');
  });
  it('switches to hours when the time itself needs them', () => {
    expect(formatMs(3_661_000)).toBe('1:01:01');
    expect(formatMs(61_000)).toBe('01:01');
    expect(formatMs(61_000, null)).toBe('01:01');
  });
  it('clamps invalid input to zero', () => {
    expect(formatMs(-5)).toBe('00:00');
    expect(formatMs(Number.NaN)).toBe('00:00');
  });
});

describe('formatDuration', () => {
  it.each([
    [null, '—'],
    [45_000, '45 秒'],
    [725_000, '12 分 05 秒'],
    [3_753_000, '1 小时 02 分'],
  ])('%s → %s', (ms, out) => expect(formatDuration(ms)).toBe(out));
});

describe('parseTimeParam', () => {
  it.each([
    ['83000', 83000],
    ['0', 0],
    [null, null],
    ['', null],
    ['12s', null],
    ['-4', null],
    ['1.5', null],
  ])('%s → %s', (raw, out) => expect(parseTimeParam(raw)).toBe(out));
});

describe('findSegmentIndex', () => {
  const segs = [{ start_ms: 0 }, { start_ms: 1000 }, { start_ms: 5000 }];
  it.each([
    [0, 0],
    [999, 0],
    [1000, 1],
    [4999, 1],
    [9_999_999, 2],
  ])('at %i → %i', (ms, i) => expect(findSegmentIndex(segs, ms)).toBe(i));
  it('returns -1 before the first segment and for no segments', () => {
    expect(findSegmentIndex([{ start_ms: 500 }], 100)).toBe(-1);
    expect(findSegmentIndex([], 100)).toBe(-1);
  });
});
