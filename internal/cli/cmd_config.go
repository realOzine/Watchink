package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"watchink/internal/app"
)

// settingsMap renders Settings the way the API does, for dotted-key access.
func settingsMap(s *app.Settings) (map[string]any, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	return m, nil
}

// lookupKey resolves a dotted key. Map keys may themselves contain dots
// (download.mirrors.huggingface.co), so every split point is tried.
func lookupKey(node any, parts []string) (any, bool) {
	if len(parts) == 0 {
		return node, true
	}
	m, ok := node.(map[string]any)
	if !ok {
		return nil, false
	}
	for i := 1; i <= len(parts); i++ {
		if child, ok := m[strings.Join(parts[:i], ".")]; ok {
			if v, ok := lookupKey(child, parts[i:]); ok {
				return v, true
			}
		}
	}
	return nil, false
}

// flatten turns nested settings into sorted "a.b = value" lines.
func flatten(prefix string, node any, out map[string]string) {
	if m, ok := node.(map[string]any); ok && (len(m) > 0 || prefix == "") {
		for k, v := range m {
			key := k
			if prefix != "" {
				key = prefix + "." + k
			}
			flatten(key, v, out)
		}
		return
	}
	out[prefix] = scalarString(node)
}

func scalarString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case nil:
		return ""
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

func jsonFieldName(f reflect.StructField) string {
	name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
	return name
}

// settingsPatch maps `config set <dotted key> <value>` to the nested JSON
// merge patch of PATCH /settings. The value type is inferred from the
// Settings field the key addresses: bool / integer / number / string /
// string list / string map entry. Error messages never repeat the value.
func settingsPatch(key, value string) (map[string]any, error) {
	unknown := func() (map[string]any, error) {
		return nil, app.E(app.CodeInvalidArgument, "未知的配置项 %q", key).
			WithHint("运行 `watchink config show` 查看全部配置项")
	}
	badValue := func(want string) (map[string]any, error) {
		return nil, app.E(app.CodeInvalidArgument, "配置项 %s 的值无效：需要%s", key, want)
	}
	parts := strings.Split(key, ".")
	t := reflect.TypeFor[app.Settings]()
	patch := map[string]any{}
	cur := patch
	for i := 0; i < len(parts); i++ {
		part := parts[i]
		if part == "" || t.Kind() != reflect.Struct {
			return unknown()
		}
		var field reflect.StructField
		found := false
		for j := 0; j < t.NumField(); j++ {
			if f := t.Field(j); jsonFieldName(f) == part {
				field, found = f, true
				break
			}
		}
		if !found {
			return unknown()
		}
		if field.Tag.Get("toml") == "-" {
			// Engine-filled fields (data_dir, api_key_* state) and secrets.
			return nil, app.E(app.CodeInvalidArgument, "配置项 %s 是只读的", key).
				WithHint("API Key 用 `watchink config set-secret llm_api_key` 设置；数据目录用 `watchink config set-data-dir`")
		}
		last := i == len(parts)-1
		ft := field.Type
		switch ft.Kind() {
		case reflect.Struct:
			if last {
				return nil, app.E(app.CodeInvalidArgument, "%s 是一个配置分组，请指定具体的配置项，如 %s.<名称>", key, key)
			}
			next := map[string]any{}
			cur[part] = next
			cur, t = next, ft
			continue
		case reflect.Map:
			if last {
				var m map[string]string
				if err := json.Unmarshal([]byte(value), &m); err != nil {
					return badValue(" JSON 对象，或改用 " + key + ".<键> <值> 设置单项")
				}
				obj := map[string]any{}
				for k, v := range m {
					obj[k] = v
				}
				cur[part] = obj
				return patch, nil
			}
			// The rest of the key is the map key (it may contain dots).
			// An empty value deletes the entry (JSON merge patch null).
			var v any = value
			if value == "" {
				v = nil
			}
			cur[part] = map[string]any{strings.Join(parts[i+1:], "."): v}
			return patch, nil
		}
		if !last {
			return unknown()
		}
		switch ft.Kind() {
		case reflect.String:
			cur[part] = value
		case reflect.Bool:
			b, err := parseBool(value)
			if err != nil {
				return badValue(" true 或 false")
			}
			cur[part] = b
		case reflect.Int, reflect.Int64:
			n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
			if err != nil {
				return badValue("整数")
			}
			cur[part] = n
		case reflect.Float64:
			f, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
			if err != nil {
				return badValue("数字")
			}
			cur[part] = f
		case reflect.Slice:
			list := []string{}
			if trimmed := strings.TrimSpace(value); strings.HasPrefix(trimmed, "[") {
				if err := json.Unmarshal([]byte(trimmed), &list); err != nil {
					return badValue(" JSON 字符串数组，或用逗号分隔的列表")
				}
			} else {
				for _, item := range strings.Split(value, ",") {
					if item = strings.TrimSpace(item); item != "" {
						list = append(list, item)
					}
				}
			}
			cur[part] = list
		default:
			return unknown()
		}
		return patch, nil
	}
	return unknown()
}

