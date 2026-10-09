# Watchink

本地优先的视频知识工具：把课程视频变成带时间引用的中文 Markdown 笔记。桌面、Web、CLI、MCP 四个入口共用同一个后台 Engine 和同一份数据。

当前状态：v0 已在 macOS arm64 上实现并完成开发机验证，Windows 和 Linux 尚未实现。设计见 [`docs/v0.md`](docs/v0.md)，范围与验收见 [`docs/roadmap.md`](docs/roadmap.md)，验证记录见 [`docs/spikes/v0-p0.md`](docs/spikes/v0-p0.md)。

## 构建（macOS arm64）

需要 Go 1.27、Node 24（自带 corepack）和 Xcode Command Line Tools。CMake 没装时构建脚本会下载到项目内的缓存目录。

```sh
make whisper        # 构建内嵌的 whisper-cli（whisper.cpp，Metal）
make frontend       # 构建前端到 internal/webui/dist
make build          # 无界面的开发二进制 bin/watchink（纯 Go）
make desktop        # dist/Watchink.app（ad-hoc 签名）
make package-macos  # 发行 zip 和 SHA256SUMS
make test lint
```

## 使用

```sh
# 首次配置：在线模型（OpenAI 兼容协议），并下载 FFmpeg、yt-dlp 和语音模型
printf '%s' "$API_KEY" | watchink setup \
  --llm-base-url https://api.example.com/v1 --llm-model <模型名> \
  --llm-api-key-stdin --prepare

watchink import ./lecture.mp4 --wait          # 也可以是 YouTube / Bilibili 单视频链接
watchink note get <video_id>
watchink export <video_id> --format md --out ./
watchink web                                  # 开启可选的 Web 界面并在浏览器中打开
watchink mcp                                  # MCP stdio 服务器，供 agent 使用
watchink doctor                               # 诊断
```

桌面应用直接打开 `Watchink.app`；它内部的可执行文件 `Watchink.app/Contents/MacOS/Watchink` 就是上面的 `watchink` 命令。

发送给在线模型的只有字幕文本、视频标题和时长；音频和画面不会离开本机。

## 开发

- 数据目录默认在 `~/Library/Application Support/Watchink/Data`，可用 `--data-dir` 或 `WATCHINK_DATA_DIR` 指定。
- 没有 API Key 时可以用伪造的在线模型跑通全流程：

  ```sh
  go run ./internal/llm/llmtest/cmd/fakellm -addr 127.0.0.1:18080
  watchink config set llm.base_url http://127.0.0.1:18080/v1
  watchink config set llm.model fake-model
  ```

- 前端开发：`cd frontend && corepack pnpm dev`（代理到本机 Engine），或 `corepack pnpm dev:mock`（不需要 Engine）。
- 代码结构和依赖规则见 `docs/v0.md` §4.3。
