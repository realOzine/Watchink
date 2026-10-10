package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"watchink/internal/app"
)

// EnvLLMAPIKey has the highest priority among the API key sources (D-14).
const EnvLLMAPIKey = "WATCHINK_LLM_API_KEY"

// SecretsFileName is the secrets file inside the user config dir.
const SecretsFileName = "secrets.toml"

// API key sources reported in Settings.LLM.APIKeySource. A key taken from the
// variable named by llm.api_key_env is reported as "env:<NAME>".
const (
	KeySourceEnv  = "env"
	KeySourceFile = "file"
)

// Secrets is the content of <cfg>/secrets.toml. It lives outside the data
// dir so that it never ends up in a backup or an export (D-14).
type Secrets struct {
	LLMAPIKey string
}

type secretsFile struct {
	LLM struct {
		APIKey string `toml:"api_key"`
	} `toml:"llm"`
}

// SecretsFile returns <cfg>/secrets.toml.
func SecretsFile() (string, error) {
	cfg, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cfg, SecretsFileName), nil
}

// checkSecretsPath refuses anything but a regular file (or nothing) at p:
// following a symlink could read or overwrite a file outside <cfg>.
func checkSecretsPath(p string) (exists bool, err error) {
	fi, err := os.Lstat(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, app.E(app.CodeInternal, "无法访问凭据文件 %s：%v", p, err).WithCause(err)
	}
	if !fi.Mode().IsRegular() {
		return false, app.E(app.CodeInternal, "凭据文件 %s 不是普通文件（可能是符号链接），已拒绝使用", p).
			WithHint("请删除该路径后重新设置 API Key")
	}
	return true, nil
}

// LoadSecrets reads <cfg>/secrets.toml. A missing file yields empty Secrets.
func LoadSecrets() (Secrets, error) {
	p, err := SecretsFile()
	if err != nil {
		return Secrets{}, app.E(app.CodeInternal, "无法确定用户配置目录：%v", err).WithCause(err)
	}
	exists, err := checkSecretsPath(p)
	if err != nil || !exists {
		return Secrets{}, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return Secrets{}, app.E(app.CodeInternal, "无法读取凭据文件 %s：%v", p, err).WithCause(err)
	}
	var f secretsFile
	if err := toml.Unmarshal(b, &f); err != nil {
		// The decode error may quote file content, so it is not included.
		return Secrets{}, app.E(app.CodeInvalidArgument, "凭据文件 %s 不是合法的 TOML", p).
			WithHint("请删除该文件后重新设置 API Key")
	}
	return Secrets{LLMAPIKey: strings.TrimSpace(f.LLM.APIKey)}, nil
}

// SetSecret stores a write-only secret in <cfg>/secrets.toml (mode 0600,
// directory 0700). Only app.SecretLLMAPIKey is accepted; an empty value
// deletes the secret, and the file once it holds nothing.
func SetSecret(name, value string) error {
	if name != app.SecretLLMAPIKey {
		return app.E(app.CodeInvalidArgument, "未知的凭据名称 %q，只支持 %s", name, app.SecretLLMAPIKey).
			WithDetail("name", name)
	}
	value = strings.TrimSpace(value)
	if strings.ContainsFunc(value, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return app.E(app.CodeInvalidArgument, "API Key 中不能包含换行或控制字符")
	}
	p, err := SecretsFile()
	if err != nil {
		return app.E(app.CodeInternal, "无法确定用户配置目录：%v", err).WithCause(err)
	}
	exists, err := checkSecretsPath(p)
	if err != nil {
		return err
	}
	if value == "" {
		if exists {
			if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return app.E(app.CodeInternal, "无法删除凭据文件：%v", err).WithCause(err)
			}
		}
		return nil
	}
	var f secretsFile
	f.LLM.APIKey = value
	b, err := toml.Marshal(f)
	if err != nil {
		return app.E(app.CodeInternal, "无法序列化凭据").WithCause(err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return app.E(app.CodeInternal, "无法创建用户配置目录：%v", err).WithCause(err)
	}
	// The rename replaces the directory entry itself, so even a symlink
	// planted after the check above is replaced rather than followed.
	if err := writeFileAtomic(p, b, 0o600); err != nil {
		return app.E(app.CodeInternal, "无法写入凭据文件：%v", err).WithCause(err)
	}
	return nil
}

// ResolveLLMAPIKey returns the API key and where it came from, in the D-14
// priority order: WATCHINK_LLM_API_KEY ("env"), then the variable named
// by llm.api_key_env ("env:<NAME>"), then secrets.toml ("file"). Both results
// are empty when no key is configured.
func ResolveLLMAPIKey(s app.Settings) (key, source string) {
	if v := strings.TrimSpace(os.Getenv(EnvLLMAPIKey)); v != "" {
		return v, KeySourceEnv
	}
	if name := s.LLM.APIKeyEnv; name != "" {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return v, KeySourceEnv + ":" + name
		}
	}
	if sec, err := LoadSecrets(); err == nil && sec.LLMAPIKey != "" {
		return sec.LLMAPIKey, KeySourceFile
	}
	return "", ""
}

// last4MinKeyLen is the shortest key whose last 4 characters are shown;
// for shorter keys the tail would reveal too large a share of the secret.
const last4MinKeyLen = 12

// Describe fills the read-only fields of s: the credential state
// (api_key_configured / api_key_last4 / api_key_source, D-14: never the key
// itself), config_dir, and data_dir when it is still empty (Load sets it).
func Describe(s *app.Settings) {
	key, source := ResolveLLMAPIKey(*s)
	s.LLM.APIKeyConfigured = key != ""
	s.LLM.APIKeySource = source
	s.LLM.APIKeyLast4 = ""
	if r := []rune(key); len(r) >= last4MinKeyLen {
		s.LLM.APIKeyLast4 = string(r[len(r)-4:])
	}
	if cfg, err := ConfigDir(); err == nil {
		s.ConfigDir = cfg
	}
	if s.DataDir == "" {
		if d, err := ResolveDataDir(""); err == nil {
			s.DataDir = d
		}
	}
}
