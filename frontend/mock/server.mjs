// Dev mock of the Watchink engine API — plain Node, no deps.
//
//   node mock/server.mjs [--port 47990] [--dist ../internal/webui/dist]
//                        [--fresh] [--require-auth] [--page-chars 20000] [--speed 1]
//
//   --dist          also serve the built SPA with history fallback (like the engine)
//   --fresh         start with missing tools and no online model (exercises /setup)
//   --require-auth  answer 401 UNAUTHORIZED to every API call (exercises the notice)
//   --page-chars    default max_chars of the note endpoint (small values force paging)
//   --speed         time multiplier for simulated jobs (2 = twice as fast)
//
// It keeps everything in memory. Media is a generated silent WAV (no ffmpeg
// needed), so seeking and time tracking are real even though there is no picture.
import { createReadStream, existsSync, statSync } from 'node:fs';
import http from 'node:http';
import { extname, join, normalize, resolve } from 'node:path';

const args = process.argv.slice(2);
const flag = (name) => args.includes(`--${name}`);
const opt = (name, fallback) => {
  const i = args.indexOf(`--${name}`);
  return i >= 0 && args[i + 1] ? args[i + 1] : fallback;
};
const PORT = Number(opt('port', '47990'));
const DIST = opt('dist', '') ? resolve(opt('dist', '')) : '';
const FRESH = flag('fresh');
const REQUIRE_AUTH = flag('require-auth');
const PAGE_CHARS = Number(opt('page-chars', '20000'));
const SPEED = Number(opt('speed', '1')) || 1;
const BASE_URL = `http://127.0.0.1:${PORT}`;
const STARTED_AT = new Date().toISOString();

// ---------------------------------------------------------------- helpers

let ulidSeq = 0;
const ULID_CHARS = '0123456789ABCDEFGHJKMNPQRSTVWXYZ';
function ulid() {
  // BigInt: milliseconds * 1e6 no longer fits a double, and IDs must be unique.
  let n = BigInt(Date.now()) * 1000000n + BigInt(ulidSeq++ % 1000000);
  let out = '';
  for (let i = 0; i < 26; i++) {
    out = ULID_CHARS[Number(n % 32n)] + out;
    n /= 32n;
  }
  return out;
}
const now = () => new Date().toISOString();
const ago = (minutes) => new Date(Date.now() - minutes * 60_000).toISOString();

const ERROR_FLAGS = {
  INVALID_ARGUMENT: [false, true, 400],
  NOT_FOUND: [false, true, 404],
  CONFLICT: [false, true, 409],
  UNSUPPORTED_SOURCE: [false, true, 400],
  MODEL_NOT_CONFIGURED: [true, true, 412],
  MODEL_AUTH_FAILED: [true, true, 502],
  UNAUTHORIZED: [false, true, 401],
  INTERNAL: [true, false, 500],
};
function err(code, message, extra = {}) {
  const [retryable, user] = ERROR_FLAGS[code] ?? ERROR_FLAGS.INTERNAL;
  return { code, message, retryable, user_action_required: user, ...extra };
}
class HttpError extends Error {
  constructor(error) {
    super(error.message);
    this.error = error;
    this.status = (ERROR_FLAGS[error.code] ?? ERROR_FLAGS.INTERNAL)[2];
  }
}
const fail = (code, message, extra) => {
  throw new HttpError(err(code, message, extra));
};

function fmt(ms, long) {
  const t = Math.floor(ms / 1000);
  const h = Math.floor(t / 3600);
  const m = Math.floor((t % 3600) / 60);
  const s = t % 60;
  const p = (n) => String(n).padStart(2, '0');
  return long ? `${h}:${p(m)}:${p(s)}` : `${p(m)}:${p(s)}`;
}

// ---------------------------------------------------------------- fixtures

const SENTENCES = [
  '今天这一讲我们从梯度下降的直觉开始讲起。',
  '先回顾一下上节课的内容，损失函数衡量的是预测和真实值之间的差距。',
  '学习率太大，参数会在最优点附近来回震荡，甚至发散。',
  '学习率太小，收敛会非常慢，训练成本也会明显上升。',
  '所以实践中通常会配合学习率预热和衰减策略。',
  '我们来看这张图，横轴是迭代次数，纵轴是训练损失。',
  '可以看到，使用动量之后，曲线明显平滑了很多。',
  '动量可以理解为给参数更新加上了惯性。',
  'Adam 把动量和自适应学习率这两个想法结合在了一起。',
  '注意，这里的偏差修正在训练初期非常重要。',
  '有同学问，为什么不直接用二阶方法，比如牛顿法？',
  '主要原因是海森矩阵的计算和存储代价太高。',
  '批量大小也会影响泛化，小批量带来的噪声有时反而是好事。',
  '接下来我们讨论正则化，先看权重衰减。',
  '权重衰减和 L2 正则在 SGD 下是等价的，但在 Adam 下并不等价。',
  '这就是 AdamW 提出的动机。',
  'Dropout 在训练时随机丢弃一部分神经元，推理时则全部保留。',
  '大家可以把它看成是对很多子网络做了隐式的集成。',
  '归一化层的作用是稳定每一层输入的分布。',
  '好，我们休息五分钟，回来以后讲反向传播的实现细节。',
];

function makeTranscript(durationMs, language = 'zh') {
  const id = ulid();
  const segments = [];
  let t = 0;
  let idx = 1;
  while (t < durationMs - 1500) {
    const text = SENTENCES[(idx * 7 + Math.floor(idx / 20)) % SENTENCES.length];
    const len = 2600 + ((idx * 37) % 9) * 350;
    const end = Math.min(durationMs, t + len);
    segments.push({
      segment_id: `seg_${id}_${String(idx).padStart(5, '0')}`,
      idx,
      start_ms: t,
      end_ms: end,
      text,
    });
    t = end + 120;
    idx++;
  }
  return { transcript_id: `tr_${id}`, language, engine: 'whispercpp', model: 'whisper-large-v3-turbo-q5_0', segments };
}

