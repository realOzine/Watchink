/** One dispatched server-sent event. */
export interface SseFrame {
  id: string | null;
  event: string;
  data: string;
}

/**
 * Incremental parser for `text/event-stream` bodies read through fetch.
 * Feed decoded text chunks of any size; complete frames are returned in order.
 * Follows the WHATWG rules the engine relies on: CRLF / LF / CR line ends,
 * `:` comment lines (heartbeats), multi-line `data:` joined with "\n", and a
 * blank line dispatching the frame. Frames without data are not dispatched.
 */
export class SseParser {
  private buffer = '';
  private id: string | null = null;
  private event = '';
  private data: string[] = [];
  /** Last `id:` seen, kept across frames for `Last-Event-ID`. */
  lastEventId: string | null = null;
  /** Number of comment lines seen (heartbeats); useful for liveness checks. */
  comments = 0;

  push(chunk: string): SseFrame[] {
    this.buffer += chunk;
    const frames: SseFrame[] = [];
    for (;;) {
      const m = /\r\n|\n|\r/.exec(this.buffer);
      if (!m) break;
      // A lone "\r" at the very end may be the first half of a split "\r\n".
      if (m[0] === '\r' && m.index === this.buffer.length - 1) break;
      const line = this.buffer.slice(0, m.index);
      this.buffer = this.buffer.slice(m.index + m[0].length);
      const frame = this.line(line);
      if (frame) frames.push(frame);
    }
    return frames;
  }

  private line(line: string): SseFrame | null {
    if (line === '') {
      const frame: SseFrame | null =
        this.data.length > 0
          ? { id: this.id ?? this.lastEventId, event: this.event || 'message', data: this.data.join('\n') }
          : null;
      this.id = null;
      this.event = '';
      this.data = [];
      return frame;
    }
    if (line.startsWith(':')) {
      this.comments++;
      return null;
    }
    const colon = line.indexOf(':');
    const field = colon === -1 ? line : line.slice(0, colon);
    let value = colon === -1 ? '' : line.slice(colon + 1);
    if (value.startsWith(' ')) value = value.slice(1);
    switch (field) {
      case 'id':
        if (!value.includes('\0')) {
          this.id = value;
          this.lastEventId = value;
        }
        break;
      case 'event':
        this.event = value;
        break;
      case 'data':
        this.data.push(value);
        break;
      default:
        break; // `retry:` and unknown fields are ignored
    }
    return null;
  }
}
