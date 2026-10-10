package config

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"

	"watchink/internal/app"
)

// SettingsFileName is the settings file inside the data dir (§12).
const SettingsFileName = "config.toml"

// SettingsFile returns <data>/config.toml.
func SettingsFile(dataDir string) string { return filepath.Join(dataDir, SettingsFileName) }

// Load returns the defaults of §12 overlaid by <data>/config.toml. A missing
// file yields the defaults; unknown keys are ignored so that a config written
// by a newer build still loads; a file that is not valid TOML or that holds an
// invalid value is INVALID_ARGUMENT naming the file.
//
// The read-only DataDir field of the result is set to the absolute dataDir.
func Load(dataDir string) (app.Settings, error) {
	s := app.DefaultSettings()
	if abs, err := filepath.Abs(dataDir); err == nil {
		s.DataDir = abs
	} else {
		s.DataDir = dataDir
	}
	p := SettingsFile(dataDir)
	b, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return s, nil
		}
		return s, app.E(app.CodeInternal, "无法读取配置文件 %s：%v", p, err).WithCause(err)
	}
	if err := toml.Unmarshal(b, &s); err != nil {
		return app.DefaultSettings(), app.E(app.CodeInvalidArgument, "配置文件 %s 不是合法的 TOML：%v", p, err).
			WithHint("请修正或删除该文件后重试").WithDetail("path", p).WithCause(err)
	}
	normalize(&s)
	if err := Validate(s); err != nil {
		e := app.AsError(err)
		e.Message = fmt.Sprintf("配置文件 %s 无效：%s", p, e.Message)
		return s, e.WithDetail("path", p)
	}
	return s, nil
}

// normalize replaces nil collections by empty ones so that the JSON shape is
// stable ("mirrors": {} rather than null).
func normalize(s *app.Settings) {
	if s.Download.Mirrors == nil {
		s.Download.Mirrors = map[string]string{}
	}
	if s.Security.ImportRoots == nil {
		s.Security.ImportRoots = []string{}
	}
}

// Save writes the TOML-tagged fields of s to <data>/config.toml atomically
// (temp file + rename) with mode 0600. Read-only fields (data_dir,
// config_dir, credential state) carry `toml:"-"` and are never written.
func Save(dataDir string, s app.Settings) error {
	normalize(&s)
	b, err := toml.Marshal(s)
	if err != nil {
		return app.E(app.CodeInternal, "无法序列化配置：%v", err).WithCause(err)
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return app.E(app.CodeInternal, "无法创建数据目录 %s：%v", dataDir, err).WithCause(err)
	}
	if err := writeFileAtomic(SettingsFile(dataDir), b, 0o600); err != nil {
		return app.E(app.CodeInternal, "无法写入配置文件：%v", err).WithCause(err)
	}
	return nil
}

// writeFileAtomic writes data to a temp file next to path and renames it over
// path, so readers see either the old or the new content.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	cleanup := func() { f.Close(); os.Remove(tmp) }
	if err := f.Chmod(mode); err != nil {
		cleanup()
		return err
	}
	if _, err := f.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := f.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// ParseDuration parses a non-negative Go duration string such as "10m",
// "120s" or "1h30m".
func ParseDuration(s string) (time.Duration, error) {
	d, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q (examples: 90s, 10m, 1h30m)", s)
	}
	if d < 0 {
		return 0, fmt.Errorf("invalid duration %q: must not be negative", s)
	}
	return d, nil
}

var sizeUnits = map[string]float64{
	"": 1, "b": 1,
	"kb": 1e3, "mb": 1e6, "gb": 1e9, "tb": 1e12,
	"k": 1 << 10, "m": 1 << 20, "g": 1 << 30, "t": 1 << 40,
	"kib": 1 << 10, "mib": 1 << 20, "gib": 1 << 30, "tib": 1 << 40,
}

// ParseSize parses a byte size: plain bytes ("1048576") or a number with a
// unit — binary KiB/MiB/GiB/TiB (also K/M/G/T) or decimal KB/MB/GB/TB. Units
// are case-insensitive and a decimal fraction is allowed ("1.5GiB").
func ParseSize(s string) (int64, error) {
	t := strings.TrimSpace(s)
	i := 0
	for i < len(t) && (t[i] >= '0' && t[i] <= '9' || t[i] == '.') {
		i++
	}
	num, unit := t[:i], strings.ToLower(strings.TrimSpace(t[i:]))
	mult, ok := sizeUnits[unit]
	if num == "" || !ok {
		return 0, fmt.Errorf("invalid size %q (examples: 8GiB, 500MiB, 1GB, 1048576)", s)
	}
	if unit == "" || unit == "b" {
		// Plain bytes must be an exact integer.
		n, err := strconv.ParseInt(num, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid size %q", s)
		}
		return n, nil
	}
	f, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	v := f * mult
	if math.IsInf(v, 0) || v >= math.MaxInt64 {
		return 0, fmt.Errorf("invalid size %q: too large", s)
	}
	return int64(v), nil
}

