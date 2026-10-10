import { useQueryClient } from '@tanstack/react-query';
import { useId, useState, type ReactNode } from 'react';
import { Link } from 'react-router';
import { api } from '../api';
import { streamStatusStore } from '../api/events';
import { useAgentSetup, useEngine, useSettings, useTools } from '../api/hooks';
import { keys } from '../api/keys';
import type { Settings } from '../api/types';
import { Page } from '../components/AppShell';
import { LlmForm } from '../components/LlmForm';
import { ToolsPanel } from '../components/ToolsPanel';
import { Button, CodeBlock, ErrorNotice, ErrorState, Field, Loading, buttonClass, cx, inputClass } from '../components/ui';
import { zh } from '../i18n/zh';
import { useStore } from '../lib/store';
import { formatInstant } from '../lib/time';
import { pushToast } from '../lib/toast';
import { platform } from '../platform';
import s from './pages.module.css';

const t = zh.settings;

function Section({ id, title, children }: { id: string; title: string; children: ReactNode }) {
  return (
    <section className={cx(s.card, s.section)} aria-labelledby={`sec-${id}`} data-section={id}>
      <h2 id={`sec-${id}`} className={s.sectionTitle}>
        {title}
      </h2>
      {children}
    </section>
  );
}

function AsrForm({ settings }: { settings: Settings }) {
  const qc = useQueryClient();
  const uid = useId();
  const tools = useTools().data;
  const [model, setModel] = useState(settings.asr.model);
  const [language, setLanguage] = useState(settings.asr.language || 'auto');
  const [vad, setVad] = useState(settings.asr.vad);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<unknown>(null);

  // ASR models are the manifest models named whisper-*; silero-vad is the VAD model.
  const models = (tools?.items ?? []).filter((i) => i.kind === 'model' && i.name.startsWith('whisper-'));
  const dirty = model !== settings.asr.model || language !== settings.asr.language || vad !== settings.asr.vad;

  async function save() {
    setSaving(true);
    setError(null);
    try {
      const next = await api.patchSettings({ asr: { model, language, vad } });
      if (next && typeof next === 'object') qc.setQueryData(keys.settings, next);
      // The required set depends on the chosen model and VAD.
      void qc.invalidateQueries({ queryKey: keys.tools });
      void qc.invalidateQueries({ queryKey: keys.engine });
      pushToast({ kind: 'success', message: zh.app.saved });
    } catch (e) {
      setError(e);
    } finally {
      setSaving(false);
    }
  }

  return (
    <div className={s.stack}>
      <p className={s.text}>{t.asr.hint}</p>
      <div className={s.grid2}>
        <Field label={t.asr.model} htmlFor={`${uid}-model`}>
          <select id={`${uid}-model`} className={inputClass} value={model} onChange={(e) => setModel(e.target.value)}>
            <option value="">
              {tools?.default_asr_model ? t.asr.modelDefault(tools.default_asr_model) : t.asr.modelDefaultPlain}
            </option>
            {models.map((m) => (
              <option key={m.name} value={m.name}>
                {m.name}
                {m.status !== 'ready' ? `（${t.asr.modelMissing}）` : ''}
              </option>
            ))}
            {model && !models.some((m) => m.name === model) && <option value={model}>{model}</option>}
          </select>
        </Field>
        <Field label={t.asr.language} htmlFor={`${uid}-lang`}>
          <select id={`${uid}-lang`} className={inputClass} value={language} onChange={(e) => setLanguage(e.target.value)}>
            {Object.entries(zh.language).map(([value, label]) => (
              <option key={value} value={value}>
                {label}
              </option>
            ))}
          </select>
        </Field>
      </div>
      <label className={s.check}>
        <input type="checkbox" checked={vad} onChange={(e) => setVad(e.target.checked)} />
        <span>
          {t.asr.vad}
          <span className={s.muted} style={{ display: 'block' }}>
            {t.asr.vadHint}
          </span>
        </span>
      </label>
      {error != null && <ErrorNotice error={error} />}
      <div className={s.row}>
        <Button variant="primary" disabled={!dirty || saving} onClick={() => void save()}>
          {zh.app.save}
        </Button>
      </div>
    </div>
  );
}

function AgentSection() {
  const setup = useAgentSetup();
  if (setup.isPending) return <Loading />;
  // The desktop shell knows the absolute path; in the browser there is no
  // native shell to ask, so fall back to the name on PATH.
  const [command = 'watchink', ...args] = setup.data?.mcp_command?.length ? setup.data.mcp_command : ['watchink', 'mcp'];
  return (
    <div className={s.stack}>
      <p className={s.text}>{t.agent.intro}</p>
      <div className={s.snippet}>
        <h3>{t.agent.promptTitle}</h3>
        <CodeBlock code={t.agent.prompt(command, args)} />
      </div>
      {!setup.data && <p className={s.muted}>{t.agent.pathNote}</p>}
      <div className={s.snippet}>
        <h3>{t.agent.examplesTitle}</h3>
        <ul className={s.examples}>
          {t.agent.examples.map((ex) => (
            <li key={ex}>{ex}</li>
          ))}
        </ul>
      </div>
    </div>
  );
}

