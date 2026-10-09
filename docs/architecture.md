# Watchink 架构概览

更新：2026-10-10。对应提交 `a0bb872` 之后的代码，只在 macOS arm64 上实现和验证。

本文描述代码**现在的样子**，供第一次读代码的人建立全局印象。三份文档的分工：

| 文档 | 回答的问题 |
| --- | --- |
| [`roadmap.md`](roadmap.md) | 做什么、版本边界、如何验收 |
| [`v0.md`](v0.md) | v0 的设计决策、接口契约和取舍（规格） |
| 本文 | 规格落到代码后的结构：进程、分层、一次导入怎么流转、去哪里改 |
| [`spikes/v0-p0.md`](spikes/v0-p0.md) | 哪些已经实测、哪些还没有 |

细节（表结构、错误码、接口清单）以 `v0.md` 为准，本文只给出位置和关系，不重复。

## 1. 一句话

一个可执行文件 `watchink`，按子命令扮演不同角色。其中只有一个角色（Engine）真正干活：它独占数据库、任务调度和外部工具进程；桌面窗口、浏览器、CLI、MCP 都是通过本机 HTTP 调用它的客户端。

## 2. 进程模型

```text
┌───────────┐ ┌───────────┐ ┌───────────┐ ┌──────────────┐
│ 桌面窗口   │ │ 浏览器     │ │ CLI       │ │ MCP stdio    │
│ Wails v2  │ │（可选）    │ │ watchink… │ │ watchink mcp │
└─────┬─────┘ └─────┬─────┘ └─────┬─────┘ └──────┬───────┘
      └── HTTP 127.0.0.1（Bearer 令牌；浏览器走同源规则）──┘
                           ▼
┌──────────────── Engine 进程（每个数据目录唯一）────────────────┐
│ api       路由 · 鉴权 · 事件流 · 媒体 Range · 内嵌前端          │
│ service   用例实现（app.Local）                               │
│ jobs      调度器 · 状态机 · 资源信号量 · 指纹                  │
│ pipeline  各处理阶段                                          │
│ media · asr · llm · notes · tools · procx                    │
│ store     SQLite（单写连接）+ 数据目录文件                     │
└───────────────────────────┬──────────────────────────────────┘
                            ▼ 子进程（经 `watchink _guard` 包装）
              yt-dlp (+deno) · ffmpeg / ffprobe · whisper-cli
```

要点：

- **Engine 是独立进程**（`watchink engine run`，或前台的 `watchink serve`）。任何客户端退出都不会中断任务。
- **发现与单例**：Engine 存活期间持有 `<data>/run/engine.lock`；地址和令牌写在 `<data>/run/engine.json`（0600）。客户端连不上时自己拉起一个分离的 Engine；多个客户端同时拉起时只有一个能拿到锁。
- **版本替换**：客户端发现 Engine 的 `build_id` 与自己不同、且 Engine 空闲时，会让它退出并拉起新的。开发时有未提交改动的构建会在 `build_id` 后加时间后缀，保证每次重新打包都能触发替换。
- **空闲退出**：后台 Engine 在没有任务、没有事件流订阅者、一段时间没有请求后退出；桌面窗口一直订阅事件流，所以窗口开着时 Engine 不会退出。
- **子进程清理**：所有外部工具都由 `procx` 启动，放进独立进程组。macOS 上经 `_guard` 包装，Engine 被强杀后 guard 负责清理整组进程。

入口在 `cmd/watchink/main.go`：它只做装配，把各包的函数注入 `cli.Deps`，并把 `service.Open` 交给 `engine.Run`。桌面相关代码只在带 `desktop` 构建标签时编译（`desktop_gui.go` / `desktop_nogui.go`）。

## 3. 分层与依赖方向

```text
cmd/watchink ── 装配
   │
   ├─ cli · mcpserver · desktop      入口层：只认识 app 里的接口和 DTO
   │        │
   │        ▼
   ├─ client ──HTTP──▶ api           传输层：client 实现 app.Remote，api 服务 app.Local
   │                    │
   ├─ engine            ▼            进程生命周期：锁、发现文件、监听、日志、空闲退出
   │                 service         用例层：导入去重、读取、导出、设置、工具
   │                    │
   │              jobs · pipeline    调度与阶段
   │                    │
   │     media · asr · llm · notes · tools · config · events    能力库（彼此独立）
   │                    │
   └──────────── store · procx       唯二的“特权”包
                        │
                       app           契约：DTO、错误码、ID、Service 接口（不依赖任何内部包）
```

