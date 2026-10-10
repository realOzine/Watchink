package config

import (
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"

	"watchink/internal/app"
)

// ApplyPatch applies a JSON merge patch shaped like the Settings JSON to cur
// and validates the result. The patch is typically a decoded JSON body, e.g.
//
//	{"llm": {"model": "x"}, "download": {"mirrors": {"huggingface.co": "https://hf-mirror.com"}}}
//
// Rules: objects merge recursively; a scalar or array replaces the value; in
// a map (download.mirrors) a null value deletes that entry; null for any
// other key resets it to its default. Unknown keys, read-only keys (data_dir,
// config_dir, llm.api_key_configured / _last4 / _source) and values of the
// wrong type are INVALID_ARGUMENT with the dotted key in the message and in
// details.key. cur is not modified.
func ApplyPatch(cur app.Settings, patch map[string]any) (app.Settings, error) {
	out := cur
	// Deep-copy the reference-typed fields so cur stays untouched.
	out.Download.Mirrors = make(map[string]string, len(cur.Download.Mirrors))
	for k, v := range cur.Download.Mirrors {
		out.Download.Mirrors[k] = v
	}
	out.Security.ImportRoots = append([]string{}, cur.Security.ImportRoots...)

	def := app.DefaultSettings()
	if err := patchStruct(reflect.ValueOf(&out).Elem(), reflect.ValueOf(def), patch, ""); err != nil {
		return cur, err
	}
	normalize(&out)
	if err := Validate(out); err != nil {
		return cur, err
	}
	return out, nil
}

func patchErr(key, format string, args ...any) *app.Error {
	return app.E(app.CodeInvalidArgument, "设置项 %s：%s", key, fmt.Sprintf(format, args...)).
		WithDetail("key", key)
}

// jsonName returns the JSON key of a struct field and whether the field is
// read-only. Read-only fields are exactly those excluded from config.toml.
func jsonName(f reflect.StructField) (name string, readOnly bool) {
	name, _, _ = strings.Cut(f.Tag.Get("json"), ",")
	return name, f.Tag.Get("toml") == "-"
}

func patchStruct(dst, def reflect.Value, patch map[string]any, prefix string) error {
	fields := map[string]int{}
	for i := 0; i < dst.NumField(); i++ {
		if name, _ := jsonName(dst.Type().Field(i)); name != "" && name != "-" {
			fields[name] = i
		}
	}
	// Sorted so that the reported key is deterministic when several are bad.
	keys := make([]string, 0, len(patch))
	for k := range patch {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		full := prefix + k
		i, ok := fields[k]
		if !ok {
			return patchErr(full, "未知的设置项")
		}
		if _, ro := jsonName(dst.Type().Field(i)); ro {
			return patchErr(full, "只读，不能修改")
		}
		if err := patchValue(dst.Field(i), def.Field(i), patch[k], full); err != nil {
			return err
		}
	}
	return nil
}

func patchValue(dst, def reflect.Value, v any, key string) error {
	if v == nil {
		// Merge-patch "remove": fall back to the default value.
		dst.Set(def)
		return nil
	}
	switch dst.Kind() {
	case reflect.Struct:
		m, ok := v.(map[string]any)
		if !ok {
			return patchErr(key, "应为对象")
		}
		return patchStruct(dst, def, m, key+".")
	case reflect.String:
		s, ok := v.(string)
		if !ok {
			return patchErr(key, "应为字符串")
		}
		dst.SetString(s)
	case reflect.Bool:
		b, ok := v.(bool)
		if !ok {
			return patchErr(key, "应为布尔值")
		}
		dst.SetBool(b)
	case reflect.Int, reflect.Int64:
		f, ok := toFloat(v)
		if !ok || f != math.Trunc(f) || math.Abs(f) > 1<<53 {
			return patchErr(key, "应为整数")
		}
		dst.SetInt(int64(f))
	case reflect.Float64:
		f, ok := toFloat(v)
		if !ok {
			return patchErr(key, "应为数字")
		}
		dst.SetFloat(f)
	case reflect.Slice: // []string
		var out []string
		switch a := v.(type) {
		case []string:
			out = append([]string{}, a...)
		case []any:
			out = make([]string, 0, len(a))
			for i, e := range a {
				s, ok := e.(string)
				if !ok {
					return patchErr(fmt.Sprintf("%s[%d]", key, i), "应为字符串")
				}
				out = append(out, s)
			}
		default:
			return patchErr(key, "应为字符串数组")
		}
		dst.Set(reflect.ValueOf(out))
	case reflect.Map: // map[string]string
		cur, _ := dst.Interface().(map[string]string)
		if cur == nil {
			cur = map[string]string{}
		}
		switch m := v.(type) {
		case map[string]string:
			for k, s := range m {
				cur[k] = s
			}
		case map[string]any:
			for k, e := range m {
				if e == nil {
					delete(cur, k)
					continue
				}
				s, ok := e.(string)
				if !ok {
					return patchErr(key+"."+k, "应为字符串")
				}
				cur[k] = s
			}
		default:
			return patchErr(key, "应为对象")
		}
		dst.Set(reflect.ValueOf(cur))
	default:
		return patchErr(key, "不支持的类型")
	}
	return nil
}

// toFloat accepts every numeric type: a decoded JSON body carries float64,
// an in-process caller may pass ints.
func toFloat(v any) (float64, bool) {
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return float64(rv.Int()), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return float64(rv.Uint()), true
	case reflect.Float32, reflect.Float64:
		f := rv.Float()
		return f, !math.IsNaN(f) && !math.IsInf(f, 0)
	}
	// json.Number (Decoder.UseNumber) is a string kind with a Float64 method.
	if n, ok := v.(interface{ Float64() (float64, error) }); ok {
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}