func parseBool(s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true", "1", "yes", "on":
		return true, nil
	case "false", "0", "no", "off":
		return false, nil
	}
	return false, strconv.ErrSyntax
}

// stdin returns the input stream for secrets and prompts.
func (c *call) stdin() io.Reader {
	if c.deps.Stdin != nil {
		return c.deps.Stdin
	}
	return c.cmd.InOrStdin()
}

func (c *call) isTerminal() bool {
	return c.deps.IsTerminal != nil && c.deps.IsTerminal()
}

// maxSecretBytes bounds what is read from stdin for a secret.
const maxSecretBytes = 64 << 10

// readSecret reads a secret: without echo from a terminal when possible,
// otherwise everything piped on stdin. It is never written anywhere.
func (c *call) readSecret(prompt string) (string, error) {
	if c.isTerminal() {
		fmt.Fprint(c.errw, prompt)
		if c.deps.ReadPassword != nil {
			s, err := c.deps.ReadPassword()
			fmt.Fprintln(c.errw)
			if err != nil {
				return "", app.E(app.CodeInternal, "读取输入失败：%v", err)
			}
			return strings.TrimSpace(s), nil
		}
		line, err := bufio.NewReader(c.stdin()).ReadString('\n')
		if err != nil && line == "" {
			return "", app.E(app.CodeInvalidArgument, "没有读到输入")
		}
		return strings.TrimSpace(line), nil
	}
	b, err := io.ReadAll(io.LimitReader(c.stdin(), maxSecretBytes+1))
	if err != nil {
		return "", app.E(app.CodeInternal, "读取 stdin 失败：%v", err)
	}
	if len(b) > maxSecretBytes {
		return "", app.E(app.CodeInvalidArgument, "stdin 内容过长，不像是一个密钥")
	}
	return strings.TrimSpace(string(b)), nil
}

// secretResult is the --json output of `config set-secret`; it reports the
// credential state only, never the value.
type secretResult struct {
	Name       string `json:"name"`
	Configured bool   `json:"configured"`
	Source     string `json:"source,omitempty"`
	Last4      string `json:"last4,omitempty"`
}