规则（由包的 import 关系保证，改代码时要守住）：

- `internal/app` 是契约，任何 JSON 形状只在这里定义。`Service` 是全部用例；`Local` = `Service` + 仅 Engine 内可用的操作；`Remote` = `Service` + 客户端侧操作。CLI、MCP、桌面都只针对这些接口编写，所以它们的测试用假实现即可。
- `cli`、`mcpserver`、`desktop` 不引用 `store`、`jobs`、`pipeline`、`config`，需要的能力由 `main` 注入。
- 只有 `store` 引用 SQLite 驱动；只有 `procx` 创建子进程（`tools` 等需要执行命令的包接收注入的函数）。
- `api` 不含业务逻辑，只做鉴权、参数解析和序列化。

## 4. 包一览

| 包 | 职责 | 备注 |
| --- | --- | --- |
| `app` | DTO、错误码与退出码、ID 生成、`Service` / `Local` / `Remote` 接口、`Settings` | 契约，改动要同步前端类型 |
| `engine` | 锁、`engine.json`、启动顺序、日志轮转与脱敏、空闲退出 | 通过 `Build` 回调拿到 service，不认识业务 |
| `api` | `/api/v1` 路由、Host 校验、CORS、事件流、媒体 Range、内嵌前端 | `apitest` 提供假 service |
| `client` | Engine 发现与拉起、HTTP 客户端、事件流读取 | 实现 `app.Remote` |
| `service` | `app.Local` 的实现：装配 store / 调度器 / 工具 / 事件总线，导入事务与去重，读写用例 | 重启恢复也在这里 |
| `jobs` | 调度循环、阶段 DAG、资源信号量、取消 / 重试 / 中断、指纹、Job DTO 组装 | 不认识具体阶段 |
| `pipeline` | `process_video` 的 9 个阶段和 `prepare_tools` 的 1 个阶段 | 向调度器注册 |
| `media` | ffprobe / ffmpeg / yt-dlp 调用与输出解析、URL 规范化、抽样哈希、播放策略 | |
| `asr` | whisper-cli 调用、进度解析、字幕后处理 | |
| `llm` | OpenAI 兼容 Chat Completions 客户端：结构化输出三级降级、重试 | `llmtest` 是可脚本化的假服务 |
| `notes` | 分块、map、reduce、引用校验、渲染、导出改写、字幕格式 | 纯逻辑，只依赖 `llm` 接口 |
| `tools` | 工具与模型清单、下载、sha256 校验、安装、内嵌 whisper-cli 的释放 | `manifest.json` 锁定版本 |
| `store` | SQLite 打开与迁移、各表的读写、数据目录内的文件操作 | 迁移在 `migrations/` |
| `procx` | 子进程启动、取消、进程树清理、`_guard`、打开浏览器 / 文件管理器 | |
| `config` | 数据目录与配置目录解析、`config.toml`、`secrets.toml`、导入路径检查 | |
| `events` | 内存事件总线，带回放缓冲 | 支撑 `GET /api/v1/events` |
| `cli` | cobra 命令树、`--json` 输出与退出码 | |
| `mcpserver` | MCP stdio 服务器，8 个工具，转发到 Engine | 首次调用时才连接 / 拉起 Engine |
| `desktop` | Wails 外壳和 6 个原生绑定 | 仅 `desktop` 标签 |
| `webui` | `go:embed` 前端构建产物 | |

## 5. 一次导入怎么流转

以 `watchink import ./lecture.mp4 --wait` 为例：

