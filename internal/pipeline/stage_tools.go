package pipeline

import (
	"context"

	"watchink/internal/app"
	"watchink/internal/jobs"
)

// ensure_tools (first stage of process_video) and prepare (the only stage of
// prepare_tools) both install tools and models through tools.Manager.

func (p *Pipeline) ensureTools(ctx context.Context, sc *jobs.StageContext) (*jobs.StageResult, error) {
	opt, err := options(sc)
	if err != nil {
		return nil, err
	}
	cfg := p.cfg()
	model := ASRModel(cfg, p.tools)
	missing := p.missing(p.tools.Needed(opt.SourceType, model, cfg.ASR.VAD), model, cfg.ASR.VAD)
	if len(missing) == 0 {
		return &jobs.StageResult{}, nil
	}
	if !cfg.Tools.AutoPrepare {
		return nil, app.E(app.CodeToolMissing, "缺少工具或模型：%v", missing).
			WithHint("运行 watchink tools prepare，或在设置中开启自动准备").WithDetail("missing", missing)
	}
	return &jobs.StageResult{}, p.ensure(ctx, sc, missing)
}

func (p *Pipeline) prepareTools(ctx context.Context, sc *jobs.StageContext) (*jobs.StageResult, error) {
	opt, err := options(sc)
	if err != nil {
		return nil, err
	}
	return &jobs.StageResult{}, p.ensure(ctx, sc, opt.Items)
}

// missing filters names down to the items that are not ready.
func (p *Pipeline) missing(names []string, model string, vad bool) []string {
	ready := map[string]bool{}
	for _, it := range p.tools.Status(model, vad).Items {
		ready[it.Name] = it.Status == app.ToolReady
	}
	var out []string
	for _, n := range names {
		if !ready[n] {
			out = append(out, n)
		}
	}
	return out
}

// ensure installs names, reporting progress as the mean of the items.
func (p *Pipeline) ensure(ctx context.Context, sc *jobs.StageContext, names []string) error {
	if len(names) == 0 {
		return nil
	}
	frac := map[string]float64{}
	return p.tools.Ensure(ctx, names, func(name string, got, total int64) {
		if total > 0 {
			frac[name] = float64(got) / float64(total)
		}
		sum := 0.0
		for _, f := range frac {
			sum += f
		}
		sc.Progress(sum / float64(len(names)))
	})
}
