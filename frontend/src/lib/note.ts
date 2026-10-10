import type { Citation } from '../api/types';

export const SEG_SCHEME = 'wk://seg/';

/** Removes the leading YAML front matter block of a canonical note. */
export function stripFrontMatter(markdown: string): string {
  const text = markdown.replace(/^﻿/, '');
  const m = /^---[ \t]*\r?\n[\s\S]*?\r?\n---[ \t]*(?:\r?\n|$)/.exec(text);
  if (!m) return text;
  return text.slice(m[0].length).replace(/^(?:\r?\n)+/, '');
}

/** Segment ID of an `wk://seg/<id>` link, or null for anything else. */
export function parseSegmentLink(href: string | null | undefined): string | null {
  if (!href || !href.startsWith(SEG_SCHEME)) return null;
  const id = href.slice(SEG_SCHEME.length);
  return /^seg_[0-9A-Za-z_]+$/.test(id) ? id : null;
}

export type LinkKind = 'segment' | 'external' | 'blocked';

/** Only wk://seg/, https:// and http:// are ever rendered as links. */
export function classifyLink(href: string | null | undefined): LinkKind {
  if (!href) return 'blocked';
  if (parseSegmentLink(href) != null) return 'segment';
  if (/^https?:\/\//i.test(href)) {
    try {
      const u = new URL(href);
      return u.protocol === 'https:' || u.protocol === 'http:' ? 'external' : 'blocked';
    } catch {
      return 'blocked';
    }
  }
  return 'blocked';
}

/**
 * `urlTransform` for react-markdown: its default drops unknown schemes, which
 * would erase wk://seg/ citations. Everything not on the allow-list becomes ''.
 */
export function noteUrlTransform(url: string): string {
  return classifyLink(url) === 'blocked' ? '' : url;
}

/** First citation per segment ID; all citations of a segment share its times. */
export function indexCitations(citations: readonly Citation[] | null | undefined): Map<string, Citation> {
  const map = new Map<string, Citation>();
  for (const c of citations ?? []) {
    if (!map.has(c.segment_id)) map.set(c.segment_id, c);
  }
  return map;
}
