import { describe, expect, it } from 'vitest';
import { SseParser } from './sse';

const collect = (chunks: string[]) => {
  const p = new SseParser();
  return { frames: chunks.flatMap((c) => p.push(c)), parser: p };
};

describe('SseParser', () => {
  it('parses a complete frame', () => {
    const { frames } = collect(['id: 7\nevent: job.updated\ndata: {"job_id":"job_1","rev":3}\n\n']);
    expect(frames).toEqual([{ id: '7', event: 'job.updated', data: '{"job_id":"job_1","rev":3}' }]);
  });

  it('gives the same result for every chunk boundary', () => {
    const text = 'id: 1\nevent: video.updated\ndata: {"video_id":"vid_1"}\n\nid: 2\nevent: tools.updated\ndata: {"items":[]}\n\n';
    const whole = collect([text]).frames;
    expect(whole).toHaveLength(2);
    for (let i = 1; i < text.length; i++) {
      expect(collect([text.slice(0, i), text.slice(i)]).frames).toEqual(whole);
    }
    expect(collect([...text]).frames).toEqual(whole);
  });

  it('ignores comment lines (heartbeats) and counts them', () => {
    const { frames, parser } = collect([': ping\n\n', ': ping\n\nevent: reset\ndata: {}\n\n']);
    expect(frames).toEqual([{ id: null, event: 'reset', data: '{}' }]);
    expect(parser.comments).toBe(2);
  });

  it('joins multi-line data with newlines', () => {
    const { frames } = collect(['event: job.updated\ndata: {"a":\ndata: 1}\n\n']);
    expect(frames[0]!.data).toBe('{"a":\n1}');
    expect(JSON.parse(frames[0]!.data)).toEqual({ a: 1 });
  });

  it('handles CRLF, also when split between CR and LF', () => {
    const { frames } = collect(['id: 3\r', '\nevent: reset\r\ndata: {}\r\n\r', '\n']);
    expect(frames).toEqual([{ id: '3', event: 'reset', data: '{}' }]);
  });

  it('does not dispatch frames without data and defaults the event name', () => {
    const { frames } = collect(['event: job.updated\n\n', 'data: x\n\n']);
    expect(frames).toEqual([{ id: null, event: 'message', data: 'x' }]);
  });

  it('keeps the last event id across frames for Last-Event-ID', () => {
    const { frames, parser } = collect(['id: 41\ndata: a\n\n', 'data: b\n\n']);
    expect(frames.map((f) => f.id)).toEqual(['41', '41']);
    expect(parser.lastEventId).toBe('41');
  });

  it('strips only one leading space and keeps colons in the value', () => {
    const { frames } = collect(['data:  a: b\n\n', 'data:no-space\n\n']);
    expect(frames.map((f) => f.data)).toEqual([' a: b', 'no-space']);
  });

  it('holds an unterminated frame until the blank line arrives', () => {
    const p = new SseParser();
    expect(p.push('event: reset\ndata: {}')).toEqual([]);
    expect(p.push('\n')).toEqual([]);
    expect(p.push('\n')).toEqual([{ id: null, event: 'reset', data: '{}' }]);
  });
});
