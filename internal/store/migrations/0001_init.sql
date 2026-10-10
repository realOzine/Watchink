-- Schema version 1: every table of a data directory, with the secondary
-- indexes at the bottom.

CREATE TABLE videos (
  id TEXT PRIMARY KEY,
  source_type TEXT NOT NULL,              -- file / upload / youtube / bilibili
  source_key TEXT NOT NULL UNIQUE,        -- dedup key
  source_uri TEXT,
  title TEXT NOT NULL,
  duration_ms INTEGER,
  language TEXT,
  current_transcript_id TEXT,
  current_note_id TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE artifacts (
  id TEXT PRIMARY KEY,
  video_id TEXT NOT NULL REFERENCES videos(id),
  kind TEXT NOT NULL,     -- source / probe / audio / asr_raw / playback / map_chunk / reduce / note_md / note_json / export
  fingerprint TEXT NOT NULL,
  path TEXT,              -- relative to <data>
  sha256 TEXT,
  size_bytes INTEGER,
  purged INTEGER NOT NULL DEFAULT 0,
  meta TEXT NOT NULL DEFAULT '{}',
  created_at TEXT NOT NULL,
  UNIQUE (kind, fingerprint)
);

CREATE TABLE jobs (
  id TEXT PRIMARY KEY,
  kind TEXT NOT NULL,                     -- process_video / prepare_tools
  video_id TEXT REFERENCES videos(id),
  status TEXT NOT NULL,
  rev INTEGER NOT NULL DEFAULT 1,
  attempt INTEGER NOT NULL DEFAULT 1,
  options TEXT NOT NULL DEFAULT '{}',
  requested_by TEXT,
  error TEXT,
  created_at TEXT NOT NULL,
  started_at TEXT,
  finished_at TEXT,
  updated_at TEXT NOT NULL,
  result TEXT NOT NULL DEFAULT '{}'       -- {transcript_id, note_id}
);

CREATE TABLE job_stages (
  job_id TEXT NOT NULL REFERENCES jobs(id),
  stage TEXT NOT NULL,
  status TEXT NOT NULL,
  progress REAL NOT NULL DEFAULT 0,
  fingerprint TEXT,
  artifact_id TEXT,
  reused INTEGER NOT NULL DEFAULT 0,
  attempt INTEGER NOT NULL DEFAULT 0,
  error TEXT,
  started_at TEXT,
  finished_at TEXT,
  PRIMARY KEY (job_id, stage)
);

CREATE TABLE job_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  job_id TEXT NOT NULL,
  at TEXT NOT NULL,
  type TEXT NOT NULL,
  data TEXT NOT NULL DEFAULT '{}'
);

CREATE TABLE transcripts (
  id TEXT PRIMARY KEY,
  video_id TEXT NOT NULL REFERENCES videos(id),
  artifact_id TEXT NOT NULL,
  language TEXT NOT NULL,
  engine TEXT NOT NULL,
  model TEXT NOT NULL,
  params TEXT NOT NULL,
  timeline_offset_ms INTEGER NOT NULL DEFAULT 0,
  segment_count INTEGER NOT NULL,
  meta TEXT NOT NULL DEFAULT '{}',
  created_at TEXT NOT NULL
);

CREATE TABLE segments (
  id TEXT PRIMARY KEY,
  transcript_id TEXT NOT NULL REFERENCES transcripts(id),
  idx INTEGER NOT NULL,
  start_ms INTEGER NOT NULL,
  end_ms INTEGER NOT NULL,
  text TEXT NOT NULL,
  UNIQUE (transcript_id, idx)
);
CREATE INDEX segments_by_time ON segments (transcript_id, start_ms);

CREATE TABLE notes (
  id TEXT PRIMARY KEY,
  video_id TEXT NOT NULL REFERENCES videos(id),
  transcript_id TEXT NOT NULL REFERENCES transcripts(id),
  version INTEGER NOT NULL,
  title TEXT NOT NULL,
  md_artifact_id TEXT NOT NULL,
  json_artifact_id TEXT NOT NULL,
  llm_provider TEXT NOT NULL,
  llm_model TEXT NOT NULL,
  prompt_version TEXT NOT NULL,
  coverage TEXT NOT NULL,
  created_by_job TEXT NOT NULL,
  created_at TEXT NOT NULL,
  UNIQUE (video_id, version)
);

CREATE TABLE citations (
  note_id TEXT NOT NULL REFERENCES notes(id),
  ref_no INTEGER NOT NULL,
  point_id TEXT NOT NULL,
  segment_id TEXT NOT NULL REFERENCES segments(id),
  start_ms INTEGER NOT NULL,
  end_ms INTEGER NOT NULL,
  PRIMARY KEY (note_id, ref_no)
);

CREATE TABLE uploads (
  id TEXT PRIMARY KEY,
  filename TEXT NOT NULL,
  size_bytes INTEGER NOT NULL,
  sample_hash TEXT NOT NULL,
  path TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  consumed_by TEXT
);

CREATE TABLE exports (
  id TEXT PRIMARY KEY,
  video_id TEXT NOT NULL,
  note_id TEXT,
  format TEXT NOT NULL,
  path TEXT NOT NULL,
  sha256 TEXT NOT NULL,
  created_at TEXT NOT NULL
);

CREATE TABLE child_processes (
  pid INTEGER NOT NULL,
  job_id TEXT,
  stage TEXT,
  exe TEXT NOT NULL,
  started_at TEXT NOT NULL
);

-- Listings are ordered by id: IDs are monotonic ULIDs, so id order is
-- creation order, while created_at strings (RFC 3339 with trimmed fractional
-- digits) do not sort lexicographically.
CREATE INDEX jobs_by_status ON jobs (status, id);
CREATE INDEX jobs_by_video ON jobs (video_id, id);
CREATE INDEX jobs_by_kind_status ON jobs (kind, status);
CREATE INDEX artifacts_by_video ON artifacts (video_id, kind);
CREATE INDEX job_events_by_job ON job_events (job_id, id);
CREATE INDEX transcripts_by_video ON transcripts (video_id, id);
CREATE INDEX exports_by_video ON exports (video_id, id);
CREATE INDEX child_processes_by_pid ON child_processes (pid);