/** Builds a canonical note whose wk://seg/ links point into `tr`. */
function makeNote(video, tr, version = 1) {
  const long = video.duration_ms >= 3_600_000;
  const segs = tr.segments;
  const at = (fraction) => segs[Math.min(segs.length - 1, Math.floor(fraction * segs.length))];
  const cite = (fraction) => {
    const s = at(fraction);
    return `[${fmt(s.start_ms, long)}](wk://seg/${s.segment_id})`;
  };
  const noteId = `note_${ulid()}`;
  const chapters = [
    ['从损失函数到梯度下降', 0.0, 0.2, [
      ['损失函数的角色', '它把“预测得好不好”变成一个可以求导的数，优化的目标就是让它变小。', [0.02, 0.05]],
      ['梯度的几何直觉', '梯度指向函数上升最快的方向，沿反方向走一小步，损失就会下降。', [0.08]],
      ['学习率的两难', '太大会震荡甚至发散，太小则收敛缓慢、训练成本升高。', [0.12, 0.15]],
    ]],
    ['动量与自适应方法', 0.2, 0.45, [
      ['动量是惯性', '累积历史梯度，让更新方向更稳定，损失曲线明显更平滑。', [0.22, 0.26]],
      ['Adam 的组成', '同时维护一阶矩和二阶矩估计，相当于动量加上逐参数的学习率。', [0.31]],
      ['偏差修正', '两个矩估计都从零开始，训练初期必须修正，否则步长会被低估。', [0.36, 0.4]],
    ]],
    ['为什么不用二阶方法', 0.45, 0.6, [
      ['海森矩阵的代价', '参数量为 n 时需要 n² 的存储，对现代网络不可行。', [0.47, 0.5]],
      ['批量大小与泛化', '小批量带来的梯度噪声有助于跳出尖锐的极小值。', [0.55]],
    ]],
    ['正则化', 0.6, 1.0, [
      ['权重衰减 ≠ L2（在 Adam 下）', '自适应学习率会缩放 L2 项，AdamW 把衰减从梯度中解耦出来。', [0.63, 0.67]],
      ['Dropout', '训练时随机丢弃神经元，推理时全部保留，可以看成隐式集成。', [0.74, 0.78]],
      ['归一化层', '稳定各层输入的分布，使更大的学习率成为可能。', [0.86]],
    ]],
  ];
  const lines = [
    '---',
    'watchink:',
    `  video_id: ${video.video_id}`,
    `  note_id: ${noteId}`,
    `  transcript_id: ${tr.transcript_id}`,
    `  source: ${video.source.uri ?? ''}`,
    `  duration: "${fmt(video.duration_ms, long)}"`,
    `  language: ${tr.language}`,
    `  generated_at: ${now()}`,
    '  model: openai-compatible/mock-model',
    '  prompt_version: note-v0.1',
    '---',
    '',
    `# ${video.title}`,
    '',
    /^https?:/.test(video.source.uri ?? '')
      ? `> 来源：[${video.title}](${video.source.uri}) · 时长 ${fmt(video.duration_ms, long)}`
      : `> 来源：${video.title} · 时长 ${fmt(video.duration_ms, long)}`,
    '',
    '## 概览',
    '',
    `这一讲围绕“如何把损失降下来”展开：先建立梯度下降的直觉，再解释动量和 Adam 为什么更好用，随后讨论二阶方法为何很少被采用，最后介绍三种常见的正则化手段。 ${cite(0.02)} ${cite(0.31)} ${cite(0.63)}`,
    '',
  ];
  chapters.forEach(([title, from, to, points], i) => {
    const a = at(from).start_ms;
    const b = at(Math.min(0.999, to)).end_ms;
    lines.push(`## ${i + 1}. ${title} \`${fmt(a, long)}–${fmt(b, long)}\``, '');
    lines.push(`本节共 ${points.length} 个要点，按讲授顺序排列。`, '');
    for (const [head, text, refs] of points) {
      lines.push(`- **${head}**：${text} ${refs.map(cite).join(' ')}`);
    }
    lines.push('');
  });
  lines.push('## 关键结论', '');
  lines.push(`- 学习率是最需要优先调的超参数，配合预热和衰减使用。 ${cite(0.15)}`);
  lines.push(`- 使用 Adam 时，权重衰减应当用 AdamW 的解耦形式。 ${cite(0.67)}`);
  lines.push(`- 梯度噪声不一定是坏事，它和泛化能力有关。 ${cite(0.55)}`, '');
  lines.push('## 术语', '', '| 术语 | 说明 | 出处 |', '| --- | --- | --- |');
  lines.push(`| 动量（Momentum） | 对历史梯度做指数滑动平均，用作更新方向 | ${cite(0.22)} |`);
  lines.push(`| 偏差修正 | 修正从零初始化的矩估计在早期的偏小问题 | ${cite(0.36)} |`);
  lines.push(`| AdamW | 把权重衰减与梯度更新解耦的 Adam 变体 | ${cite(0.67)} |`);
  lines.push(`| Dropout | 训练时随机置零部分激活的正则化方法 | ${cite(0.74)} |`, '');
  // Things the renderer must neutralise: raw HTML and non-allow-listed links.
  lines.push('<script>window.__xss = true</script>', '');
  lines.push('延伸阅读：[Adam 论文](https://arxiv.org/abs/1412.6980) · [不安全链接](javascript:alert(1))', '');
  const content = lines.join('\n');

  const byId = new Map(segs.map((s) => [s.segment_id, s]));
  const citations = [];
  const re = /\(wk:\/\/seg\/([^)]+)\)/g;
  for (let m; (m = re.exec(content)); ) {
    const s = byId.get(m[1]);
    citations.push({
      ref_no: citations.length + 1,
      point_id: `p_${String(citations.length + 1).padStart(3, '0')}`,
      segment_id: s.segment_id,
      start_ms: s.start_ms,
      end_ms: s.end_ms,
      text: s.text,
    });
  }
  const pointCount = chapters.reduce((n, c) => n + c[3].length, 0);
  return {
    note_id: noteId,
    video_id: video.video_id,
    transcript_id: tr.transcript_id,
    version,
    title: video.title,
    content,
    citations,
    coverage: {
      chunks: Math.max(1, Math.round(video.duration_ms / 600_000)),
      chunks_empty: 0,
      points: pointCount + 3,
      points_dropped: 1,
      points_unplaced: 0,
      citations: citations.length,
      citations_dropped: 2,
      repair_calls: 1,
      split_retries: 0,
      transcript_segments: segs.length,
      transcript_chars: segs.reduce((n, s) => n + [...s.text].length, 0),
    },
    llm_provider: 'openai-compatible',
    llm_model: 'mock-model',
    prompt_version: 'note-v0.1',
    created_at: now(),
  };
}

// ---------------------------------------------------------------- state

const state = {
  videos: new Map(), // video_id -> { video, sourceKey, transcript, note }
  jobs: new Map(), // job_id -> { job, timeline?, timer? }
  uploads: new Map(),
  exports: new Map(),
  settings: {
    engine: { port: PORT, idle_exit: '10m', auto_resume: true },
    asr: { engine: 'whispercpp', model: '', language: 'auto', threads: 0, vad: true },
    llm: {
      provider: 'openai-compatible',
      base_url: FRESH ? '' : 'https://api.deepseek.com/v1',
      model: FRESH ? '' : 'deepseek-chat',
      api_key_env: '',
      structured_output: 'auto',
      temperature: 0.2,
      timeout: '120s',
      max_concurrency: 3,
    },
    note: { chunk_max_tokens: 3000, chunk_max_duration: '10m' },
    media: { keep_audio: false, playback_max_height: 720 },
    tools: { auto_prepare: true },
    network: { proxy: '' },
    download: { mirrors: {} },
    security: { import_roots: ['~'], max_upload_size: '8GiB' },
    web: { enabled: true },
  },
  apiKey: FRESH ? '' : 'sk-mock-0000000000004f2a',
  tools: [
    tool('whisper-cli', 'tool', { version: '1.8.2', embedded: true, status: 'ready', size: 4_812_000, license: 'MIT' }),
    tool('ffmpeg', 'tool', { version: '7.1.1', status: FRESH ? 'missing' : 'ready', download: 26_400_000, license: 'GPL（上游构建）' }),
    tool('ffprobe', 'tool', { version: '7.1.1', status: FRESH ? 'missing' : 'ready', download: 26_100_000, license: 'GPL（上游构建）' }),
    tool('yt-dlp', 'tool', { version: '2026.09.20', status: FRESH ? 'missing' : 'ready', download: 3_300_000, license: 'Unlicense' }),
    tool('deno', 'tool', { version: '2.5.1', status: 'missing', lazy: true, required: false, download: 41_900_000, license: 'MIT' }),
    tool('whisper-large-v3-turbo-q5_0', 'model', { status: FRESH ? 'missing' : 'ready', download: 574_000_000, license: 'MIT' }),
    tool('whisper-small-q5_1', 'model', { status: 'missing', required: false, download: 190_000_000, license: 'MIT' }),
    tool('silero-vad', 'model', { status: FRESH ? 'missing' : 'ready', download: 885_000, license: 'MIT' }),
  ],
};

