# 从源码构建

普通用户不需要构建：下载发行文件即可，见 [README](../README.md#安装)。本文面向想修改代码或自己打包的人。

三个平台都需要 Go 1.27 和 Node 24（自带 corepack）。各平台的 CI 工作流（`.github/workflows/`）就是完整、可运行的构建步骤，本文与它们保持一致。

## macOS（arm64）

需要 Go 1.27、Node 24（自带 corepack）和 Xcode Command Line Tools。CMake 没装时构建脚本会下载到项目内的缓存目录。

```sh
make whisper        # 构建内嵌的 whisper-cli（whisper.cpp，Metal）
make frontend       # 构建前端到 internal/webui/dist
make build          # 无界面的开发二进制 bin/watchink（纯 Go）
make desktop        # dist/Watchink.app（ad-hoc 签名）
make package-macos  # 发行 zip 和 SHA256SUMS
make test lint
```

## Linux（amd64，Ubuntu 24.04）

构建需要 Go 1.27、Node 24 和以下系统包：

```sh
sudo apt-get install build-essential pkg-config cmake libgtk-3-dev libwebkit2gtk-4.1-dev
```

```sh
make whisper-linux   # 构建内嵌的 whisper-cli（静态链接，仅 CPU，需要支持 AVX2 的处理器）
make frontend
make desktop-linux   # dist/watchink-<版本>-linux-amd64：单个 ELF，包含桌面窗口和全部子命令
make package-linux   # 再加上 dist/watchink-headless-<版本>-linux-amd64 和 SHA256SUMS
make test lint-linux
scripts/smoke-linux.sh dist/watchink-<版本>-linux-amd64 --gui   # 无图形会话下的冒烟检查
scripts/e2e.sh dist/watchink-<版本>-linux-amd64           # 真实工具加伪造在线模型的全流程
```

运行 `watchink-<版本>-linux-amd64` 需要以下系统包。它动态链接 GTK 和 WebKitGTK，所以即使只用 `serve`、`mcp` 或命令行，前两个包也必须装上；`watchink doctor` 会检查这四项：

```sh
sudo apt-get install libwebkit2gtk-4.1-0 libgtk-3-0t64 gstreamer1.0-plugins-good gstreamer1.0-libav
```

- 后两个包提供窗口内播放视频所需的 H.264 / AAC 解码。缺少时其余功能不受影响，也可以用 `watchink web` 在浏览器里播放。
- 没有 WebKitGTK 的机器（服务器、容器）请使用 `watchink-headless-<版本>-linux-amd64`：纯 Go 静态链接，没有任何系统依赖，除了桌面窗口以外功能相同。
- 没有图形会话（例如 SSH 登录）时，`serve`、`web`、`mcp` 和命令行照常可用，只有打开窗口会报错。
- `watchink web` 用 `xdg-open` 打开浏览器（`xdg-utils` 包）。
- 数据目录默认在 `${XDG_DATA_HOME:-~/.local/share}/watchink`，凭据在 `${XDG_CONFIG_HOME:-~/.config}/watchink`。
- 可选的桌面集成（应用菜单里的图标）：把可执行文件以 `watchink` 为名放进 `PATH`，然后

  ```sh
  install -Dm644 build/linux/watchink.desktop ~/.local/share/applications/watchink.desktop
  install -Dm644 build/linux/watchink.png ~/.local/share/icons/hicolor/256x256/apps/watchink.png
  ```

## Windows（amd64）

发行物是一个 zip，里面有两个由同一份代码构建的程序：

| 文件 | 用途 |
| --- | --- |
| `Watchink.exe` | 桌面应用（GUI 子系统），双击打开窗口，不出现控制台 |
| `watchink.com` | 命令行程序（控制台子系统），在 cmd、PowerShell 和 MCP 客户端的配置里使用 |

两个文件放在同一个文件夹里；把该文件夹加入 `PATH` 后，输入 `watchink` 运行的是 `watchink.com`。桌面窗口需要 WebView2 运行时（Windows 11 自带，缺少时会提示安装）。数据目录默认在 `%LOCALAPPDATA%\Watchink\Data`，凭据在 `%APPDATA%\Watchink\secrets.toml`。默认只允许导入用户文件夹内的本地视频，其它位置用 `watchink config set security.import_roots "~,D:\videos"` 加入。

在 Windows 上构建（Git Bash；需要 Go 1.27、Node 24、带 C++ 工具的 Visual Studio、CMake，以及提供 `windres` 的 MinGW-w64）：

```sh
make whisper-windows   # 内嵌的 whisper-cli.exe（MSVC，静态运行时，仅 CPU）
make package-windows   # dist/Watchink-<版本>-windows-amd64.zip 和 SHA256SUMS
```

Windows 的桌面构建不需要 cgo，所以在 macOS 或 Linux 上也能交叉编译，用来检查代码能否通过编译；这样得到的程序没有内嵌 whisper-cli，没有 `windres` 时也没有图标和清单：

```sh
make lint-windows      # GOOS=windows 的 go vet（nogui 和 desktop 两种标签）
make desktop-windows   # dist/Watchink/{Watchink.exe, watchink.com}
```

## 开发时常用

- 数据目录默认在 `~/Library/Application Support/Watchink/Data`，可用 `--data-dir` 或 `WATCHINK_DATA_DIR` 指定。
- 没有 API Key 时可以用伪造的在线模型跑通全流程：

  ```sh
  go run ./internal/capability/llm/llmtest/cmd/fakellm -addr 127.0.0.1:18080
  watchink config set llm.base_url http://127.0.0.1:18080/v1
  watchink config set llm.model fake-model
  ```

- 前端开发：`cd frontend && corepack pnpm dev`（代理到本机 Engine），或 `corepack pnpm dev:mock`（不需要 Engine）。
- 代码结构见 [`architecture.md`](architecture.md)，性能基准见 [`benchmark.md`](benchmark.md)。