func (e *env) cmdConfig() *cobra.Command {
	show := &cobra.Command{
		Use:   "show",
		Short: "显示全部配置（不含凭据）",
		Args:  noArgs,
		RunE: e.run(func(c *call) error {
			return c.withRemote(func(r app.Remote) error {
				ctx, cancel := c.reqCtx()
				defer cancel()
				s, err := r.GetSettings(ctx)
				if err != nil {
					return err
				}
				return c.emit(s, func(w io.Writer) { printSettings(w, s) })
			})
		}),
	}

	get := &cobra.Command{
		Use:   "get <key>",
		Short: "读取一个配置项，如 llm.model",
		Args:  exactArgs(1, "config get <key>"),
		RunE: e.run(func(c *call) error {
			return c.withRemote(func(r app.Remote) error {
				ctx, cancel := c.reqCtx()
				defer cancel()
				s, err := r.GetSettings(ctx)
				if err != nil {
					return err
				}
				m, err := settingsMap(s)
				if err != nil {
					return err
				}
				v, ok := lookupKey(m, strings.Split(c.args[0], "."))
				if !ok {
					return app.E(app.CodeNotFound, "没有配置项 %q", c.args[0]).
						WithHint("运行 `watchink config show` 查看全部配置项")
				}
				return c.emit(map[string]any{"key": c.args[0], "value": v}, func(w io.Writer) {
					if _, nested := v.(map[string]any); nested {
						lines := map[string]string{}
						flatten(c.args[0], v, lines)
						printSorted(w, lines)
						return
					}
					fmt.Fprintln(w, scalarString(v))
				})
			})
		}),
	}

	set := &cobra.Command{
		Use:   "set <key> <value>",
		Short: "修改一个配置项，如 `config set llm.model gpt-4o-mini`",
		Args:  exactArgs(2, "config set <key> <value>"),
		RunE: e.run(func(c *call) error {
			patch, err := settingsPatch(c.args[0], c.args[1])
			if err != nil {
				return err
			}
			return c.withRemote(func(r app.Remote) error {
				ctx, cancel := c.reqCtx()
				defer cancel()
				s, err := r.PatchSettings(ctx, patch)
				if err != nil {
					return err
				}
				return c.emit(s, func(w io.Writer) {
					fmt.Fprintf(w, "已更新 %s\n", c.args[0])
				})
			})
		}),
	}

	setDataDir := &cobra.Command{
		Use:   "set-data-dir <路径>",
		Short: "更改数据目录（写入 location.toml；需先停止 Engine，不迁移已有数据）",
		Args:  exactArgs(1, "config set-data-dir <路径>"),
		RunE: e.run(func(c *call) error {
			if c.deps.SetDataDir == nil {
				return app.E(app.CodeInternal, "此构建不支持更改数据目录")
			}
			if c.args[0] == "" {
				return app.E(app.CodeInvalidArgument, "数据目录不能为空")
			}
			target, err := filepath.Abs(c.args[0])
			if err != nil {
				return app.E(app.CodeInvalidArgument, "无法解析路径：%v", err)
			}
			current, err := c.resolveDataDir()
			if err != nil {
				return err
			}
			// the engine must be stopped first, otherwise it would
			// keep serving the old directory.
			if c.deps.IsEngineRunning != nil && c.deps.IsEngineRunning(current) {
				return app.E(app.CodeConflict, "Engine 正在运行，不能更改数据目录").
					WithHint("先运行 `watchink engine stop`，再重试").
					WithDetail("data_dir", current)
			}
			if err := c.deps.SetDataDir(target); err != nil {
				return err
			}
			res := map[string]any{"data_dir": target, "previous_data_dir": current}
			return c.emit(res, func(w io.Writer) {
				fmt.Fprintf(w, "数据目录已改为 %s\n", target)
				if current != "" && current != target {
					fmt.Fprintf(w, "原目录 %s 中的数据不会自动迁移。\n", current)
				}
			})
		}),
	}

	var del bool
	setSecret := &cobra.Command{
		Use:   "set-secret llm_api_key",
		Short: "从 stdin 读取并保存凭据（不回显）；--delete 删除",
		Long: "从 stdin 读取凭据并保存，例如：\n" +
			"  printf %s \"$KEY\" | watchink config set-secret llm_api_key\n" +
			"凭据只写不读：任何命令都不会输出它。",
		Args: exactArgs(1, "config set-secret llm_api_key"),
		RunE: e.run(func(c *call) error {
			name := c.args[0]
			if name != app.SecretLLMAPIKey {
				return app.E(app.CodeInvalidArgument, "未知的凭据名 %q", name).
					WithHint("目前只支持 " + app.SecretLLMAPIKey)
			}
			value := ""
			if !del {
				v, err := c.readSecret("API Key：")
				if err != nil {
					return err
				}
				if v == "" {
					return app.E(app.CodeInvalidArgument, "没有从 stdin 读到凭据").
						WithHint("通过管道传入凭据；要删除已保存的凭据请加 --delete")
				}
				value = v
			}
			return c.withRemote(func(r app.Remote) error {
				ctx, cancel := c.reqCtx()
				defer cancel()
				if err := r.SetSecret(ctx, name, value); err != nil {
					return err
				}
				res := secretResult{Name: name, Configured: !del}
				if s, err := r.GetSettings(ctx); err == nil {
					res.Configured = s.LLM.APIKeyConfigured
					res.Source, res.Last4 = s.LLM.APIKeySource, s.LLM.APIKeyLast4
				}
				return c.emit(res, func(w io.Writer) {
					if del {
						fmt.Fprintf(w, "已删除凭据 %s\n", name)
					} else {
						fmt.Fprintf(w, "已保存凭据 %s\n", name)
					}
				})
			})
		}),
	}
	setSecret.Flags().BoolVar(&del, "delete", false, "删除已保存的凭据，不读取 stdin")

	return group("config", "配置管理", show, get, set, setDataDir, setSecret)
}

func printSorted(w io.Writer, lines map[string]string) {
	keys := make([]string, 0, len(lines))
	for k := range lines {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	pairs := make([][2]string, 0, len(keys))
	for _, k := range keys {
		pairs = append(pairs, [2]string{k, "= " + lines[k]})
	}
	kv(w, pairs...)
}

func printSettings(w io.Writer, s *app.Settings) {
	m, err := settingsMap(s)
	if err != nil {
		fmt.Fprintln(w, err)
		return
	}
	lines := map[string]string{}
	flatten("", m, lines)
	printSorted(w, lines)
}
