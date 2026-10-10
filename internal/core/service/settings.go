package service

import (
	"context"

	"watchink/internal/app"
	"watchink/internal/capability/llm"
	"watchink/internal/capability/tools"
	"watchink/internal/core/jobs"
	"watchink/internal/core/pipeline"
	"watchink/internal/infra/config"
)

// GetSettings implements app.Service; credentials are write-only.
func (s *Service) GetSettings(context.Context) (*app.Settings, error) {
	cfg := s.Settings()
	config.Describe(&cfg)
	return &cfg, nil
}

// PatchSettings implements app.Service. The engine is the only writer of
// config.toml while it runs, and a change applies at once.
func (s *Service) PatchSettings(ctx context.Context, patch map[string]any) (*app.Settings, error) {
	s.mu.Lock()
	next, err := config.ApplyPatch(s.settings, patch)
	if err == nil {
		if err = config.Save(s.dataDir, next); err == nil {
			s.settings = next
		}
	}
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	s.tools.SetNetwork(next.Download.Mirrors, next.Network.Proxy)
	s.registerSecret(next)
	s.publishTools()
	return s.GetSettings(ctx)
}

// SetSecret implements app.Service.
func (s *Service) SetSecret(_ context.Context, name, value string) error {
	if name != app.SecretLLMAPIKey {
		return app.E(app.CodeInvalidArgument, "未知的凭据名：%s", name)
	}
	if value != "" && s.addSecret != nil {
		s.addSecret(value)
	}
	return config.SetSecret(name, value)
}

// TestLLM implements app.Service.
func (s *Service) TestLLM(ctx context.Context) (*app.LLMTestResult, error) {
	return llm.Test(ctx, pipeline.LLMConfig(s.Settings(), func(ctx context.Context) (func(), error) {
		return s.sched.Acquire(ctx, jobs.ResourceLLM)
	})), nil
}

// ListLLMModels implements app.Service.
func (s *Service) ListLLMModels(ctx context.Context, req app.LLMModelsRequest) (*app.LLMModelList, error) {
	cfg := pipeline.LLMConfig(s.Settings(), nil)
	if req.BaseURL != "" {
		cfg.BaseURL = req.BaseURL
	}
	if req.APIKey != "" {
		cfg.APIKey = req.APIKey
		if s.addSecret != nil {
			s.addSecret(req.APIKey)
		}
	}
	models, err := llm.ListModels(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &app.LLMModelList{BaseURL: cfg.BaseURL, Models: models}, nil
}

// GetTools implements app.Service.
func (s *Service) GetTools(context.Context) (*app.ToolsStatus, error) {
	cfg := s.Settings()
	st := s.tools.Status(pipeline.ASRModel(cfg, s.tools), cfg.ASR.VAD)
	if st.Items == nil {
		st.Items = []app.ToolItem{}
	}
	if st.DefaultASRModel == "" {
		st.DefaultASRModel = s.tools.DefaultASRModel()
	}
	return &st, nil
}

// PrepareTools implements app.Service: one prepare_tools job at a time.
func (s *Service) PrepareTools(ctx context.Context, req app.PrepareToolsRequest) (*app.Job, error) {
	if active, err := s.st.ActiveJobByKind(ctx, app.JobKindPrepareTools); err != nil {
		return nil, err
	} else if active != nil {
		j := s.sched.ToDTO(active)
		return &j, nil
	}
	items := req.Items
	if len(items) == 0 {
		// Default: everything a local-file import needs and lacks.
		st, _ := s.GetTools(ctx)
		for _, it := range st.Items {
			if it.Required && it.Status != app.ToolReady {
				items = append(items, it.Name)
			}
		}
	} else {
		known := map[string]bool{}
		st, _ := s.GetTools(ctx)
		for _, it := range st.Items {
			known[it.Name] = true
		}
		for _, n := range items {
			if !known[n] {
				return nil, app.E(app.CodeInvalidArgument, "未知的工具或模型：%s", n)
			}
		}
	}
	j, err := s.sched.Submit(ctx, jobs.JobParams{Kind: app.JobKindPrepareTools, Options: pipeline.JobOptions{Items: items}})
	if err != nil {
		return nil, err
	}
	return &j, nil
}

// UpdateTool implements app.Service (v0: yt-dlp only).
func (s *Service) UpdateTool(ctx context.Context, name string) (*app.ToolsStatus, error) {
	if name != tools.ToolYtDlp {
		return nil, app.E(app.CodeInvalidArgument, "v0 只支持更新 yt-dlp")
	}
	if _, err := s.tools.UpdateTool(ctx, name); err != nil {
		return nil, err
	}
	return s.GetTools(ctx)
}

// ImportTool implements app.Service.
func (s *Service) ImportTool(ctx context.Context, req app.ToolImportRequest) (*app.ToolsStatus, error) {
	if req.Name == "" || req.Path == "" {
		return nil, app.E(app.CodeInvalidArgument, "name 和 path 不能为空")
	}
	if err := s.tools.Import(ctx, req.Name, req.Path); err != nil {
		return nil, err
	}
	return s.GetTools(ctx)
}
