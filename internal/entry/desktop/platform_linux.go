//go:build desktop && linux

package desktop

import (
	"os"

	linuxassets "watchink/build/linux"
	"watchink/internal/app"
)

// windowIcon is the icon GTK shows for the window. The release is a single
// ELF without a bundle, so the image is embedded.
func windowIcon() []byte { return linuxassets.Icon }

// preflight reports why no window can be opened in this session. Wails
// panics inside gtk_init when there is no display, which would leave a user
// on an SSH session with a stack trace instead of an explanation.
func preflight() error {
	if os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != "" {
		return nil
	}
	return app.E(app.CodeInvalidArgument, "没有图形会话（未设置 DISPLAY 或 WAYLAND_DISPLAY），无法打开桌面窗口").
		WithHint("运行 `watchink serve` 后用 `watchink web` 或浏览器访问，或使用其他子命令；它们不需要图形会话")
}
