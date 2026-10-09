import { useState } from 'react';
import { useJob, usePrepareTools, useTools, useUpdateTool } from '../api/hooks';
import { isJobActive } from '../api/keys';
import { zh } from '../i18n/zh';
import { formatBytes, formatPercent } from '../lib/format';
import { pushToast } from '../lib/toast';
import s from '../pages/pages.module.css';
import { Badge, Button, ErrorNotice, ErrorState, Loading, Notice, ProgressBar, StatusBadge, cx } from './ui';

const t = zh.settings.tools;

/**
 * Tools & models status with prepare / update actions. Rows update live from
 * `tools.updated` events; the prepare job (if we started one) adds its error.
 */
export function ToolsPanel({ showUpdate = true, showImportHint = true }: { showUpdate?: boolean; showImportHint?: boolean }) {
  const tools = useTools();
  const prepare = usePrepareTools();
  const update = useUpdateTool();
  const [jobId, setJobId] = useState<string | null>(null);
  const job = useJob(jobId).data;

  if (tools.isPending) return <Loading />;
  if (tools.isError) return <ErrorState error={tools.error} onRetry={() => void tools.refetch()} />;

  const data = tools.data;
  const items = data.items ?? [];
  const downloading = items.some((i) => i.status === 'downloading') || isJobActive(job?.status) || prepare.isPending;
  const start = (names: string[]) =>
    prepare.mutate(names, {
      onSuccess: (j) => {
        setJobId(j.job_id);
        pushToast({ kind: 'info', message: t.started });
      },
    });
  const hasYtDlp = items.some((i) => i.name === 'yt-dlp');

  return (
    <div className={s.stack} data-testid="tools-panel">
      {data.ready ? (
        <Notice tone="ok">{t.allReady}</Notice>
      ) : (
        <Notice tone="warn">{t.missing(formatBytes(data.missing_download_bytes))}</Notice>
      )}
      <div className={cx(s.card, s.tableWrap)}>
        <table className={s.table}>
          <thead>
            <tr>
              <th scope="col">{t.columns.name}</th>
              <th scope="col" className={s.hideNarrow}>
                {t.columns.kind}
              </th>
              <th scope="col" className={s.hideNarrow}>
                {t.columns.version}
              </th>
              <th scope="col">{t.columns.status}</th>
              <th scope="col">{t.columns.size}</th>
              <th scope="col">
                <span className="visually-hidden">操作</span>
              </th>
            </tr>
          </thead>
          <tbody>
            {items.map((item) => (
              <tr key={item.name} data-tool={item.name} data-status={item.status}>
                <td>
                  <span className={s.toolName}>{item.name}</span>
                  <span className={s.toolTags}>
                    {item.embedded && <Badge plain>{t.embedded}</Badge>}
                    {item.lazy && <Badge plain>{t.lazy}</Badge>}
                    {item.required && !item.embedded && (
                      <Badge plain tone="info">
                        {t.required}
                      </Badge>
                    )}
                  </span>
                </td>
                <td className={cx(s.nowrap, s.hideNarrow)}>{t.kind[item.kind] ?? item.kind}</td>
                <td className={cx(s.nowrap, s.hideNarrow)}>{item.version || zh.app.none}</td>
                <td>
                  {item.status === 'downloading' ? (
                    <div className={s.toolProgress}>
                      <div className={s.muted}>
                        {zh.status.tool.downloading} {formatPercent(item.progress)}
                      </div>
                      <ProgressBar value={item.progress ?? null} label={`${item.name} ${t.jobProgress}`} />
                    </div>
                  ) : (
                    <StatusBadge status={item.status} labels={zh.status.tool} />
                  )}
                  {item.error && <div className={s.muted}>{item.error}</div>}
                </td>
                <td className={s.nowrap}>
                  {formatBytes(item.status === 'ready' ? item.size_bytes : item.download_bytes)}
                </td>
                <td>
                  {(item.status === 'missing' || item.status === 'failed') && !item.embedded && (
                    <Button small disabled={downloading} onClick={() => start([item.name])}>
                      {t.prepareOne}
                    </Button>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {job?.error && <ErrorNotice error={job.error} />}
      {prepare.isError && <ErrorNotice error={prepare.error} />}
      {update.isError && <ErrorNotice error={update.error} />}
      <div className={s.row}>
        {!data.ready && (
          <Button variant="primary" disabled={downloading} onClick={() => start([])}>
            {downloading ? t.preparing : t.prepareAll(formatBytes(data.missing_download_bytes))}
          </Button>
        )}
        {showUpdate && hasYtDlp && (
          <Button
            disabled={update.isPending}
            onClick={() => update.mutate('yt-dlp', { onSuccess: () => pushToast({ kind: 'success', message: t.updated }) })}
          >
            {update.isPending ? t.updating : t.updateYtDlp}
          </Button>
        )}
      </div>
      {showImportHint && (
        <p className={s.muted}>
          {t.importHint} <code>{t.importCmd}</code>
        </p>
      )}
    </div>
  );
}
