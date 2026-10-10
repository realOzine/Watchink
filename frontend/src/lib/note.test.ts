import { defaultUrlTransform } from 'react-markdown';
import { describe, expect, it } from 'vitest';
import type { Citation } from '../api/types';
import { classifyLink, indexCitations, noteUrlTransform, parseSegmentLink, stripFrontMatter } from './note';

describe('stripFrontMatter', () => {
  it('removes the YAML block and the blank lines after it', () => {
    const md = '---\nwatchink:\n  video_id: vid_1\n  duration: "1:02:33"\n---\n\n# 标题\n\n正文';
    expect(stripFrontMatter(md)).toBe('# 标题\n\n正文');
  });
  it('handles CRLF and a BOM', () => {
    expect(stripFrontMatter('﻿---\r\na: 1\r\n---\r\n# T')).toBe('# T');
  });
  it('leaves notes without front matter untouched', () => {
    expect(stripFrontMatter('# 标题\n\n---\n\n正文')).toBe('# 标题\n\n---\n\n正文');
  });
  it('does not treat an unterminated block as front matter', () => {
    expect(stripFrontMatter('---\na: 1\n# T')).toBe('---\na: 1\n# T');
  });
  it('only strips the first block', () => {
    expect(stripFrontMatter('---\na: 1\n---\nx\n---\nb\n---\n')).toBe('x\n---\nb\n---\n');
  });
});

describe('citation links', () => {
  it('extracts the segment id', () => {
    expect(parseSegmentLink('wk://seg/seg_01JA2M8Q_00042')).toBe('seg_01JA2M8Q_00042');
  });
  it.each(['wk://seg/', 'wk://seg/../x', 'wk://seg/seg_1?x=1', 'wk://other/seg_1', 'https://a.b/wk://seg/seg_1', '', null])(
    'rejects %s',
    (href) => expect(parseSegmentLink(href)).toBeNull(),
  );

  it('classifies links by the allow-list', () => {
    expect(classifyLink('wk://seg/seg_A_00001')).toBe('segment');
    expect(classifyLink('https://example.com/a?b=1')).toBe('external');
    expect(classifyLink('HTTP://example.com')).toBe('external');
    for (const bad of ['javascript:alert(1)', 'data:text/html,x', 'file:///etc/passwd', 'wk://cmd/run', '/relative', '#frag', 'mailto:a@b.c', 'https://', undefined]) {
      expect(classifyLink(bad)).toBe('blocked');
    }
  });

  it('keeps wk://seg/ where the react-markdown default would drop it', () => {
    const href = 'wk://seg/seg_A_00001';
    expect(defaultUrlTransform(href)).toBe('');
    expect(noteUrlTransform(href)).toBe(href);
    expect(noteUrlTransform('https://example.com')).toBe('https://example.com');
    expect(noteUrlTransform('javascript:alert(1)')).toBe('');
  });

  it('indexes citations by segment, first one wins', () => {
    const c = (ref_no: number, segment_id: string, start_ms: number): Citation => ({
      ref_no,
      point_id: `p${ref_no}`,
      segment_id,
      start_ms,
      end_ms: start_ms + 1000,
      text: 't',
    });
    const map = indexCitations([c(1, 'seg_a', 10), c(2, 'seg_b', 20), c(3, 'seg_a', 10)]);
    expect(map.size).toBe(2);
    expect(map.get('seg_a')!.ref_no).toBe(1);
    expect(indexCitations(null).size).toBe(0);
  });
});