function tool(name, kind, o) {
  const ready = o.status === 'ready';
  return {
    name,
    kind,
    version: o.version,
    status: o.status,
    embedded: o.embedded ?? false,
    lazy: o.lazy ?? false,
    required: o.required ?? true,
    size_bytes: ready ? (o.size ?? o.download ?? 0) : 0,
    download_bytes: o.download ?? 0,
    license: o.license,
  };
}

const llmConfigured = () => !!(state.settings.llm.base_url && state.settings.llm.model && state.apiKey);

function toolsStatus() {
  const missing = state.tools.filter((t) => t.required && t.status !== 'ready');
  return {
    items: state.tools,
    ready: missing.length === 0,
    missing_download_bytes: missing.reduce((n, t) => n + t.download_bytes, 0),
    default_asr_model: 'whisper-large-v3-turbo-q5_0',
  };
}

function settingsView() {
  return {
    ...state.settings,
    llm: {
      ...state.settings.llm,
      api_key_configured: !!state.apiKey,
      ...(state.apiKey ? { api_key_last4: state.apiKey.slice(-4), api_key_source: 'file' } : {}),
    },
    data_dir: '/Users/demo/Library/Application Support/Watchink',
    config_dir: '/Users/demo/Library/Application Support/Watchink/config',
  };
}

function activeJobs() {
  let n = 0;
  for (const { job } of state.jobs.values()) if (['queued', 'running', 'cancelling'].includes(job.status)) n++;
  return n;
}

function engineInfo() {
  return {
    version: '0.1.0-mock',
    build_id: 'mock-20261009',
    api: 1,
    pid: process.pid,
    data_dir: settingsView().data_dir,
    mode: 'foreground',
    base_url: BASE_URL,
    started_at: STARTED_AT,
    active_jobs: activeJobs(),
    tools_ready: toolsStatus().ready,
    llm_configured: llmConfigured(),
  };
}

// ---------------------------------------------------------------- events

const clients = new Set();
const backlog = []; // last 1000 events
let eventId = 0;

function emit(type, data) {
  const ev = { id: ++eventId, type, data };
  backlog.push(ev);
  if (backlog.length > 1000) backlog.shift();
  for (const res of clients) writeEvent(res, ev);
}
function writeEvent(res, ev) {
  res.write(`id: ${ev.id}\nevent: ${ev.type}\ndata: ${JSON.stringify(ev.data)}\n\n`);
}
setInterval(() => {
  for (const res of clients) res.write(': ping\n\n');
}, 15_000).unref();

// ---------------------------------------------------------------- videos & jobs

const STAGES = ['ensure_tools', 'acquire', 'probe', 'extract_audio', 'transcribe', 'playback', 'note_map', 'note_reduce', 'note_render'];
const WEIGHTS = { ensure_tools: 2, acquire: 15, probe: 1, extract_audio: 4, transcribe: 45, note_map: 25, note_reduce: 7, note_render: 1 };
// [stage, dependsOn, seconds]: about 20 s for the main chain at --speed 1.
const PLAN = [
  ['ensure_tools', null, 0.6],
  ['acquire', 'ensure_tools', 2.4],
  ['probe', 'acquire', 0.6],
  ['extract_audio', 'probe', 1.2],
  ['transcribe', 'extract_audio', 7.5],
  ['playback', 'probe', 5],
  ['note_map', 'transcribe', 5],
  ['note_reduce', 'note_map', 2],
  ['note_render', 'note_reduce', 0.7],
];

function videoDto(rec) {
  return rec.video;
}

function addVideo({ title, source, sourceKey, durationMs, status, createdAt, language = 'zh' }) {
  const video_id = `vid_${ulid()}`;
  const video = {
    video_id,
    title,
    source,
    duration_ms: durationMs ?? null,
    language: status === 'ready' ? language : null,
    status,
    latest_job_id: null,
    transcript_id: null,
    note_id: null,
    playback_ready: false,
    web_url: `${BASE_URL}/videos/${video_id}`,
    created_at: createdAt ?? now(),
    updated_at: createdAt ?? now(),
  };
  const rec = { video, sourceKey, transcript: null, note: null, plannedDurationMs: durationMs ?? 754_000 };
  state.videos.set(video_id, rec);
  return rec;
}

function touchVideo(rec, patch) {
  Object.assign(rec.video, patch, { updated_at: now() });
  emit('video.updated', rec.video);
}

function newJob(kind, videoId, stages, requestedBy) {
  const job = {
    job_id: `job_${ulid()}`,
    kind,
    video_id: videoId,
    status: 'queued',
    rev: 1,
    attempt: 1,
    stage: stages[0],
    progress: 0,
    stages: stages.map((stage) => ({ stage, status: 'pending', progress: 0, reused: false, attempt: 0 })),
    error: null,
    result: { transcript_id: null, note_id: null },
    requested_by: requestedBy || 'web',
    created_at: now(),
    updated_at: now(),
  };
  const rec = { job, timer: null };
  state.jobs.set(job.job_id, rec);
  return rec;
}

function bump(rec) {
  const job = rec.job;
  job.rev++;
  job.updated_at = now();
  if (job.kind === 'process_video') {
    let done = 0;
    let total = 0;
    for (const st of job.stages) {
      const w = WEIGHTS[st.stage];
      if (w == null) continue; // playback is shown separately
      total += w;
      done += w * st.progress;
    }
    job.progress = total ? done / total : 0;
  } else {
    job.progress = job.stages[0]?.progress ?? 0;
  }
  const open = job.stages.find((s) => s.status === 'pending' || s.status === 'running');
  job.stage = ['succeeded', 'failed', 'cancelled'].includes(job.status) ? '' : (open?.stage ?? '');
  emit('job.updated', job);
}

function stageOf(job, name) {
  return job.stages.find((s) => s.stage === name);
}

