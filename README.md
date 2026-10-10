# Watchink

**简体中文** · [English](README.en.md)

[![macOS](https://github.com/realOzine/Watchink/actions/workflows/macos.yml/badge.svg)](https://github.com/realOzine/Watchink/actions/workflows/macos.yml)
[![Linux](https://github.com/realOzine/Watchink/actions/workflows/linux.yml/badge.svg)](https://github.com/realOzine/Watchink/actions/workflows/linux.yml)
[![Windows](https://github.com/realOzine/Watchink/actions/workflows/windows.yml/badge.svg)](https://github.com/realOzine/Watchink/actions/workflows/windows.yml)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

把课程视频变成带时间引用的笔记。笔记里的每个要点都能点回视频里的那一秒。

转写在本机完成，音频和画面不离开你的电脑。下载一个文件就能用。

## 安装

**不需要安装 Go、Node、Python、Docker 或数据库。** 一个平台一个文件，下载后直接运行。FFmpeg、yt-dlp 和语音模型在第一次用到时自动下载并校验。

| 平台 | 下载 | 运行 |
| --- | --- | --- |
| macOS（Apple 芯片） | `Watchink-<版本>-macos-arm64.zip` | 解压，打开 `Watchink.app` |
| Windows（x64） | `Watchink-<版本>-windows-amd64.zip` | 解压，双击 `Watchink.exe` |
| Linux（x64） | `watchink-<版本>-linux-amd64` | `chmod +x` 后运行 |
| Linux 服务器 / 容器 | `watchink-headless-<版本>-linux-amd64` | 同上，无任何系统依赖，没有桌面窗口 |

> **还没有发布正式版本。** 现在可以从 [Actions](https://github.com/realOzine/Watchink/actions) 页面下载最近一次构建的产物（需要登录 GitHub，保留 14 天），或者[从源码构建](docs/building.md)。首个版本发布后会放在 [Releases](https://github.com/realOzine/Watchink/releases)。

各平台需要留意的一件事：

- **macOS**：应用没有 Apple 签名。首次打开被拦截时，到“系统设置 → 隐私与安全性”里点“仍要打开”。
- **Windows**：桌面窗口需要 WebView2（Windows 11 自带，缺少时会提示安装）。SmartScreen 拦截时点“更多信息 → 仍要运行”。
- **Linux**：桌面版需要 WebKitGTK，Ubuntu 24.04 上执行
  `sudo apt install libwebkit2gtk-4.1-0 libgtk-3-0t64 gstreamer1.0-plugins-good gstreamer1.0-libav`。
  不想装就用 headless 版，配合浏览器界面使用。

装好后运行 `watchink doctor` 可以检查环境。

## 三步上手

1. **打开应用。** 首次启动的向导会带你下载转写所需的工具和语音模型（约 200–600 MB，视平台而定），点一下即可。
2. **填写在线模型。** 任何兼容 OpenAI 接口的服务都可以：填地址、模型名和 API Key。它只用来把字幕整理成笔记。
3. **导入视频。** 选一个本地文件，或粘贴 YouTube / Bilibili 的单个视频链接。

处理完成后，左边是视频，右边是笔记。点笔记里的时间，视频跳到那里。

## 特点

- **引用可核对。** 模型只引用字幕片段的编号，时间和链接由程序计算并校验，不会出现编造的时间戳。
- **本地转写。** 使用 whisper.cpp；macOS 上走 Metal 加速。
- **长视频不截断。** 按段处理再汇总，每个有效要点都会出现在笔记里。
- **中断可续。** 取消、失败、甚至程序被强杀之后都能接着跑，已完成的下载和转写不重做。
- **四种用法，一份数据。** 桌面窗口、浏览器、命令行、MCP，看到的是同一个资料库。
- **导出。** Markdown、SRT、VTT、TXT、JSON。

## 命令行

同一个可执行文件也是命令行工具。macOS 上它在 `Watchink.app/Contents/MacOS/Watchink`；Windows 上用 zip 里的 `watchink.com`。

```sh
# 首次配置：在线模型，并下载工具和语音模型
printf '%s' "$API_KEY" | watchink setup \
  --llm-base-url https://api.example.com/v1 --llm-model <模型名> \
  --llm-api-key-stdin --prepare

watchink import ./lecture.mp4 --wait     # 也可以是视频链接
watchink note get <video_id>
watchink export <video_id> --format md --out ./
watchink web                             # 在浏览器里使用
```

所有命令都支持 `--json`，退出码稳定，适合写脚本。

## 给 Agent 用

Watchink 自带 MCP 服务器（stdio）。在支持 MCP 的客户端里加上：

```json
{
  "mcpServers": {
    "watchink": { "command": "watchink", "args": ["mcp"] }
  }
}
```

`watchink` 不在 `PATH` 里时换成可执行文件的绝对路径；桌面应用的“设置 → Agent 接入”里有一段可以直接发给 Agent 的接入说明。

Agent 可以导入视频、查询进度、读取笔记和字幕、导出。生成笔记用的仍是你配置的在线模型，不是 Agent 自己的模型。

## 隐私

- 发给在线模型的只有**字幕文本、视频标题和时长**。音频和画面不上传。
- 程序只监听本机地址，不收集任何使用数据。
- API Key 保存在权限为 0600 的本地文件里，不写入日志和导出文件。

## 当前状态

v0，尚未发布正式版本。

| 平台 | 自动化验证 | 人工验证 |
| --- | --- | --- |
| macOS arm64 | 构建、测试、打包 | 完整流程，含 YouTube 和 Bilibili 各一个视频 |
| Linux amd64 | 构建、测试、端到端 | 未做 |
| Windows amd64 | 构建、测试、端到端 | 未做 |

还没有验证的：真实在线模型下的笔记质量（自动化测试用的是假模型）；Linux 和 Windows 上窗口内的操作和播放。v0 不含搜索、画面理解和笔记编辑，规划见 [路线图](docs/roadmap.md)。

## 文档

- [从源码构建](docs/building.md)
- [架构与代码导读](docs/architecture.md)
- [性能基准](docs/benchmark.md)
- [v0 设计规格](docs/v0.md) · [路线图](docs/roadmap.md)
- 平台验证记录：[macOS](docs/spikes/v0-p0.md) · [Linux](docs/spikes/v0-linux.md) · [Windows](docs/spikes/v0-windows.md)

## 许可证

[MIT](LICENSE)。运行时下载的第三方工具（FFmpeg、yt-dlp、Deno）和 whisper 模型各自遵循其上游许可证，本项目不分发它们。
