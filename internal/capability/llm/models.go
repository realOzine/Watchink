package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"watchink/internal/app"
)

// modelsTimeout bounds the model listing: it backs a settings dropdown and
// must answer quickly.
const modelsTimeout = 20 * time.Second

// ListModels returns the model IDs the endpoint offers (GET {base_url}/models),
// sorted. Only BaseURL, APIKey and HTTPClient of cfg are used. Endpoints that
// do not implement the listing yield MODEL_UNAVAILABLE; the caller then lets
// the user type the model name.
func ListModels(ctx context.Context, cfg Config) ([]string, error) {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	probe := cfg
	probe.Model = "-" // the listing needs no model; satisfy Validate
	if err := Validate(probe); err != nil {
		return nil, err
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{}
	}
	ctx, cancel := context.WithTimeout(ctx, modelsTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/models", nil)
	if err != nil {
		return nil, app.E(app.CodeModelNotConfigured, "base_url 无效")
	}
	req.Header.Set("Accept", "application/json")
	if cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, app.E(app.CodeModelUnavailable, "无法连接在线模型服务").
			WithHint("检查网络、代理和 base_url").WithDetail("cause", redact(transportCause(err).Error(), cfg.APIKey))
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, app.E(app.CodeModelAuthFailed, "在线模型鉴权失败（HTTP %d）", resp.StatusCode).
			WithHint("检查 API Key 是否正确")
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return nil, app.E(app.CodeModelUnavailable, "该接口没有提供模型列表（HTTP %d）", resp.StatusCode).
			WithHint("可以手动输入模型名").WithDetail("http_status", resp.StatusCode)
	}
	// OpenAI shape: {"data":[{"id":"…"}]}; some gateways use "models" and
	// "name", or return a bare array.
	type entry struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	var body struct {
		Data   []entry `json:"data"`
		Models []entry `json:"models"`
	}
	list := []entry(nil)
	if json.Unmarshal(raw, &body) == nil {
		list = append(body.Data, body.Models...)
	}
	if len(list) == 0 {
		_ = json.Unmarshal(raw, &list)
	}
	seen := map[string]bool{}
	out := []string{}
	for _, e := range list {
		id := strings.TrimSpace(e.ID)
		if id == "" {
			id = strings.TrimPrefix(strings.TrimSpace(e.Name), "models/")
		}
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	if len(out) == 0 {
		return nil, app.E(app.CodeModelUnavailable, "该接口返回的模型列表为空或无法识别").WithHint("可以手动输入模型名")
	}
	sort.Strings(out)
	return out, nil
}