function finishStage(st, status = 'succeeded') {
  st.status = status;
  if (status === 'succeeded') st.progress = 1;
  st.finished_at = now();
}

function stopTimer(rec) {
  clearInterval(rec.timer);
  rec.timer = null;
}

function runProcessJob(rec) {
  const job = rec.job;
  const vrec = state.videos.get(job.video_id);
  const TICK = 400;
  job.status = 'running';
  job.started_at ??= now();
  delete job.finished_at;
  job.error = null;
  bump(rec);
  touchVideo(vrec, { status: 'processing' });

  rec.timer = setInterval(() => {
    for (const [name, dep, seconds] of PLAN) {
      const st = stageOf(job, name);
      if (st.status === 'succeeded') continue;
      if (dep && stageOf(job, dep).status !== 'succeeded') continue;
      if (st.status === 'pending') {
        st.status = 'running';
        st.attempt++;
        st.started_at = now();
        if (name === 'note_map' && !llmConfigured()) {
          // without a model the job fails right here instead of hanging.
          const e = err('MODEL_NOT_CONFIGURED', '没有配置在线模型', {
            stage: 'note_map',
            hint: '在设置中填写接口地址、模型名和 API Key 后重试该任务',
          });
          st.error = e;
          finishStage(st, 'failed');
          job.status = 'failed';
          job.error = e;
          job.finished_at = now();
          stopTimer(rec);
          bump(rec);
          touchVideo(vrec, { status: 'failed' });
          return;
        }
      }
      st.progress = Math.min(1, st.progress + (TICK / 1000 / seconds) * SPEED);
      if (st.progress < 1) continue;
      finishStage(st);
      if (name === 'acquire' && !vrec.video.title) touchVideo(vrec, { title: '梯度下降与优化器（导入示例）' });
      if (name === 'probe') touchVideo(vrec, { duration_ms: vrec.plannedDurationMs });
      if (name === 'transcribe') {
        vrec.transcript = makeTranscript(vrec.video.duration_ms);
        job.result.transcript_id = vrec.transcript.transcript_id;
        touchVideo(vrec, { transcript_id: vrec.transcript.transcript_id, language: 'zh' });
      }
      if (name === 'playback') touchVideo(vrec, { playback_ready: true });
      if (name === 'note_render') {
        vrec.note = makeNote(vrec.video, vrec.transcript, (vrec.note?.version ?? 0) + 1);
        job.result.note_id = vrec.note.note_id;
        touchVideo(vrec, { note_id: vrec.note.note_id });
      }
    }
    if (job.stages.every((s) => s.status === 'succeeded')) {
      job.status = 'succeeded';
      job.finished_at = now();
      stopTimer(rec);
      bump(rec);
      touchVideo(vrec, { status: 'ready' });
      return;
    }
    bump(rec);
  }, TICK);
}

function runPrepareJob(rec, names) {
  const job = rec.job;
  const items = state.tools.filter((t) => names.includes(t.name));
  const st = job.stages[0];
  const TICK = 400;
  const seconds = 6;
  job.status = 'running';
  job.started_at ??= now();
  job.error = null;
  st.status = 'running';
  st.attempt++;
  st.started_at = now();
  for (const t of items) Object.assign(t, { status: 'downloading', progress: 0 });
  bump(rec);
  emit('tools.updated', toolsStatus());
  rec.timer = setInterval(() => {
    st.progress = Math.min(1, st.progress + (TICK / 1000 / seconds) * SPEED);
    for (const t of items) t.progress = st.progress;
    if (st.progress >= 1) {
      for (const t of items) {
        t.status = 'ready';
        t.size_bytes = t.download_bytes;
        delete t.progress;
      }
      finishStage(st);
      job.status = 'succeeded';
      job.finished_at = now();
      stopTimer(rec);
    }
    bump(rec);
    emit('tools.updated', toolsStatus());
  }, TICK);
}

function cancelJob(rec) {
  const job = rec.job;
  if (!['queued', 'running'].includes(job.status)) fail('CONFLICT', `任务当前状态为 ${job.status}，不能取消`);
  stopTimer(rec);
  job.status = 'cancelling';
  bump(rec);
  setTimeout(() => {
    for (const st of job.stages) if (st.status === 'running') finishStage(st, 'cancelled');
    job.status = 'cancelled';
    job.finished_at = now();
    bump(rec);
    if (job.video_id) touchVideo(state.videos.get(job.video_id), { status: 'cancelled' });
    if (job.kind === 'prepare_tools') {
      for (const t of state.tools) {
        if (t.status === 'downloading') {
          t.status = 'missing';
          delete t.progress;
        }
      }
      emit('tools.updated', toolsStatus());
    }
  }, 500);
  return job;
}

function retryJob(rec) {
  const job = rec.job;
  if (!['failed', 'cancelled', 'interrupted'].includes(job.status)) {
    fail('CONFLICT', `任务当前状态为 ${job.status}，不能重试`);
  }
  job.attempt++;
  job.status = 'queued';
  job.error = null;
  for (const st of job.stages) {
    if (st.status === 'succeeded') st.reused = true; // still-valid artifacts are reused
    else Object.assign(st, { status: 'pending', progress: 0, error: undefined, finished_at: undefined });
  }
  bump(rec);
  setTimeout(() => {
    if (job.kind === 'process_video') runProcessJob(rec);
    else runPrepareJob(rec, rec.names ?? []);
  }, 500);
  return job;
}

function startImport(rec, requestedBy) {
  const jrec = newJob('process_video', rec.video.video_id, STAGES, requestedBy);
  rec.video.latest_job_id = jrec.job.job_id;
  emit('job.updated', jrec.job);
  emit('video.updated', rec.video);
  setTimeout(() => runProcessJob(jrec), 700);
  return jrec;
}

