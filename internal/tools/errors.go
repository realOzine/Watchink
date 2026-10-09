package tools

import (
	"context"
	"errors"
	"syscall"

	"watchink/internal/app"
)

// ioErr converts a local filesystem error into an *app.Error: DISK_FULL when
// the volume is out of space, INTERNAL otherwise.
func ioErr(err error, what string) error {
	if err == nil {
		return nil
	}
	if isCtxErr(err) {
		return err
	}
	var ae *app.Error
	if errors.As(err, &ae) {
		return err
	}
	// TODO(windows): ERROR_DISK_FULL / ERROR_HANDLE_DISK_FULL are not mapped.
	if errors.Is(err, syscall.ENOSPC) {
		return app.E(app.CodeDiskFull, "磁盘空间不足：%s", what).
			WithHint("请清理磁盘空间后重试。").WithCause(err)
	}
	return app.E(app.CodeInternal, "%s失败：%v", what, err).WithCause(err)
}

func isCtxErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func errUnknownItem(name string) error {
	return app.E(app.CodeInvalidArgument, "未知的工具或模型：%s", name)
}
