import { useQueryClient } from '@tanstack/react-query';
import { useEffect, useId, useRef, useState, type FormEvent } from 'react';
import { api } from '../api';
import { describeError } from '../lib/describeError';
import { keys } from '../api/keys';
import type { LLMTestResult, Settings } from '../api/types';
import { zh } from '../i18n/zh';
import { pushToast } from '../lib/toast';
import s from '../pages/pages.module.css';
import { Button, ErrorNotice, Field, Notice, Spinner, inputClass } from './ui';

const t = zh.settings.llm;

/** Presets only fill in base_url; model name and key stay with the user. */
export const LLM_PRESETS: { id: string; baseUrl: string }[] = [
  { id: 'openai', baseUrl: 'https://api.openai.com/v1' },
  { id: 'deepseek', baseUrl: 'https://api.deepseek.com/v1' },
  { id: 'dashscope', baseUrl: 'https://dashscope.aliyuncs.com/compatible-mode/v1' },
  { id: 'siliconflow', baseUrl: 'https://api.siliconflow.cn/v1' },
  { id: 'openrouter', baseUrl: 'https://openrouter.ai/api/v1' },
];

const normalize = (url: string) => url.trim().replace(/\/+$/, '');
export const presetFor = (baseUrl: string): string =>
  LLM_PRESETS.find((p) => p.baseUrl === normalize(baseUrl))?.id ?? 'custom';

function keyStatus(llm: Settings['llm']): string {
  if (!llm.api_key_configured) return t.keyMissing;
  const tail = llm.api_key_last4 ? t.keyConfigured(llm.api_key_last4) : t.keyConfiguredNoTail;
  const src = llm.api_key_source ?? '';
  const where = src.startsWith('env:') ? t.keySourceEnv(src.slice(4)) : src === 'env' ? t.keySource.env : src === 'file' ? t.keySource.file : '';
  return where ? `${tail}（${where}）` : tail;
}

/** The model list of the endpoint currently typed into the form. */
type ModelsState =
  | { kind: 'idle' }
  | { kind: 'loading' }
  | { kind: 'ready'; models: string[] }
  | { kind: 'failed'; reason: string };

const MANUAL = '__manual__';
const validBaseUrl = (url: string) => /^https?:\/\/[^\s/]+/i.test(url.trim());

type TestState = { kind: 'idle' } | { kind: 'running' } | { kind: 'done'; result: LLMTestResult } | { kind: 'error'; error: unknown };