1. **CLI** 调用 `client.Connect`：读 `engine.json`，连不上就拉起 Engine，然后 `POST /api/v1/imports`。
2. **api** 校验令牌和 Host，解析请求，调用 `service.Import`。
3. **service** 在一个写事务里计算去重键、应用重复导入策略、建视频和任务，立即返回 `job_id`。
4. **jobs** 调度循环按任务创建顺序挑选依赖已满足的阶段，受资源信号量限制后执行。每次状态变化都落库、递增 `rev` 并发布事件。
5. **pipeline** 各阶段先算输入指纹，命中已登记且文件完好的产物就直接复用；否则干活、写文件、登记产物。
6. **CLI** 用 `GET /api/v1/jobs/{id}?wait_seconds=…&after_rev=…` 长轮询直到任务结束；桌面和 Web 则订阅事件流。

阶段 DAG 与资源类（定义在 `pipeline/pipeline.go`）：

```text
ensure_tools ─▶ acquire ─▶ probe ─┬▶ extract_audio ─▶ transcribe ─▶ note_map ─▶ note_reduce ─▶ note_render
   net          net / io    io    │      media            asr          llm          llm           cpu
                                  └▶ playback（media，不计入总进度）
```

并发上限：`net` 2、`media` 1、`asr` 1、`llm` 4（并发请求数）；`io` 和 `cpu` 不限。

**指纹复用是可靠性的核心**：重试、重启后续跑、重复导入、`--reprocess-from` 都靠它，而不是各写一套逻辑。例如换了模型配置后重试，只有 `note_*` 的指纹变化，下载和转写不会重跑。

## 6. 笔记与引用

模型从不输出时间戳。流程在 `notes` 包里：

1. 字幕按 token 和时长分块。
2. **map**：每块调用一次模型产出要点，要点只能引用本块内的片段编号（`S123`）；无效引用被移除，按块缓存。
3. **reduce**：只组织要点编号，不再接触字幕。
4. **渲染**：程序把编号换成片段 ID 和时间，生成 `[mm:ss](lt://seg/<segment_id>)`，并把没被放进章节的要点补回去。
5. **校验**：重新解析生成的 Markdown，确认每个链接都对应本字幕版本的片段且时间在范围内，否则阶段以 `INTERNAL` 失败。

结构化笔记 JSON 是规范形式，Markdown 是渲染结果；导出时再把 `lt://seg/` 改写成平台时间链接或纯文本时间。

## 7. 数据

- **数据目录** `<data>`（macOS 默认 `~/Library/Application Support/Watchink/Data`）：`watchink.db`、`config.toml`、`run/`、`logs/`、`library/<video_id>/`（源文件、播放文件、ASR 原始输出、中间结果、笔记）、`exports/`、`uploads/`、`tools/`、`models/`。数据库只存相对路径，目录可整体搬走。
- **配置目录** `<cfg>`：只有 `secrets.toml`（0600，API Key）和可选的 `location.toml`。凭据不在数据目录内。
- **事实来源**是 SQLite 加文件产物。直接改数据目录里的文件不受支持。
- 写产物的顺序固定为：临时文件 → fsync → rename → 登记到数据库；孤立文件在 Engine 启动时清理。

## 8. 安全边界

- 只监听 `127.0.0.1`，校验 `Host` 头防 DNS rebinding。
- 桌面、CLI、MCP 用每次启动随机生成的 Bearer 令牌。
- **浏览器界面是可选开关**（`web.enabled`，默认关）。开启后浏览器不需要登录，靠同源规则保护：带跨站标记或 Origin 不是 Engine 自己的请求被拒绝，CORS 只放行桌面 WebView 的来源。这一点与 `v0.md` 最初设计的“一次性登录码换 cookie”不同。
- 本地路径导入受 `security.import_roots` 限制；Engine 只在数据目录内写文件，导出到别处由 CLI 或桌面进程自己写。
- 外部命令只用参数数组；链接先规范化并通过主机名白名单。
- 模型没有任何工具；字幕里的指令只是数据。发给在线模型的只有字幕文本、标题和时长。
- 日志和错误详情里的令牌、API Key 统一脱敏。

## 9. 前端

`frontend/` 是 React + TypeScript + Vite 的单页应用，桌面和 Web 共用，构建到 `internal/webui/dist` 后被嵌进二进制。