function seed() {
  const ready = addVideo({
    title: '深度学习优化方法：从 SGD 到 AdamW',
    source: { type: 'youtube', uri: 'https://www.youtube.com/watch?v=mock0000001', platform_id: 'mock0000001' },
    sourceKey: 'yt:mock0000001',
    durationMs: 3_753_000,
    status: 'ready',
    createdAt: ago(60 * 26),
  });
  ready.transcript = makeTranscript(ready.video.duration_ms);
  ready.note = makeNote(ready.video, ready.transcript);
  Object.assign(ready.video, {
    transcript_id: ready.transcript.transcript_id,
    note_id: ready.note.note_id,
    playback_ready: true,
  });
  const doneJob = newJob('process_video', ready.video.video_id, STAGES, 'cli');
  Object.assign(doneJob.job, { status: 'succeeded', progress: 1, stage: '', rev: 42, started_at: ago(60 * 26), finished_at: ago(60 * 25) });
  doneJob.job.created_at = ago(60 * 26);
  doneJob.job.result = { transcript_id: ready.transcript.transcript_id, note_id: ready.note.note_id };
  for (const st of doneJob.job.stages) Object.assign(st, { status: 'succeeded', progress: 1, attempt: 1 });
  stageOf(doneJob.job, 'ensure_tools').reused = true;
  ready.video.latest_job_id = doneJob.job.job_id;

  // A short local file whose playback file is not available: the no-media fallback.
  const local = addVideo({
    title: '线性代数复习：特征值与特征向量',
    source: { type: 'file', uri: '/Users/demo/Movies/linear-algebra-07.mkv' },
    sourceKey: 'file:la07',
    durationMs: 1_425_000,
    status: 'ready',
    createdAt: ago(60 * 50),
  });
  local.transcript = makeTranscript(local.video.duration_ms);
  local.note = makeNote(local.video, local.transcript);
  Object.assign(local.video, { transcript_id: local.transcript.transcript_id, note_id: local.note.note_id });

  const failed = addVideo({
    title: '操作系统：虚拟内存与页表',
    source: { type: 'bilibili', uri: 'https://www.bilibili.com/video/BV1mock00002?p=1', platform_id: 'BV1mock00002' },
    sourceKey: 'bili:BV1mock00002:1',
    durationMs: 2_912_000,
    status: 'failed',
    createdAt: ago(60 * 5),
  });
  failed.transcript = makeTranscript(failed.video.duration_ms);
  Object.assign(failed.video, { transcript_id: failed.transcript.transcript_id, playback_ready: true, language: 'zh' });
  const failedJob = newJob('process_video', failed.video.video_id, STAGES, 'web');
  const authError = err('MODEL_AUTH_FAILED', '在线模型鉴权失败（HTTP 401）', {
    stage: 'note_map',
    hint: '在设置中更新 API Key 后重试该任务',
    details: { http_status: 401 },
  });
  Object.assign(failedJob.job, { status: 'failed', stage: '', rev: 31, error: authError, started_at: ago(60 * 5), finished_at: ago(60 * 4.6) });
  failedJob.job.created_at = ago(60 * 5);
  failedJob.job.result.transcript_id = failed.transcript.transcript_id;
  for (const st of failedJob.job.stages) {
    if (st.stage === 'note_map') Object.assign(st, { status: 'failed', progress: 0.14, attempt: 1, error: authError });
    else if (!st.stage.startsWith('note_')) Object.assign(st, { status: 'succeeded', progress: 1, attempt: 1 });
  }
  bumpQuiet(failedJob);
  failed.video.latest_job_id = failedJob.job.job_id;

  // Enough older rows to exercise cursor pagination (page size 30).
  const topics = ['概率论', '数据结构', '计算机网络', '编译原理', '数据库系统', '机器学习导论', '信号与系统', '离散数学'];
  for (let i = 0; i < 34; i++) {
    const rec = addVideo({
      title: `${topics[i % topics.length]} 第 ${i + 1} 讲`,
      source: { type: 'bilibili', uri: `https://www.bilibili.com/video/BV1mockA${String(i).padStart(4, '0')}`, platform_id: `BV1mockA${i}` },
      sourceKey: `bili:BV1mockA${i}:1`,
      durationMs: 1_800_000 + i * 61_000,
      status: 'ready',
      createdAt: ago(60 * 72 + i * 600),
    });
    rec.lazy = true; // transcript / note are generated on first read
    rec.video.note_id = `note_${ulid()}`;
    rec.video.transcript_id = `tr_${ulid()}`;
  }

  // A job that is running when the UI opens (about 20 s).
  const running = addVideo({
    title: 'CS231n Lecture 3：Loss Functions and Optimization',
    source: { type: 'youtube', uri: 'https://www.youtube.com/watch?v=mock0000003', platform_id: 'mock0000003' },
    sourceKey: 'yt:mock0000003',
    durationMs: null,
    status: 'processing',
  });
  running.plannedDurationMs = 4_471_000;
  startImport(running, 'web');
}

function bumpQuiet(rec) {
  const job = rec.job;
  let done = 0;
  let total = 0;
  for (const st of job.stages) {
    const w = WEIGHTS[st.stage];
    if (w == null) continue;
    total += w;
    done += w * st.progress;
  }
  job.progress = total ? done / total : 0;
}

function materialise(rec) {
  if (!rec.lazy) return;
  rec.lazy = false;
  rec.transcript = makeTranscript(rec.video.duration_ms);
  rec.transcript.transcript_id = rec.video.transcript_id;
  rec.note = makeNote(rec.video, rec.transcript);
  rec.note.note_id = rec.video.note_id;
}

// ---------------------------------------------------------------- media (generated WAV)

const WAV_RATE = 8000; // 8-bit mono PCM: 8 kB per second of "video"
function wavHeader(dataBytes) {
  const b = Buffer.alloc(44);
  b.write('RIFF', 0);
  b.writeUInt32LE(36 + dataBytes, 4);
  b.write('WAVEfmt ', 8);
  b.writeUInt32LE(16, 16);
  b.writeUInt16LE(1, 20);
  b.writeUInt16LE(1, 22);
  b.writeUInt32LE(WAV_RATE, 24);
  b.writeUInt32LE(WAV_RATE, 28);
  b.writeUInt16LE(1, 32);
  b.writeUInt16LE(8, 34);
  b.write('data', 36);
  b.writeUInt32LE(dataBytes, 40);
  return b;
}

function serveMedia(req, res, rec) {
  const dataBytes = Math.floor((rec.video.duration_ms / 1000) * WAV_RATE);
  const total = 44 + dataBytes;
  let start = 0;
  let end = total - 1;
  const m = /^bytes=(\d*)-(\d*)$/.exec(req.headers.range ?? '');
  if (m) {
    if (m[1] === '' && m[2] !== '') start = Math.max(0, total - Number(m[2]));
    else {
      start = Number(m[1]);
      if (m[2] !== '') end = Math.min(end, Number(m[2]));
    }
    if (start > end || start >= total) {
      res.writeHead(416, { 'Content-Range': `bytes */${total}` }).end();
      return;
    }
  }
  const headers = {
    'Content-Type': 'audio/wav',
    'Accept-Ranges': 'bytes',
    'Content-Length': end - start + 1,
    'Cache-Control': 'no-store',
  };
  if (m) headers['Content-Range'] = `bytes ${start}-${end}/${total}`;
  res.writeHead(m ? 206 : 200, headers);
  if (req.method === 'HEAD') return res.end();
  const header = wavHeader(dataBytes);
  let pos = start;
  const write = () => {
    while (pos <= end) {
      const n = Math.min(256 * 1024, end - pos + 1);
      const chunk = Buffer.alloc(n, 128); // 128 = silence for unsigned 8-bit PCM
      if (pos < 44) header.copy(chunk, 0, pos, Math.min(44, pos + n));
      pos += n;
      if (!res.write(chunk)) return res.once('drain', write);
    }
    res.end();
  };
  write();
}

// ---------------------------------------------------------------- HTTP plumbing

function send(res, status, body) {
  const text = body === undefined ? '' : JSON.stringify(body);
  res.writeHead(status, { 'Content-Type': 'application/json; charset=utf-8', 'Cache-Control': 'no-store' });
  res.end(text);
}