function AboutSection() {
  const engine = useEngine();
  const stream = useStore(streamStatusStore);
  const e = engine.data;
  const a = t.about;
  return (
    <div className={s.stack}>
      {engine.isError && <ErrorNotice error={engine.error} />}
      {e && (
        <dl className={s.kv}>
          <dt>{a.version}</dt>
          <dd data-testid="engine-version">{e.version}</dd>
          <dt>{a.buildId}</dt>
          <dd className={s.mono}>{e.build_id}</dd>
          <dt>{a.mode}</dt>
          <dd>{a.modes[e.mode] ?? e.mode}</dd>
          <dt>{a.api}</dt>
          <dd>v{e.api}</dd>
          <dt>{a.baseUrl}</dt>
          <dd className={s.mono}>{e.base_url}</dd>
          <dt>{a.pid}</dt>
          <dd>{e.pid}</dd>
          <dt>{a.started}</dt>
          <dd>{formatInstant(e.started_at)}</dd>
          <dt>{a.activeJobs}</dt>
          <dd>{e.active_jobs}</dd>
          <dt>{a.client}</dt>
          <dd>{a.clients[platform.mode]}</dd>
          <dt>{a.stream}</dt>
          <dd>{a.streams[stream]}</dd>
        </dl>
      )}
      <p className={s.muted}>{a.telemetry}</p>
      <div className={s.row}>
        <Link className={buttonClass()} to="/setup">
          {a.rerunSetup}
        </Link>
      </div>
    </div>
  );
}

/** The optional browser UI: a switch, the address, and a way to open it. */
function WebSection({ settings }: { settings: Settings }) {
  const qc = useQueryClient();
  const engine = useEngine().data;
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const enabled = settings.web?.enabled ?? false;
  const address = engine?.base_url ?? '';

  async function toggle(next: boolean) {
    setBusy(true);
    setError(null);
    try {
      const saved = await api.patchSettings({ web: { enabled: next } });
      if (saved && typeof saved === 'object') qc.setQueryData(keys.settings, saved);
      await qc.invalidateQueries({ queryKey: keys.settings });
      pushToast({ kind: 'success', message: next ? t.web.enabled : t.web.disabled });
    } catch (e) {
      setError(e);
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className={s.stack}>
      <label className={s.check}>
        <input type="checkbox" role="switch" checked={enabled} disabled={busy} onChange={(e) => void toggle(e.target.checked)} />
        <span>
          {t.web.toggle}
          <span className={s.muted} style={{ display: 'block' }}>
            {t.web.hint}
          </span>
        </span>
      </label>
      {enabled && platform.mode === 'web' && <p className={s.muted}>{t.web.selfOff}</p>}
      {enabled && address ? (
        <>
          <dl className={s.kv}>
            <dt>{t.web.address}</dt>
            <dd className={s.mono} data-testid="web-address">
              {address}
            </dd>
          </dl>
          {platform.mode === 'desktop' && (
            <div className={s.row}>
              <Button small variant="accent" onClick={() => platform.openExternal(address)}>
                {t.web.open}
              </Button>
            </div>
          )}
        </>
      ) : (
        !enabled && <p className={s.muted}>{t.web.off}</p>
      )}
      {error != null && <ErrorNotice error={error} />}
    </div>
  );
}

export function SettingsPage() {
  const settings = useSettings();
  const reveal = platform.revealInFileManager;

  if (settings.isPending) return <Page title={t.title}><Loading /></Page>;
  if (settings.isError) {
    return (
      <Page title={t.title}>
        <ErrorState error={settings.error} onRetry={() => void settings.refetch()} />
      </Page>
    );
  }
  const data = settings.data;

  return (
    <Page title={t.title}>
      <div className={s.sections}>
        <Section id="llm" title={t.sections.llm}>
          <p className={s.text} style={{ marginBottom: 12 }}>
            {t.llm.intro}
          </p>
          <LlmForm settings={data} />
        </Section>
        <Section id="asr" title={t.sections.asr}>
          <AsrForm settings={data} />
        </Section>
        <Section id="tools" title={t.sections.tools}>
          <ToolsPanel />
        </Section>
        <Section id="web" title={t.sections.web}>
          <WebSection settings={data} />
        </Section>
        <Section id="data" title={t.sections.data}>
          <div className={s.stack}>
            <dl className={s.kv}>
              <dt>{t.data.dir}</dt>
              <dd className={s.mono} data-testid="data-dir">
                {data.data_dir || zh.app.none}
              </dd>
              {data.config_dir && (
                <>
                  <dt>{t.data.configDir}</dt>
                  <dd className={s.mono}>{data.config_dir}</dd>
                </>
              )}
            </dl>
            {reveal && data.data_dir && (
              <div className={s.row}>
                <Button small onClick={() => void reveal(data.data_dir!)}>
                  {t.data.reveal}
                </Button>
              </div>
            )}
            <p className={s.text}>{t.data.howto}</p>
            <CodeBlock code={t.data.cmd} />
            <p className={s.muted}>{t.data.note}</p>
          </div>
        </Section>
        <Section id="agent" title={t.sections.agent}>
          <AgentSection />
        </Section>
        <Section id="about" title={t.sections.about}>
          <AboutSection />
        </Section>
      </div>
    </Page>
  );
}
