import { useState } from 'react';
import { useNavigate } from 'react-router';
import { useEngine, useSettings } from '../api/hooks';
import { SETUP_DISMISSED_KEY } from '../components/AppShell';
import { LlmForm } from '../components/LlmForm';
import { ToolsPanel } from '../components/ToolsPanel';
import { Button, ErrorState, Loading, Notice, cx } from '../components/ui';
import { zh } from '../i18n/zh';
import { safeStorage } from '../lib/store';
import s from './pages.module.css';

const t = zh.setup;

/** First-run wizard (§14.1). Every step can be skipped; nothing here is mandatory. */
export function SetupPage() {
  const navigate = useNavigate();
  const [step, setStep] = useState(0);
  const engine = useEngine();
  const settings = useSettings();
  const last = t.steps.length - 1;

  function finish() {
    // Remember the choice so the redirect from the library does not come back.
    safeStorage.set(SETUP_DISMISSED_KEY, '1');
    void navigate('/');
  }

  let body;
  if (step === 0) {
    body = (
      <div className={s.stack}>
        <div>
          <h2 className={s.h2}>{t.tools.title}</h2>
          <p className={s.text}>{t.tools.body}</p>
        </div>
        <ToolsPanel showUpdate={false} />
        <p className={s.muted}>{t.tools.lazyNote}</p>
      </div>
    );
  } else if (step === 1) {
    body = (
      <div className={s.stack}>
        <div>
          <h2 className={s.h2}>{t.llm.title}</h2>
          <p className={s.text}>{t.llm.body}</p>
        </div>
        {settings.isPending ? (
          <Loading />
        ) : settings.isError ? (
          <ErrorState error={settings.error} onRetry={() => void settings.refetch()} />
        ) : (
          <LlmForm settings={settings.data} />
        )}
      </div>
    );
  } else {
    const e = engine.data;
    body = (
      <div className={s.stack}>
        <h2 className={s.h2}>{t.done.title}</h2>
        {e?.tools_ready && e.llm_configured && <Notice tone="ok">{t.done.allSet}</Notice>}
        {e && !e.tools_ready && <Notice tone="warn">{t.done.toolsPending}</Notice>}
        {e && !e.llm_configured && <Notice tone="warn">{t.done.llmPending}</Notice>}
      </div>
    );
  }

  return (
    <div className={s.wizard}>
      <h1 className={s.wizardTitle}>{t.title}</h1>
      <p className={s.text}>{t.intro}</p>
      <ol className={s.steps} aria-label={t.stepOf(step + 1, t.steps.length)}>
        {t.steps.map((name, i) => (
          <li
            key={name}
            className={cx(s.step, i < step && s.stepDone, i === step && s.stepCurrent)}
            aria-current={i === step ? 'step' : undefined}
          >
            {i + 1}. {name}
          </li>
        ))}
      </ol>
      <div className={cx(s.card, s.section)} data-step={step}>
        {body}
      </div>
      <div className={s.wizardNav}>
        <div className={s.row}>
          {step > 0 && <Button onClick={() => setStep(step - 1)}>{t.prev}</Button>}
        </div>
        <div className={s.row}>
          {step < last ? (
            <>
              <Button variant="ghost" onClick={() => setStep(step + 1)}>
                {t.skip}
              </Button>
              <Button variant="primary" onClick={() => setStep(step + 1)}>
                {t.next}
              </Button>
            </>
          ) : (
            <Button variant="primary" onClick={finish}>
              {t.done.finish}
            </Button>
          )}
        </div>
      </div>
    </div>
  );
}
