import type { ReactNode } from 'react';
import type { FullNote, FullTranscript } from '../api';
import type { Video } from '../api/types';
import { zh } from '../i18n/zh';
import { formatDuration, formatInstant } from '../lib/time';
import { platform } from '../platform';
import s from '../pages/VideoDetail.module.css';

const t = zh.video.info;
const f = t.fields;

function Rows({ rows }: { rows: [string, ReactNode][] }) {
  return (
    <dl className={s.dl}>
      {rows.map(([k, v]) => (
        <div key={k} style={{ display: 'contents' }}>
          <dt>{k}</dt>
          <dd>{v == null || v === '' ? zh.app.none : v}</dd>
        </div>
      ))}
    </dl>
  );
}

const Mono = ({ children }: { children: ReactNode }) => <span className={s.mono}>{children}</span>;

export function InfoPanel({
  video,
  note,
  transcript,
}: {
  video: Video;
  note: FullNote | undefined;
  transcript: FullTranscript | undefined;
}) {
  const c = note?.coverage;
  const uri = video.source.uri;
  const isLink = !!uri && /^https?:\/\//i.test(uri);
  return (
    <div className={s.info}>
      {c && (
        <section>
          <h2 className={s.infoTitle}>{t.coverage}</h2>
          <p className={s.infoHint}>{t.coverageHint}</p>
          <Rows
            rows={[
              [f.transcript_segments, c.transcript_segments],
              [f.transcript_chars, c.transcript_chars],
              [f.chunks, c.chunks],
              [f.chunks_empty, c.chunks_empty],
              [f.points, c.points],
              [f.points_dropped, c.points_dropped],
              [f.points_unplaced, c.points_unplaced],
              [f.citations, c.citations],
              [f.citations_dropped, c.citations_dropped],
              [f.repair_calls, c.repair_calls],
              [f.split_retries, c.split_retries],
              [f.overview_fallback, c.overview_fallback ? t.yes : t.no],
            ]}
          />
        </section>
      )}
      {note && (
        <section>
          <h2 className={s.infoTitle}>{t.generation}</h2>
          <Rows
            rows={[
              [f.model, note.llm_model],
              [f.provider, note.llm_provider],
              [f.prompt_version, note.prompt_version],
              [f.note_version, note.version],
              [f.generated_at, formatInstant(note.created_at)],
            ]}
          />
        </section>
      )}
      {transcript && (
        <section>
          <h2 className={s.infoTitle}>{t.transcript}</h2>
          <Rows
            rows={[
              [f.engine, transcript.engine],
              [f.asr_model, transcript.model],
              [f.language, zh.language[transcript.language] ?? transcript.language],
              [f.segment_count, transcript.segment_count],
            ]}
          />
        </section>
      )}
      <section>
        <h2 className={s.infoTitle}>{t.source}</h2>
        <Rows
          rows={[
            [f.source_type, zh.source[video.source.type] ?? video.source.type],
            [
              f.source_uri,
              isLink ? (
                <button type="button" className={s.linkButton} onClick={() => platform.openExternal(uri)}>
                  {uri}
                </button>
              ) : (
                uri
              ),
            ],
            [f.duration, formatDuration(video.duration_ms)],
            [f.created_at, formatInstant(video.created_at)],
          ]}
        />
      </section>
      <section>
        <h2 className={s.infoTitle}>{t.identifiers}</h2>
        <Rows
          rows={[
            [f.video_id, <Mono>{video.video_id}</Mono>],
            [f.note_id, video.note_id && <Mono>{video.note_id}</Mono>],
            [f.transcript_id, video.transcript_id && <Mono>{video.transcript_id}</Mono>],
            [f.job_id, video.latest_job_id && <Mono>{video.latest_job_id}</Mono>],
          ]}
        />
      </section>
    </div>
  );
}