| 目录 | 内容 |
| --- | --- |
| `src/api` | HTTP 客户端、事件流读取、TanStack Query hooks；`types.ts` 由 Go DTO 生成（`pnpm gen:types`） |
| `src/platform` | 桌面与 Web 的全部差异（鉴权、选文件 / 上传、保存 / 下载、打开链接）；页面代码不做分支 |
| `src/pages` | 资料库、视频详情、任务、设置、首次配置 |
| `src/components` | 播放器、笔记、字幕面板、导入对话框等 |
| `src/i18n/zh.ts` | 全部界面文案（v0 只有简体中文） |
| `mock/` · `e2e/` | 不依赖 Engine 的模拟服务，以及基于它的 Playwright 用例 |

业务调用一律走 HTTP；Wails 绑定只提供原生能力。**网页由 Engine 进程提供**，所以改了前端后要让 Engine 换成新二进制才能在浏览器里看到。

## 10. 外部工具

| 工具 | 来源 | 时机 |
| --- | --- | --- |
| whisper-cli | 自建（`third_party/whisper/build.sh`，Metal），内嵌在二进制里 | 首次使用时释放并校验 |
| ffmpeg / ffprobe、yt-dlp | 按 `internal/tools/manifest.json` 下载固定版本并校验 sha256 | 首次需要时 |
| deno | 同上 | 仅首次导入 YouTube 链接时 |
| whisper 模型 | Hugging Face 固定 revision | 首次需要时，可走镜像或本地导入 |

清单目前只有 `darwin-arm64` 的条目。开发时可用 `WATCHINK_WHISPER_BIN` 指向现成的 whisper-cli。

## 11. 构建与测试

- `make build`：纯 Go 的无界面二进制 `bin/watchink`（不含桌面）。
- `make desktop`：前端 + whisper-cli + 带 `desktop,production` 标签的 cgo 构建，组装并 ad-hoc 签名 `dist/Watchink.app`。
- `make test lint`：Go 单元测试和检查；前端用 `corepack pnpm test` / `e2e`。

测试的层次：能力库各自有单元测试；`jobs` 用假阶段测调度和状态机；`client` 对着 `api` + 假 service 做往返测试；`cli` 和 `mcpserver` 用假的 `app.Service`；前端 e2e 跑在模拟服务上。**`pipeline` 和 `service` 目前没有自己的测试文件**，它们只被开发机上的端到端手工验证覆盖（见 spike 记录）。

## 12. 与规格的差异和已知缺口

| 项 | 现状 |
| --- | --- |
| 平台 | 只有 macOS arm64。Windows / Linux 的 procx、工具清单、打包都只有带构建标签的占位 |
| Web 鉴权 | 改为可选开关加同源规则，没有登录码和 cookie（见 §8） |
| VAD | 默认关闭：实测 VAD 下时间映射错误并会丢内容 |
| 英文转写 | 加了固定提示词，否则输出无大小写和标点 |
| 真实在线模型 | 结构化输出支持矩阵（S-10）和笔记质量未用真实服务验证，全流程是用 `llmtest` 假服务跑通的 |
| 桌面窗口 | 只做过程序化启动检查，没有系统的人工操作验证 |
| 自动化测试 | `pipeline`、`service` 缺测试；规格中的故障注入构建和 depguard 检查未见落地 |
| 引用粒度 | 引用只能定位到片段起点，单个片段可长达约 28 秒 |

## 13. 常见改动去哪里做

| 想做的事 | 位置 |
| --- | --- |
| 加一个接口字段或用例 | `app`（DTO / 接口）→ `service` 实现 → `api` 路由 → `client` → 重新生成前端类型 |
| 加一个处理阶段 | `pipeline` 写阶段函数并加进阶段列表；需要新产物类型时动 `store` |
| 改提示词或笔记结构 | `notes`（记得递增 `prompt_version`，它参与指纹） |
| 升级某个外部工具 | `internal/tools/manifest.json`（版本、URL、sha256） |
| 加 CLI 命令 / MCP 工具 | `cli` / `mcpserver`，只用 `app.Service` |
| 改表结构 | `store/migrations` 新增迁移文件，不改已有的 |
| 桌面原生能力 | `desktop` 的绑定 + `frontend/src/platform/desktop.ts` |