export function LlmForm({ settings }: { settings: Settings }) {
  const qc = useQueryClient();
  const uid = useId();
  const llm = settings.llm;
  const [baseUrl, setBaseUrl] = useState(llm.base_url);
  const [model, setModel] = useState(llm.model);
  const [structured, setStructured] = useState(llm.structured_output || 'auto');
  const [apiKey, setApiKey] = useState('');
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<unknown>(null);
  const [test, setTest] = useState<TestState>({ kind: 'idle' });

  const [models, setModels] = useState<ModelsState>({ kind: 'idle' });
  // Manual entry is the fallback for endpoints without a model listing and
  // for a model the list does not show.
  const [manual, setManual] = useState(false);
  const [reload, setReload] = useState(0);
  const firstLoad = useRef(true);

  // Ask the endpoint for its models whenever the address or the typed key
  // changes (debounced), using the unsaved values so the list is available
  // before anything is stored.
  useEffect(() => {
    const url = normalize(baseUrl);
    if (!validBaseUrl(url)) {
      setModels({ kind: 'idle' });
      return;
    }
    const ctl = new AbortController();
    const delay = firstLoad.current ? 0 : 500;
    firstLoad.current = false;
    const timer = setTimeout(() => {
      setModels({ kind: 'loading' });
      api
        .listLlmModels({ base_url: url, ...(apiKey.trim() ? { api_key: apiKey.trim() } : {}) }, ctl.signal)
        .then((res) => {
          const list = res.models ?? [];
          setModels(list.length ? { kind: 'ready', models: list } : { kind: 'failed', reason: t.modelListEmpty });
        })
        .catch((e: unknown) => {
          if (ctl.signal.aborted) return;
          setModels({ kind: 'failed', reason: describeError(e).message });
        });
    }, delay);
    return () => {
      clearTimeout(timer);
      ctl.abort();
    };
  }, [baseUrl, apiKey, reload]);

  const fromEnv = (llm.api_key_source ?? '').startsWith('env');
  const busy = saving || test.kind === 'running';

  async function save(): Promise<boolean> {
    setSaving(true);
    setSaveError(null);
    try {
      const next = await api.patchSettings({
        llm: { base_url: normalize(baseUrl), model: model.trim(), structured_output: structured },
      });
      // The key is write-only: it goes to its own endpoint and never comes back.
      if (apiKey.trim()) {
        await api.setLlmApiKey(apiKey.trim());
        setApiKey('');
      }
      if (next && typeof next === 'object' && !apiKey.trim()) qc.setQueryData(keys.settings, next);
      await qc.invalidateQueries({ queryKey: keys.settings });
      void qc.invalidateQueries({ queryKey: keys.engine });
      return true;
    } catch (e) {
      setSaveError(e);
      return false;
    } finally {
      setSaving(false);
    }
  }

  async function onSubmit(e: FormEvent) {
    e.preventDefault();
    if (await save()) pushToast({ kind: 'success', message: zh.app.saved });
  }

  // The test endpoint takes no arguments: it tests what is stored, so save first.
  async function onTest() {
    setTest({ kind: 'idle' });
    if (!(await save())) return;
    setTest({ kind: 'running' });
    try {
      setTest({ kind: 'done', result: await api.testLlm() });
    } catch (e) {
      setTest({ kind: 'error', error: e });
    }
  }

  async function removeKey() {
    setSaving(true);
    setSaveError(null);
    try {
      await api.setLlmApiKey('');
      await qc.invalidateQueries({ queryKey: keys.settings });
      void qc.invalidateQueries({ queryKey: keys.engine });
      pushToast({ kind: 'info', message: t.keyRemoved });
    } catch (e) {
      setSaveError(e);
    } finally {
      setSaving(false);
    }
  }

  return (
    <form className={s.stack} onSubmit={(e) => void onSubmit(e)}>
      <Notice tone="info">{t.privacy}</Notice>
      <div className={s.grid2}>
        <Field label={t.preset} hint={t.presetHint} htmlFor={`${uid}-preset`}>
          <select
            id={`${uid}-preset`}
            className={inputClass}
            value={presetFor(baseUrl)}
            onChange={(e) => {
              const p = LLM_PRESETS.find((x) => x.id === e.target.value);
              if (p) setBaseUrl(p.baseUrl);
            }}
          >
            {LLM_PRESETS.map((p) => (
              <option key={p.id} value={p.id}>
                {t.presets[p.id]}
              </option>
            ))}
            <option value="custom">{t.presets.custom}</option>
          </select>
        </Field>
        <Field label={t.baseUrl} htmlFor={`${uid}-base`}>
          <input
            id={`${uid}-base`}
            className={inputClass}
            type="url"
            autoComplete="off"
            spellCheck={false}
            placeholder={t.baseUrlPlaceholder}
            value={baseUrl}
            onChange={(e) => setBaseUrl(e.target.value)}
          />
        </Field>
        <Field
          label={t.model}
          htmlFor={`${uid}-model`}
          hint={
            <>
              {models.kind === 'idle' && t.modelNeedBaseUrl}
              {models.kind === 'loading' && t.modelLoading}
              {models.kind === 'ready' && t.modelCount(models.models.length)}
              {models.kind === 'failed' && t.modelListFailed(models.reason)}
              {models.kind !== 'idle' && models.kind !== 'loading' && (
                <>
                  {' · '}
                  <button type="button" className={s.linkButton} onClick={() => setReload((n) => n + 1)}>
                    {t.modelRefresh}
                  </button>
                </>
              )}
              {models.kind === 'ready' && manual && (
                <>
                  {' · '}
                  <button type="button" className={s.linkButton} onClick={() => setManual(false)}>
                    {t.modelBackToList}
                  </button>
                </>
              )}
            </>
          }
        >
          {models.kind === 'ready' && !manual ? (
            <select
              id={`${uid}-model`}
              className={inputClass}
              value={model}
              onChange={(e) => {
                if (e.target.value === MANUAL) setManual(true);
                else setModel(e.target.value);
              }}
            >
              {!model && <option value="">{t.modelChoose}</option>}
              {model && !models.models.includes(model) && <option value={model}>{t.modelCurrent(model)}</option>}
              {models.models.map((m) => (
                <option key={m} value={m}>
                  {m}
                </option>
              ))}
              <option value={MANUAL}>{t.modelManualOption}</option>
            </select>
          ) : models.kind === 'loading' && !manual ? (
            <select id={`${uid}-model`} className={inputClass} value={model} disabled>
              <option value={model}>{model || t.modelLoading}</option>
            </select>
          ) : (
            <input
              id={`${uid}-model`}
              className={inputClass}
              autoComplete="off"
              spellCheck={false}
              placeholder={t.modelPlaceholder}
              value={model}
              onChange={(e) => setModel(e.target.value)}
            />
          )}
        </Field>
        <Field label={t.structured} htmlFor={`${uid}-so`}>
          <select id={`${uid}-so`} className={inputClass} value={structured} onChange={(e) => setStructured(e.target.value)}>
            {Object.entries(t.structuredOptions).map(([value, label]) => (
              <option key={value} value={value}>
                {label}
              </option>
            ))}
          </select>
        </Field>
      </div>
      <Field
        label={t.apiKey}
        htmlFor={`${uid}-key`}
        hint={
          <>
            <span data-testid="key-status">{keyStatus(llm)}</span> · {fromEnv ? t.keyFromEnvNote : t.keyWriteOnly}
          </>
        }
      >
        <input
          id={`${uid}-key`}
          className={inputClass}
          type="password"
          autoComplete="off"
          spellCheck={false}
          placeholder={llm.api_key_configured ? t.apiKeyPlaceholderReplace : t.apiKeyPlaceholderNew}
          value={apiKey}
          onChange={(e) => setApiKey(e.target.value)}
        />
      </Field>

      {saveError != null && <ErrorNotice error={saveError} />}
      {test.kind === 'done' && test.result.ok && (
        <Notice tone="ok" role="status">
          {test.result.structured_output
            ? t.testOk(test.result.latency_ms, test.result.structured_output)
            : t.testOkPlain(test.result.latency_ms)}
        </Notice>
      )}
      {test.kind === 'done' && !test.result.ok && (
        <ErrorNotice
          title={t.testFailed}
          error={test.result.error ?? { code: 'INTERNAL', message: t.testFailed }}
        />
      )}
      {test.kind === 'error' && <ErrorNotice title={t.testFailed} error={test.error} />}

      <div className={s.row}>
        <Button type="submit" variant="primary" disabled={busy}>
          {t.save}
        </Button>
        <Button variant="accent" disabled={busy} onClick={() => void onTest()}>
          {test.kind === 'running' ? (
            <>
              <Spinner /> {t.testing}
            </>
          ) : (
            t.test
          )}
        </Button>
        {llm.api_key_configured && llm.api_key_source === 'file' && (
          <Button variant="ghost" disabled={busy} onClick={() => void removeKey()}>
            {t.removeKey}
          </Button>
        )}
      </div>
    </form>
  );
}