function readBody(req) {
  return new Promise((resolveBody, reject) => {
    const chunks = [];
    req.on('data', (c) => chunks.push(c));
    req.on('end', () => resolveBody(Buffer.concat(chunks)));
    req.on('error', reject);
  });
}
async function readJson(req) {
  const buf = await readBody(req);
  if (!buf.length) return {};
  try {
    return JSON.parse(buf.toString('utf8'));
  } catch {
    fail('INVALID_ARGUMENT', '请求体不是合法的 JSON');
  }
}

function page(list, url, keyOf) {
  const limit = Math.min(100, Math.max(1, Number(url.searchParams.get('limit')) || 20));
  const cursor = url.searchParams.get('cursor');
  let from = 0;
  if (cursor) {
    from = list.findIndex((x) => keyOf(x) === cursor) + 1;
    if (from === 0) fail('INVALID_ARGUMENT', 'cursor 无效');
  }
  const items = list.slice(from, from + limit);
  const more = from + limit < list.length;
  return { items, next_cursor: more ? keyOf(items[items.length - 1]) : null };
}

function getVideo(id) {
  const rec = state.videos.get(id);
  if (!rec) fail('NOT_FOUND', `视频 ${id} 不存在`);
  return rec;
}
function getJob(id) {
  const rec = state.jobs.get(id);
  if (!rec) fail('NOT_FOUND', `任务 ${id} 不存在`);
  return rec;
}

function sourceKeyOf(source) {
  if (source?.type === 'url') {
    let u;
    try {
      u = new URL(source.url);
    } catch {
      fail('INVALID_ARGUMENT', '链接格式无效');
    }
    const host = u.hostname.replace(/^www\./, '');
    if (host === 'youtube.com' || host === 'youtu.be') {
      const id = u.searchParams.get('v') || u.pathname.slice(1);
      return { key: `yt:${id}`, source: { type: 'youtube', uri: source.url, platform_id: id }, title: '' };
    }
    if (host === 'bilibili.com') {
      const id = u.pathname.split('/').filter(Boolean)[1] ?? '';
      return { key: `bili:${id}:${u.searchParams.get('p') ?? 1}`, source: { type: 'bilibili', uri: source.url, platform_id: id }, title: '' };
    }
    fail('UNSUPPORTED_SOURCE', '只支持 YouTube 和 Bilibili 的单个视频链接', { hint: '请粘贴单个视频的播放页地址' });
  }
  if (source?.type === 'upload') {
    const up = state.uploads.get(source.upload_id);
    if (!up) fail('NOT_FOUND', `上传 ${source.upload_id} 不存在或已过期`);
    return {
      key: `upload:${up.filename}:${up.size_bytes}`,
      source: { type: 'upload', uri: up.filename },
      title: up.filename.replace(/\.[^.]+$/, ''),
    };
  }
  if (source?.type === 'file') {
    if (!source.path?.startsWith('/')) fail('INVALID_ARGUMENT', '本地路径必须是绝对路径');
    const name = source.path.split('/').pop();
    return { key: `file:${source.path}`, source: { type: 'file', uri: source.path }, title: name.replace(/\.[^.]+$/, '') };
  }
  fail('INVALID_ARGUMENT', 'source.type 必须是 file / url / upload');
}

function doImport(body) {
  const { key, source, title } = sourceKeyOf(body.source);
  const warnings = llmConfigured() ? [] : ['MODEL_NOT_CONFIGURED'];
  const existing = [...state.videos.values()].find((r) => r.sourceKey === key);
  if (existing && !body.reprocess_from) {
    const jrec = existing.video.latest_job_id ? state.jobs.get(existing.video.latest_job_id) : null;
    const status = jrec?.job.status;
    const base = { video_id: existing.video.video_id, job_id: jrec?.job.job_id ?? '', note_id: existing.video.note_id, warnings };
    // dedup table.
    if (['queued', 'running', 'cancelling'].includes(status)) return { ...base, status, deduplicated: true };
    if (existing.video.status === 'ready') return { ...base, status: 'already_processed', deduplicated: true };
    if (status) return { ...base, status, deduplicated: true };
  }
  const rec = existing ?? addVideo({ title, source, sourceKey: key, durationMs: null, status: 'processing' });
  const jrec = startImport(rec, body.requested_by);
  return { video_id: rec.video.video_id, job_id: jrec.job.job_id, status: 'queued', deduplicated: false, note_id: null, warnings };
}

function applyPatch(target, patch, path = '') {
  for (const [k, v] of Object.entries(patch ?? {})) {
    if (!(k in target) || ['data_dir', 'config_dir'].includes(k)) fail('INVALID_ARGUMENT', `未知的配置项 ${path}${k}`);
    if (v && typeof v === 'object' && !Array.isArray(v) && typeof target[k] === 'object' && k !== 'mirrors') {
      applyPatch(target[k], v, `${path}${k}.`);
    } else {
      target[k] = v;
    }
  }
}

function renderTranscript(segs, format) {
  const ts = (ms, sep) => {
    const p = (n, w = 2) => String(n).padStart(w, '0');
    return `${p(Math.floor(ms / 3_600_000))}:${p(Math.floor(ms / 60_000) % 60)}:${p(Math.floor(ms / 1000) % 60)}${sep}${p(ms % 1000, 3)}`;
  };
  if (format === 'txt') return segs.map((s) => s.text).join('\n') + '\n';
  if (format === 'srt') return segs.map((s, i) => `${i + 1}\n${ts(s.start_ms, ',')} --> ${ts(s.end_ms, ',')}\n${s.text}\n`).join('\n');
  return 'WEBVTT\n\n' + segs.map((s) => `${ts(s.start_ms, '.')} --> ${ts(s.end_ms, '.')}\n${s.text}\n`).join('\n');
}

const MIME = {
  '.html': 'text/html; charset=utf-8',
  '.js': 'text/javascript; charset=utf-8',
  '.css': 'text/css; charset=utf-8',
  '.svg': 'image/svg+xml',
  '.json': 'application/json',
  '.png': 'image/png',
  '.ico': 'image/x-icon',
  '.woff2': 'font/woff2',
};

function serveStatic(req, res, pathname) {
  if (!DIST) return send(res, 404, { error: err('NOT_FOUND', 'not found') });
  const rel = normalize(decodeURIComponent(pathname)).replace(/^(\.\.[/\\])+/, '');
  let file = join(DIST, rel);
  const isAsset = pathname.startsWith('/assets/');
  if (!file.startsWith(DIST) || !existsSync(file) || statSync(file).isDirectory()) {
    if (isAsset) return res.writeHead(404).end('not found');
    file = join(DIST, 'index.html'); // history fallback, like the engine
  }
  res.writeHead(200, {
    'Content-Type': MIME[extname(file)] ?? 'application/octet-stream',
    'Cache-Control': isAsset ? 'public, max-age=31536000, immutable' : 'no-cache',
  });
  createReadStream(file).pipe(res);
}

// ---------------------------------------------------------------- routes

