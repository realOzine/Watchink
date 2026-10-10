package pipeline

import (
	"context"
	"net/http"
	"net/url"
	"time"

	"watchink/internal/app"
	"watchink/internal/capability/llm"
	"watchink/internal/capability/notes"
	"watchink/internal/capability/tools"
	"watchink/internal/infra/config"
)

// The functions in this file translate app.Settings into what the asr, llm
// and notes packages take. The service layer uses the exported ones too, so
// "is the model configured" means the same thing everywhere.

// ASRModel returns the configured ASR model ID or the platform default.
func ASRModel(cfg app.Settings, t *tools.Manager) string {
	if cfg.ASR.Model != "" {
		return cfg.ASR.Model
	}
	return t.DefaultASRModel()
}

// language returns the language requested for the job: the import option,
// else the configured default.
func (p *Pipeline) language(opt JobOptions) string {
	if opt.Language != "" {
		return opt.Language
	}
	if l := p.cfg().ASR.Language; l != "" {
		return l
	}
	return "auto"
}

// HTTPClient returns a client honouring network.proxy; an empty
// proxy follows the environment.
func HTTPClient(proxy string) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if proxy != "" {
		if u, err := url.Parse(proxy); err == nil {
			tr.Proxy = http.ProxyURL(u)
		}
	}
	return &http.Client{Transport: tr}
}

// LLMConfig builds the online model configuration from the settings and the
// resolved credential.
func LLMConfig(cfg app.Settings, acquire func(context.Context) (func(), error)) llm.Config {
	key, _ := config.ResolveLLMAPIKey(cfg)
	timeout, err := config.ParseDuration(cfg.LLM.Timeout)
	if err != nil || timeout <= 0 {
		timeout = 120 * time.Second
	}
	return llm.Config{
		BaseURL: cfg.LLM.BaseURL, Model: cfg.LLM.Model, APIKey: key,
		StructuredOutput: cfg.LLM.StructuredOutput, Temperature: cfg.LLM.Temperature,
		Timeout: timeout, HTTPClient: HTTPClient(cfg.Network.Proxy), Acquire: acquire,
	}
}

// LLMConfigured reports whether notes can be generated with cfg.
func LLMConfigured(cfg app.Settings) bool {
	return llm.Validate(LLMConfig(cfg, nil)) == nil
}

// modelID identifies the online model in fingerprints: a different endpoint
// or model must not reuse another one's output.
func modelID(cfg app.Settings) map[string]any {
	return map[string]any{"base_url": cfg.LLM.BaseURL, "model": cfg.LLM.Model, "temperature": cfg.LLM.Temperature}
}

func (p *Pipeline) chunkOptions() notes.Options {
	cfg := p.cfg()
	d, err := config.ParseDuration(cfg.Note.ChunkMaxDuration)
	if err != nil || d <= 0 {
		d = 10 * time.Minute
	}
	return notes.Options{MaxTokens: cfg.Note.ChunkMaxTokens, MaxDuration: d}
}
