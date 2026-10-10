import { useEffect, useId, useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { Link } from 'react-router';
import { flushListRefresh } from '../api/events';
import { keys } from '../api/keys';
import { useVideos } from '../api/hooks';
import { Page } from '../components/AppShell';
import { ImportDialog } from '../components/ImportDialog';
import { Button, EmptyState, ErrorState, Loading, StatusBadge, cx, inputClass } from '../components/ui';
import { zh } from '../i18n/zh';
import { formatDuration, formatInstant } from '../lib/time';
import s from './pages.module.css';

const t = zh.library;

export function LibraryPage() {
  const [input, setInput] = useState('');
  const [filter, setFilter] = useState('');
  const [importing, setImporting] = useState(false);
  const searchId = useId();

  // Debounce so typing does not fire one request per keystroke.
  useEffect(() => {
    const timer = setTimeout(() => setFilter(input.trim()), 250);
    return () => clearTimeout(timer);
  }, [input]);

  const qc = useQueryClient();
  const q = useVideos(filter);
  const videos = q.data?.pages.flatMap((p) => p.videos ?? []) ?? [];

  const importButton = (
    <Button variant="primary" onClick={() => setImporting(true)}>
      {t.import}
    </Button>
  );

  let body;
  if (q.isPending) body = <Loading />;
  else if (q.isError) body = <ErrorState error={q.error} onRetry={() => void q.refetch()} />;
  else if (videos.length === 0) {
    body = filter ? (
      <EmptyState title={t.emptyFilter(filter)}>
        <Button onClick={() => setInput('')}>{t.clearFilter}</Button>
      </EmptyState>
    ) : (
      <EmptyState title={t.emptyTitle} body={t.emptyBody}>
        {importButton}
      </EmptyState>
    );
  } else {
    body = (
      <>
        <div className={cx(s.card, s.tableWrap)}>
          <table className={s.table}>
            <thead>
              <tr>
                <th scope="col">{t.columns.title}</th>
                <th scope="col">{t.columns.status}</th>
                <th scope="col">{t.columns.source}</th>
                <th scope="col">{t.columns.duration}</th>
                <th scope="col" className={s.hideNarrow}>
                  {t.columns.created}
                </th>
              </tr>
            </thead>
            <tbody>
              {videos.map((v) => (
                <tr key={v.video_id} data-video-id={v.video_id}>
                  <td className={s.titleCell}>
                    <Link className={s.titleLink} to={`/videos/${encodeURIComponent(v.video_id)}`}>
                      {v.title || t.untitled}
                    </Link>
                  </td>
                  <td>
                    <StatusBadge status={v.status} labels={zh.status.video} />
                  </td>
                  <td className={s.nowrap}>{zh.source[v.source.type] ?? v.source.type}</td>
                  <td className={s.nowrap}>{formatDuration(v.duration_ms)}</td>
                  <td className={cx(s.nowrap, s.hideNarrow)}>{formatInstant(v.created_at)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
        <div className={s.more}>
          <span>{t.count(videos.length)}</span>
          {q.hasNextPage && (
            <Button disabled={q.isFetchingNextPage} onClick={() => void q.fetchNextPage().finally(() => flushListRefresh(qc, keys.videosAll))}>
              {q.isFetchingNextPage ? t.loadingMore : t.loadMore}
            </Button>
          )}
        </div>
      </>
    );
  }

  return (
    <Page title={t.title} actions={importButton}>
      <div className={s.toolbar}>
        <label className="visually-hidden" htmlFor={searchId}>
          {t.filterLabel}
        </label>
        <input
          id={searchId}
          className={cx(inputClass, s.search)}
          type="search"
          placeholder={t.filterPlaceholder}
          value={input}
          onChange={(e) => setInput(e.target.value)}
        />
      </div>
      {body}
      <ImportDialog open={importing} onClose={() => setImporting(false)} />
    </Page>
  );
}
