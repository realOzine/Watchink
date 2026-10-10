//go:build desktop

package desktop

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/wailsapp/wails/v2/pkg/options/windows"
)

// ConsoleExecutableName is the console-subsystem twin shipped next to the
// GUI executable on Windows: a single executable cannot both open without a
// console window and behave as a normal console program. Both are built from the same code; this one
// is what a terminal or an MCP client should start.
const ConsoleExecutableName = "watchink.com"

// windowsOptions are the Wails options that only apply on Windows; other
// platforms ignore them. (This file is not named *_windows.go on purpose: its
// helpers are compiled and unit-tested on every platform.)
func windowsOptions() *windows.Options {
	return &windows.Options{
		WebviewUserDataPath: webviewUserDataPath(),
		// Shown when the WebView2 runtime is missing or too old. With
		// the default Wails strategy the user confirms and Microsoft's
		// bootstrapper is downloaded and run; nothing is installed silently.
		Messages: &windows.Messages{
			InstallationRequired: "Watchink 需要 Microsoft Edge WebView2 运行时才能显示界面。点击“确定”下载并安装（安装程序在后台下载，请稍候）。",
			UpdateRequired:       "当前的 WebView2 运行时版本过旧。点击“确定”下载并安装新版本（安装程序在后台下载，请稍候）。",
			MissingRequirements:  "缺少运行组件",
			Webview2NotInstalled: "未安装 WebView2 运行时",
			Error:                "错误",
			FailedToInstall:      "WebView2 运行时安装失败。请从 https://developer.microsoft.com/microsoft-edge/webview2/ 下载“常青版”安装程序，安装后重新打开 Watchink。",
			DownloadPage:         "Watchink 需要 WebView2 运行时。点击“确定”打开下载页面。最低版本：",
			PressOKToInstall:     "点击“确定”开始安装。",
			ContactAdmin:         "Watchink 需要 WebView2 运行时，请联系系统管理员安装。",
			InvalidFixedWebview2: "指定的 WebView2 运行时无效，请检查路径和最低版本要求。",
			WebView2ProcessCrash: "WebView2 进程意外退出，请重新打开 Watchink。",
		},
	}
}

// webviewUserDataPath keeps the WebView2 profile (cache, local storage) in
// %LOCALAPPDATA%\Watchink\WebView2, next to the default data directory,
// instead of Wails' default of a roaming folder named after the
// executable. Empty selects that default.
func webviewUserDataPath() string {
	if runtime.GOOS != "windows" {
		return ""
	}
	dir, err := os.UserCacheDir() // %LOCALAPPDATA%
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "Watchink", "WebView2")
}

// agentExecutable returns the executable an agent should start for `mcp`.
// On Windows that is the console twin next to the running GUI executable
// when it is there: an MCP client or a shell gets ordinary console semantics
// from it, which a GUI-subsystem program cannot give.
func agentExecutable(exe string) string {
	if runtime.GOOS != "windows" {
		return exe
	}
	return consoleSibling(exe)
}

func consoleSibling(exe string) string {
	sibling := filepath.Join(filepath.Dir(exe), ConsoleExecutableName)
	if st, err := os.Stat(sibling); err == nil && st.Mode().IsRegular() {
		return sibling
	}
	return exe
}

// windowsQuote quotes s for cmd.exe and PowerShell alike when it contains
// anything but plain path characters. A Windows path cannot contain a double
// quote, so wrapping is enough.
func windowsQuote(s string) string {
	safe := s != ""
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune(`\/._-+=:@`, r):
		default:
			safe = false
		}
	}
	if safe {
		return s
	}
	return `"` + s + `"`
}
