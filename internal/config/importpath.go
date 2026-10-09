package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"watchink/internal/app"
)

// expandRoot expands a leading ~ and requires the result to be absolute.
func expandRoot(root string) (string, error) {
	r := strings.TrimSpace(root)
	if r == "" {
		return "", errors.New("empty import root")
	}
	if r == "~" || strings.HasPrefix(r, "~/") || strings.HasPrefix(r, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		r = filepath.Join(home, r[1:])
	}
	if !filepath.IsAbs(r) {
		return "", errors.New("import root must be absolute or start with ~")
	}
	return filepath.Clean(r), nil
}

// ExpandRoots turns security.import_roots into absolute, symlink-resolved
// directories (§15). A root that does not exist is skipped: nothing can be
// inside it. A relative or empty entry is INVALID_ARGUMENT.
func ExpandRoots(roots []string) ([]string, error) {
	out := make([]string, 0, len(roots))
	for _, root := range roots {
		r, err := expandRoot(root)
		if err != nil {
			return nil, app.E(app.CodeInvalidArgument, "导入根目录 %q 无效：必须是绝对路径或以 ~ 开头", root).
				WithDetail("key", "security.import_roots")
		}
		resolved, err := filepath.EvalSymlinks(r)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, app.E(app.CodeInternal, "无法解析导入根目录 %s：%v", r, err).WithCause(err)
		}
		out = append(out, resolved)
	}
	return out, nil
}

// within reports whether path equals root or lies below it, comparing whole
// path components ("/a/bc" is not inside "/a/b").
func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil || filepath.IsAbs(rel) {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// CheckImportPath enforces the local import policy of §15: path must be
// absolute (INVALID_ARGUMENT), exist (NOT_FOUND), be a regular file
// (INVALID_ARGUMENT) and, after resolving symlinks, lie inside one of roots
// (PATH_NOT_ALLOWED). roots may be the raw security.import_roots; they are
// expanded with ExpandRoots. It returns the symlink-resolved path, which is
// the one the caller must open.
func CheckImportPath(path string, roots []string) (resolved string, err error) {
	if path == "" {
		return "", app.E(app.CodeInvalidArgument, "缺少文件路径")
	}
	if !filepath.IsAbs(path) {
		return "", app.E(app.CodeInvalidArgument, "文件路径必须是绝对路径：%s", path)
	}
	expanded, err := ExpandRoots(roots)
	if err != nil {
		return "", err
	}
	resolved, err = filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return "", app.E(app.CodeNotFound, "文件不存在：%s", path)
		case errors.Is(err, fs.ErrPermission):
			return "", app.E(app.CodePathNotAllowed, "没有权限访问该文件：%s", path).
				WithHint("请检查文件权限，或在系统设置中允许 Watchink 访问该位置")
		}
		return "", app.E(app.CodeInvalidArgument, "无法解析文件路径 %s：%v", path, err).WithCause(err)
	}
	allowed := false
	for _, root := range expanded {
		if within(root, resolved) {
			allowed = true
			break
		}
	}
	if !allowed {
		return "", app.E(app.CodePathNotAllowed, "该文件不在允许导入的目录内：%s", path).
			WithHint("可在设置 security.import_roots 中添加该文件所在的目录").
			WithDetail("import_roots", roots)
	}
	fi, err := os.Stat(resolved)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", app.E(app.CodeNotFound, "文件不存在：%s", path)
		}
		return "", app.E(app.CodeInvalidArgument, "无法读取文件信息 %s：%v", path, err).WithCause(err)
	}
	if !fi.Mode().IsRegular() {
		return "", app.E(app.CodeInvalidArgument, "该路径不是普通文件：%s", path)
	}
	return resolved, nil
}