var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func invalid(key, format string, args ...any) *app.Error {
	return app.E(app.CodeInvalidArgument, "配置项 %s 无效：%s", key, fmt.Sprintf(format, args...)).
		WithDetail("key", key)
}

// checkURL reports whether raw is an absolute URL with one of the schemes and
// a host.
func checkURL(raw string, schemes ...string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("不是合法的 URL")
	}
	ok := false
	for _, s := range schemes {
		if strings.EqualFold(u.Scheme, s) {
			ok = true
		}
	}
	if !ok {
		return fmt.Errorf("协议必须是 %s", strings.Join(schemes, " / "))
	}
	if u.Hostname() == "" {
		return errors.New("缺少主机名")
	}
	return nil
}

func oneOf(v string, allowed ...string) bool {
	for _, a := range allowed {
		if v == a {
			return true
		}
	}
	return false
}

// Validate checks every setting against §12 and returns INVALID_ARGUMENT
// naming the first offending key (in details.key as well).
func Validate(s app.Settings) error {
	// 0 lets the OS pick a port; §12 already falls back to a random port
	// when the configured one is taken.
	if s.Engine.Port < 0 || s.Engine.Port > 65535 {
		return invalid("engine.port", "端口必须在 0–65535 之间")
	}
	if _, err := ParseDuration(s.Engine.IdleExit); err != nil {
		return invalid("engine.idle_exit", "%v", err)
	}
	if !oneOf(s.ASR.Language, "auto", "zh", "en") {
		return invalid("asr.language", "只能是 auto / zh / en")
	}
	if s.ASR.Threads < 0 {
		return invalid("asr.threads", "不能为负数（0 表示自动）")
	}
	if s.LLM.BaseURL != "" {
		if err := checkURL(s.LLM.BaseURL, "http", "https"); err != nil {
			return invalid("llm.base_url", "%v", err)
		}
	}
	if s.LLM.APIKeyEnv != "" && !envNameRE.MatchString(s.LLM.APIKeyEnv) {
		return invalid("llm.api_key_env", "不是合法的环境变量名")
	}
	if !oneOf(s.LLM.StructuredOutput, "auto", "json_schema", "json_object", "prompt") {
		return invalid("llm.structured_output", "只能是 auto / json_schema / json_object / prompt")
	}
	if math.IsNaN(s.LLM.Temperature) || s.LLM.Temperature < 0 || s.LLM.Temperature > 2 {
		return invalid("llm.temperature", "必须在 0–2 之间")
	}
	if d, err := ParseDuration(s.LLM.Timeout); err != nil {
		return invalid("llm.timeout", "%v", err)
	} else if d <= 0 {
		return invalid("llm.timeout", "必须大于 0")
	}
	if s.LLM.MaxConcurrency < 1 {
		return invalid("llm.max_concurrency", "必须不小于 1")
	}
	if s.Note.ChunkMaxTokens <= 0 {
		return invalid("note.chunk_max_tokens", "必须大于 0")
	}
	if d, err := ParseDuration(s.Note.ChunkMaxDuration); err != nil {
		return invalid("note.chunk_max_duration", "%v", err)
	} else if d <= 0 {
		return invalid("note.chunk_max_duration", "必须大于 0")
	}
	if s.Media.PlaybackMaxHeight < 0 {
		return invalid("media.playback_max_height", "不能为负数")
	}
	if s.Network.Proxy != "" {
		if err := checkURL(s.Network.Proxy, "http", "https", "socks5", "socks5h"); err != nil {
			return invalid("network.proxy", "%v", err)
		}
	}
	for host, target := range s.Download.Mirrors {
		key := "download.mirrors." + host
		if host == "" || strings.ContainsAny(host, "/:@? \t") {
			return invalid(key, "键必须是主机名，例如 huggingface.co")
		}
		if err := checkURL(target, "http", "https"); err != nil {
			return invalid(key, "%v", err)
		}
	}
	for i, r := range s.Security.ImportRoots {
		if _, err := expandRoot(r); err != nil {
			return invalid(fmt.Sprintf("security.import_roots[%d]", i), "必须是绝对路径或以 ~ 开头")
		}
	}
	if n, err := ParseSize(s.Security.MaxUploadSize); err != nil {
		return invalid("security.max_upload_size", "%v", err)
	} else if n <= 0 {
		return invalid("security.max_upload_size", "必须大于 0")
	}
	return nil
}