async function route(req, res) {
  const url = new URL(req.url, BASE_URL);
  const path = url.pathname;
  const method = req.method;

  const media = /^\/media\/([^/]+)\/playback$/.exec(path);
  if (media) {
    const rec = state.videos.get(media[1]);
    if (!rec || url.searchParams.get('mt') !== `mock-${media[1]}`) return res.writeHead(403).end('forbidden');
    return serveMedia(req, res, rec);
  }
  if (!path.startsWith('/api/v1/')) return serveStatic(req, res, path);

  const p = path.slice('/api/v1'.length);
  if (p === '/health') return send(res, 200, { status: 'ok' });
  if (REQUIRE_AUTH) fail('UNAUTHORIZED', '缺少或无效的会话');
  // Cookie sessions must carry the custom header on state-changing requests.
  if (method !== 'GET' && method !== 'HEAD' && req.headers['x-wk-request'] !== '1' && !req.headers.authorization) {
    fail('UNAUTHORIZED', '状态变更请求缺少 X-WK-Request 头');
  }

  let m;
  if (p === '/engine' && method === 'GET') return send(res, 200, engineInfo());

  if (p === '/events' && method === 'GET') {
    res.writeHead(200, {
      'Content-Type': 'text/event-stream',
      'Cache-Control': 'no-store',
      Connection: 'keep-alive',
      'X-Accel-Buffering': 'no',
    });
    res.write(': connected\n\n');
    const last = req.headers['last-event-id'];
    if (last != null && last !== '') {
      const after = Number(last);
      const oldest = backlog[0]?.id ?? eventId + 1;
      // Outside the replay buffer (or from another engine run): full refresh.
      if (!Number.isFinite(after) || after > eventId || after < oldest - 1) res.write('event: reset\ndata: {}\n\n');
      else for (const ev of backlog) if (ev.id > after) writeEvent(res, ev);
    }
    clients.add(res);
    req.on('close', () => clients.delete(res));
    return;
  }

  if (p === '/uploads' && method === 'POST') {
    let size = 0;
    for await (const chunk of req) size += chunk.length;
    let filename = 'upload.mp4';
    try {
      filename = decodeURIComponent(String(req.headers['x-filename'] ?? filename));
    } catch {
      fail('INVALID_ARGUMENT', 'X-Filename 头无效');
    }
    const up = { upload_id: `upl_${ulid()}`, filename, size_bytes: size, expires_at: new Date(Date.now() + 3_600_000).toISOString() };
    state.uploads.set(up.upload_id, up);
    return send(res, 201, up);
  }

  if (p === '/imports' && method === 'POST') return send(res, 202, doImport(await readJson(req)));

  if (p === '/videos' && method === 'GET') {
    const q = (url.searchParams.get('title_contains') ?? '').toLowerCase();
    const list = [...state.videos.values()]
      .map(videoDto)
      .filter((v) => !q || v.title.toLowerCase().includes(q))
      .sort((a, b) => b.created_at.localeCompare(a.created_at) || b.video_id.localeCompare(a.video_id));
    const { items, next_cursor } = page(list, url, (v) => v.video_id);
    return send(res, 200, { videos: items, next_cursor });
  }
  if ((m = /^\/videos\/([^/]+)$/.exec(p)) && method === 'GET') return send(res, 200, getVideo(m[1]).video);

  if ((m = /^\/videos\/([^/]+)\/note$/.exec(p)) && method === 'GET') {
    const rec = getVideo(m[1]);
    materialise(rec);
    if (!rec.note) fail('NOT_FOUND', '这个视频还没有笔记');
    const noteId = url.searchParams.get('note_id');
    if (noteId && noteId !== rec.note.note_id) fail('NOT_FOUND', `笔记 ${noteId} 不存在`);
    const cps = [...rec.note.content]; // offset / max_chars count code points
    const offset = Math.max(0, Number(url.searchParams.get('offset')) || 0);
    const max = Math.max(1, Number(url.searchParams.get('max_chars')) || PAGE_CHARS);
    const end = Math.min(cps.length, offset + max);
    return send(res, 200, {
      ...rec.note,
      format: 'markdown',
      content: cps.slice(offset, end).join(''),
      total_chars: cps.length,
      next_offset: end < cps.length ? end : null,
    });
  }

  if ((m = /^\/videos\/([^/]+)\/transcript$/.exec(p)) && method === 'GET') {
    const rec = getVideo(m[1]);
    materialise(rec);
    const tr = rec.transcript;
    if (!tr) fail('NOT_FOUND', '这个视频还没有字幕');
    const format = url.searchParams.get('format') || 'json';
    let segs = tr.segments;
    const ids = url.searchParams.get('segment_ids');
    if (ids) {
      const want = new Set(ids.split(','));
      if (want.size > 50) fail('INVALID_ARGUMENT', 'segment_ids 最多 50 个');
      segs = segs.filter((s) => want.has(s.segment_id));
    } else {
      const from = url.searchParams.get('start_ms');
      const to = url.searchParams.get('end_ms');
      if (from != null) segs = segs.filter((s) => s.start_ms >= Number(from));
      if (to != null) segs = segs.filter((s) => s.start_ms < Number(to));
    }
    const raw = Number(url.searchParams.get('max_chars')) || 0;
    const max = raw < 0 ? Infinity : raw || 20000;
    const out = [];
    let chars = 0;
    let next = null;
    for (const s of segs) {
      const n = [...s.text].length;
      if (out.length > 0 && chars + n > max) {
        next = s.start_ms;
        break;
      }
      out.push(s);
      chars += n;
    }
    return send(res, 200, {
      transcript_id: tr.transcript_id,
      video_id: rec.video.video_id,
      language: tr.language,
      engine: tr.engine,
      model: tr.model,
      segment_count: tr.segments.length,
      format,
      segments: out,
      ...(format === 'json' ? {} : { content: renderTranscript(out, format) }),
      truncated: next != null,
      next_start_ms: next,
    });
  }

  if ((m = /^\/videos\/([^/]+)\/playback$/.exec(p)) && method === 'GET') {
    const rec = getVideo(m[1]);
    if (!rec.video.playback_ready) fail('NOT_FOUND', '播放文件尚未就绪');
    return send(res, 200, {
      url: `/media/${rec.video.video_id}/playback?mt=mock-${rec.video.video_id}`,
      mime: 'audio/wav',
      expires_at: new Date(Date.now() + 6 * 3_600_000).toISOString(),
    });
  }

  if ((m = /^\/videos\/([^/]+)\/exports$/.exec(p)) && method === 'POST') {
    const rec = getVideo(m[1]);
    materialise(rec);
    const { format } = await readJson(req);
    const ext = { md: 'md', 'md-raw': 'md', json: 'json', srt: 'srt', vtt: 'vtt', txt: 'txt' }[format];
    if (!ext) fail('INVALID_ARGUMENT', `不支持的导出格式 ${format}`);
    let content;
    if (['srt', 'vtt', 'txt'].includes(format)) {
      if (!rec.transcript) fail('NOT_FOUND', '这个视频还没有字幕');
      content = renderTranscript(rec.transcript.segments, format);
    } else {
      if (!rec.note) fail('NOT_FOUND', '这个视频还没有笔记');
      if (format === 'json') content = JSON.stringify({ ...rec.note, content: undefined }, null, 2);
      else if (format === 'md-raw') content = rec.note.content;
      else content = rec.note.content.replace(/\[([^\]]+)\]\(wk:\/\/seg\/[^)]+\)/g, '[$1]');
    }
    const export_id = `exp_${ulid()}`;
    const filename = `${rec.video.title.replace(/[\\/:*?"<>|]/g, '_')}-${rec.video.video_id.slice(-6)}.${ext}`;
    const exp = {
      export_id,
      video_id: rec.video.video_id,
      format,
      path: `${settingsView().data_dir}/exports/${filename}`,
      filename,
      bytes: Buffer.byteLength(content),
      sha256: '0'.repeat(64),
      download_url: `/api/v1/exports/${export_id}/download`,
      created_at: now(),
    };
    state.exports.set(export_id, { exp, content });
    return send(res, 201, exp);
  }
  if ((m = /^\/exports\/([^/]+)\/download$/.exec(p)) && method === 'GET') {
    const e = state.exports.get(m[1]);
    if (!e) fail('NOT_FOUND', '导出不存在');
    res.writeHead(200, {
      'Content-Type': 'application/octet-stream',
      'Content-Disposition': `attachment; filename*=UTF-8''${encodeURIComponent(e.exp.filename)}`,
      'Content-Length': Buffer.byteLength(e.content),
    });
    return res.end(e.content);
  }

  if (p === '/jobs' && method === 'GET') {
    const status = url.searchParams.get('status');
    const vid = url.searchParams.get('video_id');
    const list = [...state.jobs.values()]
      .map((r) => r.job)
      .filter((j) => (!status || j.status === status) && (!vid || j.video_id === vid))
      .sort((a, b) => b.created_at.localeCompare(a.created_at) || b.job_id.localeCompare(a.job_id));
    const { items, next_cursor } = page(list, url, (j) => j.job_id);
    return send(res, 200, { jobs: items, next_cursor });
  }
  if ((m = /^\/jobs\/([^/]+)$/.exec(p)) && method === 'GET') return send(res, 200, getJob(m[1]).job);
  if ((m = /^\/jobs\/([^/]+)\/cancel$/.exec(p)) && method === 'POST') return send(res, 200, cancelJob(getJob(m[1])));
  if ((m = /^\/jobs\/([^/]+)\/retry$/.exec(p)) && method === 'POST') return send(res, 200, retryJob(getJob(m[1])));

  if (p === '/settings' && method === 'GET') return send(res, 200, settingsView());
  if (p === '/settings' && method === 'PATCH') {
    applyPatch(state.settings, await readJson(req));
    return send(res, 200, settingsView());
  }
  if (p === '/settings/secrets/llm_api_key' && method === 'PUT') {
    const { value } = await readJson(req);
    if (typeof value !== 'string') fail('INVALID_ARGUMENT', 'value 必须是字符串');
    state.apiKey = value;
    res.writeHead(204).end();
    return;
  }
  if (p === '/settings/llm/models' && method === 'POST') {
    const body = (await readJson(req).catch(() => ({}))) ?? {};
    const base = body.base_url || state.settings?.llm?.base_url || '';
    if (/no-list/.test(base)) return send(res, 502, { error: err('MODEL_UNAVAILABLE', '该接口没有提供模型列表（HTTP 404）', { hint: '可以手动输入模型名' }) });
    return send(res, 200, { base_url: base, models: ['mock-large', 'mock-mini', 'mock-reasoner'] });
  }
  if (p === '/settings/llm/test' && method === 'POST') {
    await new Promise((r) => setTimeout(r, 600));
    if (!llmConfigured()) {
      return send(res, 200, {
        ok: false,
        latency_ms: 0,
        error: err('MODEL_NOT_CONFIGURED', '没有配置在线模型', { hint: '填写接口地址、模型名和 API Key' }),
      });
    }
    if (state.apiKey.includes('bad')) {
      return send(res, 200, {
        ok: false,
        latency_ms: 212,
        error: err('MODEL_AUTH_FAILED', '在线模型鉴权失败（HTTP 401）', { hint: '检查 API Key 是否正确', details: { http_status: 401 } }),
      });
    }
    return send(res, 200, { ok: true, structured_output: 'json_schema', model: state.settings.llm.model, latency_ms: 483 });
  }

  if (p === '/tools' && method === 'GET') return send(res, 200, toolsStatus());
  if (p === '/tools/prepare' && method === 'POST') {
    const body = await readJson(req);
    const active = [...state.jobs.values()].find((r) => r.job.kind === 'prepare_tools' && ['queued', 'running'].includes(r.job.status));
    if (active) return send(res, 202, active.job);
    let names = Array.isArray(body.items) ? body.items : [];
    if (names.length === 0) names = state.tools.filter((t) => t.required && t.status !== 'ready').map((t) => t.name);
    for (const n of names) if (!state.tools.some((t) => t.name === n)) fail('INVALID_ARGUMENT', `未知的工具或模型 ${n}`);
    const jrec = newJob('prepare_tools', null, ['prepare'], 'web');
    jrec.names = names;
    emit('job.updated', jrec.job);
    setTimeout(() => runPrepareJob(jrec, names), 300);
    return send(res, 202, jrec.job);
  }
  if (p === '/tools/update' && method === 'POST') {
    const { name } = await readJson(req);
    const t = state.tools.find((x) => x.name === name);
    if (!t) fail('NOT_FOUND', `工具 ${name} 不存在`);
    if (name !== 'yt-dlp') fail('INVALID_ARGUMENT', 'v0 只支持更新 yt-dlp');
    await new Promise((r) => setTimeout(r, 800));
    Object.assign(t, { version: '2026.10.08', status: 'ready', size_bytes: t.download_bytes });
    emit('tools.updated', toolsStatus());
    return send(res, 200, toolsStatus());
  }
  if (p === '/tools/import' && method === 'POST') {
    const { name } = await readJson(req);
    const t = state.tools.find((x) => x.name === name);
    if (!t) fail('NOT_FOUND', `工具 ${name} 不存在`);
    Object.assign(t, { status: 'ready', size_bytes: t.download_bytes });
    emit('tools.updated', toolsStatus());
    return send(res, 200, toolsStatus());
  }

  fail('NOT_FOUND', `没有这个接口：${method} ${path}`);
}

const server = http.createServer((req, res) => {
  route(req, res).catch((e) => {
    if (res.headersSent) return res.end();
    if (e instanceof HttpError) return send(res, e.status, { error: e.error });
    console.error(e);
    send(res, 500, { error: err('INTERNAL', String(e?.message ?? e)) });
  });
});

seed();
server.listen(PORT, '127.0.0.1', () => {
  console.log(`[mock] Watchink mock engine on ${BASE_URL}${DIST ? ` (serving ${DIST})` : ''}`);
});
for (const sig of ['SIGINT', 'SIGTERM']) process.on(sig, () => process.exit(0));
