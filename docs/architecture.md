# Watchink 架构与代码导读

更新：2026-10-10。在 macOS arm64 上实现并验证；Windows amd64 和 Linux amd64 已实现，但只做过交叉编译，尚未在真机上运行。

本文描述代码**现在的样子**，目标是让第一次接触这个项目的人读完后能看懂它、知道去哪里改。文档分工：

| 文档 | 回答的问题 |
| --- | --- |
| [`roadmap.md`](roadmap.md) | 做什么、版本边界、如何验收 |
| [`v0.md`](v0.md) | v0 的设计决策、接口契约和取舍（规格）。文中 `§7.5` 这类引用都指它 |
| 本文 | 规格落到代码后的结构：进程、分层、数据流、每个包里有什么、按什么顺序读 |
| [`spikes/v0-p0.md`](spikes/v0-p0.md) | 哪些已经实测、哪些还没有 |
| [`benchmark.md`](benchmark.md) | 性能基准的设计、实测结果与复现方法 |

表结构、错误码表、接口清单等细节以 `v0.md` 为准，本文只说明它们在代码里的位置和相互关系。

目录：

1. [五分钟版本](#1-五分钟版本)
2. [进程模型](#2-进程模型)
3. [分层与依赖规则](#3-分层与依赖规则)
4. [契约层 `app`](#4-契约层-app)
5. [一次导入的完整路径](#5-一次导入的完整路径)
6. [调度器 `jobs`](#6-调度器-jobs)
7. [处理阶段 `pipeline`](#7-处理阶段-pipeline)
8. [笔记生成 `notes` 与 `llm`](#8-笔记生成-notes-与-llm)
9. [存储 `store`](#9-存储-store)
10. [Engine 生命周期：`engine`、`api`、`client`](#10-engine-生命周期engineapiclient)
11. [四个入口](#11-四个入口)
12. [外部工具：`tools`、`procx`、`media`、`asr`](#12-外部工具toolsprocxmediaasr)
13. [前端](#13-前端)
14. [安全边界](#14-安全边界)
15. [构建、测试与验证](#15-构建测试与验证)
16. [推荐阅读顺序](#16-推荐阅读顺序)
17. [常见改动去哪里做](#17-常见改动去哪里做)
18. [与规格的差异和已知缺口](#18-与规格的差异和已知缺口)

---

## 1. 五分钟版本

- **产品**：导入一个视频（本地文件、上传、YouTube / Bilibili 链接）→ 本地转写（whisper.cpp）→ 在线模型整理成中文笔记 → 笔记里每句话都带可点击的时间引用。
- **一个可执行文件，多个角色**：`watchink` 按子命令决定自己是桌面窗口、CLI、MCP 服务器，还是后台的 Engine。
- **只有 Engine 干活**：它独占数据库、任务调度和外部工具进程。其余角色都是通过本机 HTTP 调用它的客户端，所以四个入口看到的是同一份数据。
- **一个任务是一张固定的阶段图**（下载 → 探测 → 提取音频 → 转写 → 生成要点 → 组织章节 → 渲染），调度器按依赖和资源限制执行。
- **每个阶段先算输入指纹**，命中已有产物就跳过。重试、崩溃后续跑、重复导入、重新生成笔记，全靠这一套机制。
- **模型不产生时间戳**：模型只引用片段编号，时间和链接由程序计算并校验。

代码量（不含测试）：Go 约 2.5 万行，分 20 个包；前端是一个 React 单页应用。

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

为什么 Engine 要独立成进程：如果让“先启动的那个入口”兼任，MCP 客户端或 CLI 一退出，正在跑的转写就跟着死了。独立之后，任何客户端退出都不影响任务。

几条贯穿全局的规则：

| 规则 | 实现位置 |
| --- | --- |
| 同一数据目录只有一个 Engine：存活期间持有 `<data>/run/engine.lock` | `engine/discovery.go` |
| 地址和令牌写在 `<data>/run/engine.json`（0600）；判断存活只看锁，这个文件只用来找地址 | `engine/discovery.go` |
| 客户端连不上就自己拉起一个分离的 Engine；并发拉起时只有一个拿到锁，其余立即退出 | `client/connect.go` |
| `build_id` 不同且 Engine 空闲时，客户端让它退出并换成新的 | `client/connect.go` |
| 后台 Engine 空闲（无任务、无事件流订阅者、一段时间无请求）后自动退出 | `engine/run.go` 的 `isIdle` |
| 外部工具随 Engine 一起消失：macOS 用进程组加 `_guard` 守护进程，Linux 用进程组加 `Pdeathsig`，Windows 用 Job Object | `procx/sys_darwin.go`、`sys_linux.go`、`sys_windows.go`（Unix 共用部分在 `sys_unix.go`） |

入口是 `cmd/watchink/main.go`，不到 150 行，只做装配：把各包的函数填进 `cli.Deps`，把 `service.Open` 交给 `engine.Run`。读它就能看到所有角色是怎么接起来的。桌面代码只在带 `desktop` 构建标签时编译（`desktop_gui.go` / `desktop_nogui.go`）。

## 3. 分层与依赖规则

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
                       app           契约（不依赖任何内部包）
```

规则靠 import 关系自然成立，目前没有工具强制检查，改代码时要守住：

- **`app` 是唯一定义 JSON 形状的地方。**
- **入口层不碰内部**：`cli`、`mcpserver`、`desktop` 不引用 `store`、`jobs`、`pipeline`、`config`；需要的能力由 `main` 注入。所以它们的测试用一个假的 `app.Service` 就够了。
- **只有 `store` 引用 SQLite 驱动。**
- **只有 `procx` 创建子进程**；`tools` 这类需要执行命令的包接收注入的函数。
- **`api` 不含业务逻辑**，只做鉴权、参数解析和序列化。

## 4. 契约层 `app`

五个文件，没有逻辑，却是理解一切的起点：

| 文件 | 内容 |
| --- | --- |
| `service.go` | 三个接口。`Service`：全部用例（导入、任务、视频、笔记、字幕、导出、设置、工具、事件订阅）。`Local` = `Service` + 只在 Engine 内有意义的操作（打开播放文件、统计活动任务、停止）。`Remote` = `Service` + 客户端侧操作（取地址、请求关闭）。 |
| `dto.go` | 所有请求和响应的结构体、状态常量、阶段名常量 |
| `errors.go` | 错误码表：每个码对应是否可重试、是否需要用户处理、HTTP 状态、CLI 退出码。`app.E(code, …)` 构造错误 |
| `settings.go` | 配置结构，同时带 TOML 和 JSON 标签：`config.toml` 和设置接口是同一个形状 |
| `ids.go` | ID 生成（`vid_`、`job_`、`tr_`、`seg_`、`note_`…）和片段 ID 的拼法 |

`Service` 有两个实现：`service.Service`（Engine 内，真的干活）和 `client.Client`（客户端内，转成 HTTP）。这就是“四个入口共用同一套能力”在代码上的含义。

## 5. 一次导入的完整路径

以 `watchink import ./lecture.mp4 --wait` 为例，括号里是可以打开看的函数：

1. **CLI**（`cli/cmd_jobs.go` 的 `cmdImport`）调用注入的连接函数。
2. **连接**（`client.Connect`）：读 `engine.json` → 带令牌访问 `GET /api/v1/engine` → 连不上就拉起 `watchink engine run` 并轮询。
3. **HTTP**（`api/routes.go`）：校验 Host 和令牌，解析 JSON，调用 `Local.Import`。
4. **用例**（`service.Import`，`service/imports.go`）：
   - `validateImport` 检查参数；
   - `resolveSource` 把来源变成去重键：本地文件算抽样哈希，链接规范化成 `youtube:<id>` / `bilibili:<BV>:p<n>`；
   - 在一个写事务里：视频不存在就新建；已存在则由 `answerFromExisting` 按重复导入规则决定是直接回答（已有活动任务 / 已处理过）还是继续建任务；
   - 调 `Scheduler.NewJob` 在同一事务里写入任务和阶段行，提交后 `Announce` 唤醒调度器。
   - 立即返回 `job_id`，不等待处理。
5. **调度**（`jobs/scheduler.go`）：见下一节。
6. **阶段**（`pipeline/stage_*.go`）：见第 7 节。
7. **等待**（`cli/wait.go`）：用 `GET /api/v1/jobs/{id}?wait_seconds=…&after_rev=…` 长轮询到任务结束。桌面和 Web 则订阅 `GET /api/v1/events` 事件流。

读取笔记是另一条更短的路径：`service.GetNote`（`service/read.go`）从 `store` 取笔记版本、读产物文件、附上引用表和 coverage。

读取路径上有一处缓存（`service/cache.go`）：字幕版本和笔记版本一经生成就不可变，所以引用表和完整字幕各有一个小的 LRU，不需要任何失效逻辑。只读一部分字幕（时间窗口或指定片段）而该字幕不在缓存里时，仍走数据库的索引查询，不会为此加载全部片段。

## 6. 调度器 `jobs`

调度器不认识“视频”，只认识任务、阶段、资源和产物指纹。

| 文件 | 内容 |
| --- | --- |
| `jobs.go` | 类型：`StageDef`（名字、依赖、资源类、权重、执行函数）、`Builder`（任务 → 阶段列表）、`JobParams`；建任务；把数据库行组装成 `app.Job`（含加权总进度） |
| `scheduler.go` | 调度循环：`schedule` 扫描活动任务，`advance` 找出依赖已满足的阶段，`startStage` 占用资源并启动，`stageDone` 落库并决定任务走向，`finish` 结束任务 |
| `ops.go` | `StageContext`（阶段函数拿到的句柄：进度、依赖产物、任务结果、申请资源）和对外操作 `Cancel` / `Retry` / `Wait` / `Stop` |
| `fingerprint.go` | 指纹 = 对 `{stage, 版本, 输入}` 的规范化 JSON 求 sha256 |

要点：

- **状态机**：任务 `queued → running → succeeded / failed / cancelled`；取消经过 `cancelling`；Engine 意外退出留下的 `running` 在下次启动时变为 `interrupted`，默认再回到 `queued` 续跑。
- **每次变化**都落库、递增任务的 `rev`、写一条 `job_events`、向事件总线发布 `job.updated`。长轮询靠 `rev`，界面靠事件。
- **资源信号量**：`net` 2、`media` 1、`asr` 1、`llm` 4；`io`、`cpu` 不限。所以两个视频不会同时转写，但一个在转写时另一个可以下载。`llm` 的名额是按请求计的，`note_map` 内部每发一次请求占一个。
- **取消**：取消阶段的 context → `procx` 向进程组发 SIGTERM，3 秒后 SIGKILL；10 秒还没停则强制终止并记录。
- **重试**沿用同一个 `job_id`，`attempt` 加 1；已成功且产物仍有效的阶段会被指纹机制跳过。

## 7. 处理阶段 `pipeline`

`pipeline.go` 里的 `processVideoStages` 就是阶段图：

```text
ensure_tools ─▶ acquire ─▶ probe ─┬▶ extract_audio ─▶ transcribe ─▶ note_map ─▶ note_reduce ─▶ note_render
   net          net / io    io    │      media            asr          llm          llm           cpu
                                  └▶ playback（media，不计入总进度）
```

每个阶段函数都是同一个形状，读懂一个就读懂了全部：

1. 取输入：任务参数（`options`）和上游阶段的产物（`depArtifact`）；
2. 算指纹（`jobs.Fingerprint`）；
3. 命中已登记且文件完好的产物就返回 `reused(a)`（`cached`）；
4. 否则干活，把结果写到 `<data>/library/<video_id>/` 下并登记产物，返回 `done(a)`。

| 文件 | 阶段 | 做什么 |
| --- | --- | --- |
| `pipeline.go` | — | 依赖注入、阶段图、阶段版本号、`wrap`（给子进程打任务标签、把磁盘满统一成 `DISK_FULL`） |
| `artifacts.go` | — | 所有阶段共用的小工具：`cached` / `reused` / `done` / `depArtifact` / `readJSON` / `putJSON` |
| `settings.go` | — | 把配置翻译成各能力包的参数：默认 ASR 模型、语言、在线模型配置、分块参数 |
| `stage_tools.go` | `ensure_tools`、`prepare` | 检查并安装本任务需要的工具和模型 |
| `stage_acquire.go` | `acquire` | 本地文件复制进库（`copyLocalFile`）、上传文件移入（`takeUpload`）、链接用 yt-dlp 下载（`download`） |
| `stage_media.go` | `probe`、`extract_audio`、`playback` | ffprobe 读时长和流信息；提取 16 kHz 单声道 WAV 并记录时间偏移；判断能否直接播放，否则重封装或转码 |
| `stage_transcribe.go` | `transcribe` | `asrSetup` 解析出二进制、模型、线程数 → 需要时先检测语言 → 整段转写 → `saveTranscript` 后处理并入库 |
| `stage_note.go` | `note_map`、`note_reduce`、`note_render` | 见第 8 节 |

几个不看代码容易漏掉的细节：

- **音频用完即删**：转写成功后 WAV 默认删除（产物标记为 `purged`）。下次任务如果字幕缓存仍有效，`extract_audio` 直接复用这条已清理的记录，不会为了一个用不上的文件重新提取。
- **相同音频的两个视频**共用一份 ASR 原始输出，但各自克隆一个字幕版本（`transcriptFor`），因为片段 ID 属于具体的视频。
- **`note_map` 按块缓存**：每完成一块立即登记，任务中断后从已有的块继续。
- **`reprocess_from`**：任务带这个参数时，从指定阶段起 `sc.IgnoreCache` 为真，强制重做；之前的阶段照常复用。
- **阶段版本号**（`pipeline.go` 末尾）参与指纹：改了某个阶段的逻辑就把它加 1，旧产物自然失效。

## 8. 笔记生成 `notes` 与 `llm`

`notes` 是纯逻辑包，只通过 `llm.Client` 接口访问模型，所以它的测试全部跑在假模型上。

| 步骤 | 文件 | 说明 |
| --- | --- | --- |
| 分块 | `chunk.go` | 按估算 token 和时长切，优先在停顿处切开，不拆片段；每块附带上一块末尾约 30 秒作为不可引用的上下文 |
| map | `map.go` | 每块一次请求，产出要点和术语。要点只能引用本块的片段编号 `S123`；无效引用被移除，移除后没有引用的要点被丢弃；丢弃过多就发一次修复请求；输出被截断则把块对半拆开重试 |
| reduce | `reduce.go` | 只把要点编号组织成章节、概览和结论，不再接触字幕。没被放进章节的要点由程序补回；要点太多时改为两级 reduce |
| 渲染 | `render.go` | `Render` 依次执行 `index`（建索引、汇总统计）→ `writeHeader` → `writeOverview` → `writeSections` → `writeTakeaways` → `writeGlossary`，把编号换成 `[mm:ss](wk://seg/<片段ID>)` |
| 校验 | `render.go` 的 `Verify` | 重新解析生成的 Markdown：每个链接都必须对应本字幕版本的片段、时间在范围内、与引用表一一对应。不满足就以 `INTERNAL` 失败，因为这是程序缺陷而不是模型问题 |
| 导出 | `export.go` | 把 `wk://seg/` 改写成平台时间链接或纯文本时间；字幕导出 SRT / VTT / TXT |
| 提示词 | `prompt.go` | 模板和 `PromptVersion`（参与指纹） |

`llm` 是自写的 OpenAI 兼容 Chat Completions 客户端：

- `client.go`：结构化输出按 `json_schema` → `json_object` → 仅提示词三级降级，降级结果按 `(base_url, model)` 缓存；429 / 5xx / 超时指数退避重试，401 / 403 / 404 立即失败并映射成对应错误码。
- `clean.go`：解析前去掉代码围栏和 `<think>` 片段。
- `tuning.go`：请求参数自愈。各家兼容端点在输出上限、`temperature`、超时等细节上不一致，客户端遇到这类拒绝或截断时自动调整后重发，并记录一条警告，而不是让任务失败。
- `llmtest/`：可脚本化的假服务，能模拟非法 JSON、401、429、截断等；`llmtest/cmd/fakellm` 可以独立运行，没有 API Key 时用它跑通全流程。

## 9. 存储 `store`

- **SQLite**：WAL 模式，写操作经单一连接串行执行（`Store.Write(ctx, func(tx *Tx) error)`），读操作走独立连接池。迁移在 `migrations/`，按 `PRAGMA user_version` 顺序执行，迁移前备份。
- **文件**（`files.go`）：数据库只存相对数据目录的路径。写产物的顺序固定为：临时文件 → fsync → rename → 登记到数据库。写了但没登记的孤立文件在 Engine 启动时由 `SweepOrphans` 清理。
- **产物表是指纹机制的另一半**：`(kind, fingerprint)` 唯一。`VerifyArtifact` 重新校验 sha256，`ArtifactPresent` 只做便宜的存在性检查（大媒体文件用后者）。

| 文件 | 负责的表 |
| --- | --- |
| `videos.go` | `videos` |
| `jobs.go` | `jobs`、`job_stages`、`job_events`，以及重启恢复 `Recover` |
| `artifacts.go` | `artifacts` |
| `transcripts.go` | `transcripts`、`segments` |
| `notes.go` | `notes`、`citations` |
| `misc.go` | `uploads`、`exports`、`child_processes` |

数据目录 `<data>`（macOS 默认 `~/Library/Application Support/Watchink/Data`）：

```text
config.toml  watchink.db  run/  logs/
library/<video_id>/   source.<ext>  audio.wav  playback.mp4  probe.json
                      asr/<transcript_id>.json   work/…   notes/<note_id>/note.{md,json}
exports/  uploads/  tools/  models/
```

配置目录 `<cfg>` 只放 `secrets.toml`（0600，API Key）和可选的 `location.toml`，凭据不在数据目录里。两个目录的解析规则在 `config/paths.go`。

## 10. Engine 生命周期：`engine`、`api`、`client`

**`engine.Run`**（`engine/run.go`）是一段带编号的线性流程，对应规格的启动顺序：

1. 拿锁（拿不到说明已有 Engine，返回 `ErrAlreadyRunning`）；
2. 建日志（JSON 行、轮转、脱敏）；
3. 调用 `Build` 回调得到 service——`main` 传进来的是 `service.Open`，它读配置、开数据库、跑迁移、做重启恢复；
4. 监听 `127.0.0.1`（默认 47823，被占用则换随机端口）；
5. 生成令牌，写 `engine.json`；
6. 启动调度器，开始服务；
7. `waitForStop` 等待停止原因（信号、关闭请求、空闲、HTTP 服务异常）；
8. `stopService` 让运行中的阶段有机会收尾，然后依次关闭 HTTP、数据库、日志，最后释放锁。

`engine` 不认识业务：它只通过 `Built` 结构拿到一个 `app.Local` 和几个回调。

**`service.Open`**（`service/service.go`）是 Engine 内部真正的装配点：创建 store、事件总线、`procx.Runner`、`tools.Manager`、调度器，把 `pipeline` 注册进调度器，然后执行重启恢复（修正上次留下的状态、清理孤立文件、核对并终止残留子进程）。

**`api`**：

| 文件 | 内容 |
| --- | --- |
| `handler.go` | 处理顺序：Host 校验 → CORS → 路由（鉴权按路由进行，因为 `/health`、媒体和前端页面的规则不同）；请求日志；空闲统计 |
| `security.go` | Host 白名单、CORS、Bearer 令牌、浏览器同源规则 |
| `routes.go` | 全部 `/api/v1` 路由，一个路由一个小函数：解析参数 → 调 service → 写 JSON |
| `events.go` | 事件流（SSE 格式）、心跳、断线后按 `Last-Event-ID` 回放 |
| `spa.go` | 内嵌前端的静态文件和前端路由回退 |

**`client`**：`connect.go` 实现发现与拉起；`client.go` 把 `app.Service` 的每个方法翻译成一次 HTTP 调用，把错误 JSON 还原成 `*app.Error`；`stream.go` 读事件流并自动重连。

`events.Bus`（`events/bus.go`）是内存事件总线：事件 ID 单调递增，保留最近一批用于回放；订阅者请求的事件已不在缓冲里时返回 `reset`，客户端应全量刷新。

## 11. 四个入口

| 入口 | 包 | 说明 |
| --- | --- | --- |
| CLI | `cli` | `cli.go` 建命令树、统一处理全局参数和退出码；`cmd_*.go` 每个文件一组命令；`output.go` 负责 `--json` 与人类可读两种输出；`wait.go` 实现 `--wait` / `--follow` |
| MCP | `mcpserver` | `server.go` 起 stdio 服务器，第一次工具调用时才连接或拉起 Engine，连接断了重连一次；`tools.go` 定义 8 个工具；`schema.go` 生成输入输出 schema。**生成笔记用的仍是 Engine 配置的在线模型**，MCP 只是转发 |
| 桌面 | `desktop` | Wails v2 外壳。前端从内嵌资源加载，业务请求直接走 HTTP；`app.go` 的 6 个绑定只提供原生能力（拿 Engine 地址和令牌、选文件、保存导出、在访达中显示、打开链接、Agent 接入信息） |
| Web | — | 没有单独的包：就是 Engine 的 `api` 把同一个前端发给浏览器。默认关闭，由 `web.enabled` 控制 |

## 12. 外部工具：`tools`、`procx`、`media`、`asr`

| 包 | 职责 |
| --- | --- |
| `tools` | `manifest.json` 锁定每个工具和模型的版本、下载地址、sha256。`Manager.Ensure` 负责下载 → 校验 → 解压 → 冒烟测试 → 原子安装；`Manager.Path` / `ModelPath` 给出安装位置。whisper-cli 是自建并 `go:embed` 进二进制的，首次使用时释放。永不执行未通过校验的文件 |
| `procx` | `Runner.Run` 启动子进程（参数数组，不经 shell），转发进度输出，保留 stderr 末尾用于报错，context 取消时终止整个进程组。`GuardMain` 是 `_guard` 子命令的实现 |
| `media` | `Probe`（ffprobe）、`ExtractAudio` / `PlanPlayback` / `BuildPlayback`（ffmpeg）、`Download`（yt-dlp，含错误分类）、`NormalizeURL`（链接规范化与主机名白名单）、`SampleHash`（抽样哈希去重） |
| `asr` | `WhisperCpp.DetectLanguage` / `Transcribe` 调用 whisper-cli 并解析它的 JSON；`PostProcess` 去空片段、折叠连续重复的幻觉片段、保证时间单调 |

工具来源：whisper-cli 由 `third_party/whisper/build.sh` 构建（macOS 用 Metal，Linux 和 Windows 为 CPU 版）；ffmpeg / ffprobe、yt-dlp 首次需要时下载；deno 只在第一次导入 YouTube 链接时下载；whisper 模型来自 Hugging Face 的固定 revision，可走镜像或本地导入。清单包含 `darwin-arm64`、`linux-amd64`、`windows-amd64` 三个平台的条目。Linux 的 ffmpeg 是 `tar.xz` 包，由 `tools/xz.go` 里自带的纯 Go 解码器解压（依赖清单不允许引入新的第三方库）。

## 13. 前端

`frontend/` 是 React + TypeScript + Vite 的单页应用，桌面和 Web 共用，构建到 `internal/webui/dist` 后嵌进二进制。

| 目录 | 内容 |
| --- | --- |
| `src/api` | HTTP 客户端、事件流读取、TanStack Query hooks；`types.ts` 由 Go 的 DTO 生成（`pnpm gen:types`） |
| `src/platform` | 桌面与 Web 的全部差异（鉴权、选文件 / 上传、保存 / 下载、打开链接）；页面代码不做分支 |
| `src/pages` | 资料库、视频详情、任务、设置、首次配置 |
| `src/components` | 播放器、笔记、字幕面板、导入对话框等 |
| `src/i18n/zh.ts` | 全部界面文案（v0 只有简体中文） |
| `mock/`、`e2e/` | 不依赖 Engine 的模拟服务，以及基于它的 Playwright 用例 |

引用跳转：前端拦截笔记里的 `wk://seg/<id>` 链接，从引用表查到起始时间，定位播放器并高亮字幕面板中的片段。

**网页是 Engine 进程发出来的**。改了前端后，浏览器里要看到新界面，必须让 Engine 换成新二进制；`build_id` 不同时客户端会自动替换空闲的 Engine。

## 14. 安全边界

- 只监听 `127.0.0.1`，校验 `Host` 头防 DNS rebinding。
- 桌面、CLI、MCP 使用每次启动随机生成的 Bearer 令牌。
- 浏览器界面是可选开关（`web.enabled`，默认关）。开启后不需要登录，靠同源规则保护：带跨站标记或 Origin 不是 Engine 自己的请求被拒绝；CORS 只放行桌面 WebView 的来源。
- 本地路径导入受 `security.import_roots` 限制（解析符号链接后判断）。Engine 只在数据目录内写文件，导出到别处由 CLI 或桌面进程自己写。
- 外部命令只用参数数组；链接先规范化并通过主机名白名单，再放在 `--` 之后交给 yt-dlp。
- 模型没有任何工具，字幕里的指令只是数据；渲染时转义所有模型文本。发给在线模型的只有字幕文本、标题和时长。
- 日志和错误详情里的令牌、API Key 统一脱敏；凭据接口只写不读。

## 15. 构建、测试与验证

```sh
make build          # 纯 Go 的无界面二进制 bin/watchink
make desktop        # 前端 + whisper-cli + cgo 构建，组装并 ad-hoc 签名 dist/Watchink.app
make test lint      # Go 测试、gofmt、go vet（含 desktop 标签）
cd frontend && corepack pnpm test && corepack pnpm e2e
scripts/e2e-macos.sh  # 真实 ffmpeg / whisper + 假模型的端到端检查（需要已安装工具和模型）
go test -run '^$' -bench . -benchmem ./internal/bench   # 基准测试
go run ./internal/bench/cmd/loadtest                    # 并发 HTTP 压测
```

测试的层次：

| 层 | 覆盖方式 |
| --- | --- |
| 能力库（`notes`、`llm`、`media`、`asr`、`tools`、`config`、`events`、`procx`、`store`） | 各自的单元测试；`media` 和 `asr` 另有需要真实工具的集成测试，由环境变量开启，默认跳过 |
| `jobs` | 用假阶段测调度、状态机、取消、重试、恢复 |
| `api` ↔ `client` | client 对着真实的 api 处理器加假 service 做往返测试 |
| `cli`、`mcpserver` | 假的 `app.Service` |
| 前端 | vitest 单元测试；Playwright 跑在模拟服务上 |
| `pipeline` | **没有 Go 测试**。它依赖真实的工具管理器和子进程，目前由 `scripts/e2e-macos.sh` 覆盖：导入、重复导入、重新生成笔记三条路径，并检查各阶段的复用情况 |
| `service` | 并发导入（同一文件、不同文件）、缓存、字幕片段选择；其余用例由端到端脚本和压测覆盖 |

有测试的有测试的各包语句覆盖率大多在 80%–97% 之间；`pipeline` 为 0%。

## 16. 推荐阅读顺序

1. `internal/app/service.go`、`dto.go`——系统能做什么。
2. `cmd/watchink/main.go`——各部分怎么接起来。
3. `internal/service/service.go` 的 `Open`——Engine 内部怎么装配。
4. `internal/service/imports.go` 的 `Import`——一个请求怎么变成任务。
5. `internal/pipeline/pipeline.go`，然后任选一个 `stage_*.go`（建议 `stage_transcribe.go`）——阶段的统一写法。
6. `internal/jobs/jobs.go` 的类型定义，再看 `scheduler.go` 的 `schedule` → `advance` → `startStage` → `stageDone`。
7. `internal/notes/map.go`、`reduce.go`、`render.go`——笔记和引用是怎么来的。
8. `internal/engine/run.go` 的 `Run` 与 `internal/client/connect.go` 的 `Connect`——进程怎么互相找到。
9. 需要时再看 `api/routes.go`、`cli/`、`mcpserver/`、`desktop/`：它们都很薄。

## 17. 常见改动去哪里做

| 想做的事 | 位置 |
| --- | --- |
| 加一个接口字段或用例 | `app`（DTO / 接口）→ `service` 实现 → `api` 路由 → `client` → `pnpm gen:types` |
| 加一个处理阶段 | `pipeline` 写阶段函数并加进 `processVideoStages`；需要新产物类型时动 `store` |
| 改某个阶段的逻辑 | 改完把 `pipeline.go` 里对应的阶段版本号加 1 |
| 改提示词或笔记结构 | `notes/prompt.go` 等，并递增 `PromptVersion` 或 `RendererVersion` |
| 升级外部工具 | `internal/tools/manifest.json`（版本、URL、sha256） |
| 加 CLI 命令 / MCP 工具 | `cli` / `mcpserver`，只用 `app.Service` |
| 改表结构 | 在 `store/migrations` 新增迁移文件，不改已有的 |
| 加配置项 | `app/settings.go` → `config`（默认值、校验、patch）→ 设置页 |
| 桌面原生能力 | `desktop/app.go` 的绑定 + `frontend/src/platform/desktop.ts` |

## 18. 与规格的差异和已知缺口

| 项 | 现状 |
| --- | --- |
| 平台 | macOS arm64 已验证。Windows amd64、Linux amd64 的进程管理、工具清单、打包脚本和 CI 工作流已写好并能交叉编译，但没有在真机上运行过；验证记录见 `spikes/v0-linux.md`、`spikes/v0-windows.md` |
| Web 鉴权 | 可选开关加同源规则，没有 `v0.md` 最初设计的登录码和 cookie；`v0.md` 相应章节尚未同步 |
| VAD | 默认关闭：实测 VAD 下时间映射错误并会丢内容 |
| 英文转写 | 加了固定提示词，否则输出无大小写和标点 |
| 真实在线模型 | 结构化输出支持矩阵（S-10）和笔记质量未用真实服务验证，全流程是用假服务跑通的 |
| 桌面窗口 | 只做过程序化启动检查，没有系统的人工操作验证 |
| 自动化测试 | `pipeline` 没有 Go 测试，`service` 只覆盖了并发导入、缓存和字幕选择；规格中的故障注入构建和依赖规则检查没有落地；CI 工作流（`.github/workflows/`）已写好但从未运行 |
| 引用粒度 | 引用只能定位到片段起点，单个片段可长达约 28 秒 |
