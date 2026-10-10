# v0 Windows amd64：S-02 决策与验证记录

记录日期：2026-10-10。

**本文记录的是一次没有 Windows 机器的移植。** 全部工作在 macOS arm64 上完成，没有 Wine：Windows 的代码只做过交叉编译、`go vet` 和编译测试，没有运行过一行。下文凡是没有标“已验证”的，都只是依据 Windows 的文档化行为做出的判断。真正的验证由 `.github/workflows/windows.yml` 在 `windows-latest` 上执行；该工作流本身也还没有运行过。

## 结论一览

| 项 | 状态 |
| --- | --- |
| S-02 子系统方案 | 选定方案 c（zip 内 `Watchink.exe` + `watchink.com`）。依据文档化行为推理得出，未实测 |
| Windows 代码 | 全部能为 `windows/amd64` 编译，`go vet` 通过（nogui 与 desktop 两种标签） |
| GUI 交叉编译 | 已验证：在 macOS 上 `CGO_ENABLED=0 GOOS=windows go build -tags desktop,production -ldflags "-H windowsgui"` 成功，产物为 GUI 子系统的 PE32+ |
| 工具清单 | 已验证：4 个 `windows-amd64` 条目都实际下载过，sha256 与上游校验文件一致，压缩包内的路径与 `files` 一致 |
| 在 Windows 上运行 | **未验证**：进程管理、单例锁、桌面窗口、whisper-cli 构建、安装流程、全流程 |
| S-01、S-04～S-09 的 Windows 部分 | 未执行。其中 S-01、S-06、S-08 的一部分已写成 CI 步骤 |

## S-02：为什么是方案 c

`docs/v0.md` §16.5 要求按 a → c → b 的顺序，选第一个同时满足三条标准的方案：双击不出现控制台；在 cmd 和 PowerShell 里 shell 等待命令结束、输出完整、管道可用；MCP 客户端经 stdio 连接正常。

**方案 a（单个 GUI 子系统 exe + `AttachConsole`）不满足标准 2。** 程序属于哪个子系统写在 PE 头里，shell 在启动程序之前就据此决定是否等待：

- cmd 在交互式提示符下启动 GUI 子系统程序后立即回到提示符，只有在批处理文件里或用 `start /wait` 才等待。
- PowerShell 同样不等待 GUI 子系统程序，除非它的输出被管道或重定向接走。
- 程序事后调用 `AttachConsole(ATTACH_PARENT_PROCESS)` 只能把输出写进父控制台：此时提示符已经打印，输出与提示符交错，程序结束后提示符也不会重绘，Ctrl+C 交给的是 shell 而不是程序。

这是 Windows 的设计，不是可以绕开的实现细节，所以没有为它做实验性实现。**这一条是推理，没有在 Windows 上复现。**

**方案 b（单个控制台子系统 exe + `FreeConsole`）不满足标准 1。** 双击控制台子系统程序时，系统先为它创建控制台窗口，然后才运行到程序的第一行；`FreeConsole` 只能让已经出现的窗口消失，窗口会闪一下。

**方案 c 满足全部三条**（待 CI 和人工确认）：

- `Watchink.exe` 以 `-H windowsgui` 链接，双击时系统不创建控制台。
- `watchink.com` 是普通的控制台程序，等待、输出、管道都由系统保证。
- MCP 客户端配置 `watchink.com mcp`，stdio 是普通的管道。

落地方式：

- 两个文件由同一个 `main` 包、同一组构建标签（`desktop,production`）构建，只差一个链接参数，`build_id` 相同。因此任意一个都能拉起或复用另一个启动的 Engine（§5.2）。
- 两个文件放在同一个文件夹时，输入 `watchink` 得到的是 `watchink.com`：`PATHEXT` 默认把 `.COM` 排在 `.EXE` 前面。Visual Studio 的 `devenv.com` / `devenv.exe` 用的是同一个办法。
- `watchink.com` 不带参数或执行 `desktop` 时同样打开窗口，控制台在窗口关闭前被占用，与在 macOS 终端里直接运行可执行文件一致。
- 桌面设置页里的 Agent 接入信息在 Windows 上指向同目录的 `watchink.com`（`internal/desktop/winshell.go`），命令行片段用 cmd 和 PowerShell 都认识的双引号。

代价：发行物从一个 exe 变成一个 zip，里面是两份各二十多 MB 的程序（各自内嵌了前端和 whisper-cli）；`.com` 扩展名少见，需要在说明里交代。

移植中发现的一个相关问题：cobra 在 Windows 上默认拒绝运行从资源管理器启动的程序（打印“这是命令行工具”后退出）。这会让双击 `Watchink.exe` 没有任何反应，已在 `internal/cli` 中关闭该行为。CI 从 PowerShell 启动窗口，覆盖不到这一点，需要人工双击确认。

## 实现要点

| 领域 | 做法 | 位置 |
| --- | --- | --- |
| 子进程（§5.5） | 每个子进程以 `CREATE_SUSPENDED \| CREATE_NO_WINDOW` 启动，放进一个新建的 `KILL_ON_JOB_CLOSE` Job Object 后再恢复运行，子进程派生的进程无法逃出 Job。取消即 `TerminateJobObject`（Windows 没有 SIGTERM，“先礼后兵”的两步是同一个调用）。Engine 持有唯一的 Job 句柄，Engine 无论怎样退出，系统都会关闭句柄并终止整棵进程树，不需要 `_guard` | `internal/procx/sys_windows.go` |
| 与 Unix 的差异 | `runner.go` 新增两个钩子：`attachGroup`（启动后建 Job）和 `releaseGroup`（结束后关句柄）；非 Windows 平台是空实现，macOS 行为不变 | `runner.go`、`group_other.go` |
| 拉起 Engine（§5.2） | `DETACHED_PROCESS \| CREATE_NEW_PROCESS_GROUP \| CREATE_NO_WINDOW`，并先尝试 `CREATE_BREAKAWAY_FROM_JOB`：终端和 MCP 宿主常把子进程关在随宿主一起终止的 Job 里，而 Engine 必须比客户端活得久（D-01）；宿主不允许脱离时退回普通方式 | 同上 |
| 残留进程（§7.6） | `QueryFullProcessImageName` 比对路径（不区分大小写）后 `TerminateProcess`；运行着当前程序自身的进程永不终止 | 同上 |
| 浏览器 / 资源管理器 | `rundll32 url.dll,FileProtocolHandler <url>`；`explorer.exe /select,"<路径>"`，命令行自行拼接，因为 explorer 不按通用规则解析引号 | 同上 |
| 物理核心数 | `GetLogicalProcessorInformation` 统计 `RelationProcessorCore`，失败时退回 `runtime.NumCPU()` | `internal/asr/cores_windows.go` |
| `engine.json` | Windows 不允许替换正被其它进程打开的文件，而客户端在 Engine 启动期间每 100 ms 读一次：替换失败时短暂重试 | `internal/engine/replace_windows.go` |
| 文件权限 | Windows 没有 0600；`engine.json`、`secrets.toml`、日志依靠用户配置目录的 ACL 保护。代码照常调用 `Chmod`（在 Windows 上无害），测试里的权限断言在 Windows 上跳过 | `engine`、`config` 的测试 |
| 工具安装 | 可执行文件名带 `.exe`：下载的工具由清单的 `bin` 指定，内嵌的 whisper-cli 按平台推导（`windows-amd64/whisper-cli.exe` 和 `whisper-cli.exe.sha256`）。`tools/bin` 下用硬链接代替符号链接（普通用户无权创建符号链接），不支持硬链接时复制。安装时的重命名在被占用时重试 2 秒，应对杀毒软件扫描刚写入的程序。`ERROR_DISK_FULL` 映射为 `DISK_FULL` | `internal/tools/sys_windows.go` 等 |
| 一个压缩包、两个工具 | BtbN 的 zip 同时包含 ffmpeg 和 ffprobe（194 MB）。第一个工具装完后把已校验的下载文件交给下一个需要它的工具，避免下载两次 | `internal/tools/install.go` 的 `releasePayload` |
| 桌面 | WebView 页面来源是 `http://wails.localhost`（Engine 的 CORS 白名单里原本就有）；WebView2 用户数据放在 `%LOCALAPPDATA%\Watchink\WebView2`；缺少 WebView2 运行时时，由 Wails 弹出中文提示，用户确认后下载并运行微软的引导安装程序（§16.3） | `internal/desktop` |
| `doctor` | 桌面检查项在 Windows 上报告 WebView2 运行时的版本；缺失时是警告而不是失败，因为 CLI 和 MCP 不依赖它 | `internal/cli/cmd_doctor.go` |
| whisper-cli | MSVC，`/MT` 静态运行时，仅 CPU，指令集基线为 AVX2 / FMA / F16C / BMI2。构建后嵌入 `activeCodePage=UTF-8` 清单：whisper-cli 以窄字符串接收参数，不加清单时，路径里只要有系统代码页之外的字符（例如中文用户名下的数据目录）就会失败 | `third_party/whisper/build.sh`、`whisper-cli.manifest` |
| 资源 | 图标（资源 ID 3，Wails 从这个 ID 加载窗口图标）、清单（DPI 感知 per-monitor v2、Common Controls 6、asInvoker）、版本信息，由构建脚本用 MinGW 的 `windres` 编译成 `.syso`，构建结束后删除。WebView2 用 Wails 默认的纯 Go 加载器，不随包携带 `WebView2Loader.dll` | `build/windows/`、`scripts/build-windows.sh` |

## 工具清单（`windows-amd64`）

| 工具 | 版本 | 来源 | sha256 |
| --- | --- | --- | --- |
| ffmpeg、ffprobe | 9.0.2（上游为 `n9.0.2-17-g2a571b6068`） | BtbN/FFmpeg-Builds `autobuild-2026-09-30-13-08`，`ffmpeg-n9.0.2-17-g2a571b6068-win64-gpl-9.0.zip` | `a0e45723…7a58d278` |
| yt-dlp | 2026.08.19 | 官方 `yt-dlp.exe` | `66674953…01dd3e7a` |
| deno | 2.9.7 | 官方 `deno-x86_64-pc-windows-msvc.zip` | `a0c3101b…786f2238` |

- 三个文件都在 2026-10-10 实际下载，本地计算的 sha256 分别与 BtbN 的 `checksums.sha256`、yt-dlp 的 `SHA2-256SUMS`、deno 的 `.sha256sum` 一致。
- BtbN 不提供恰好等于 `n9.0.2` 的保留版本：每日构建只保留约两周，月末构建长期保留。所以锁定的是 9 月的月末构建，它在 9.0 发布分支上比 9.0.2 多 17 个提交。清单的版本号是各平台共用的，仍写作 9.0.2。
- 清单里 ffmpeg 的 `license` 文字仍只提到 macOS 的上游构建者，各平台合并后需要改成通用的说法。

## 已验证（全部在 macOS 上）

| 命令 | 结果 |
| --- | --- |
| `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -tags nogui ./...` | 通过 |
| 同上环境的 `go vet -tags nogui ./...` 和 `go vet -tags desktop ./...` | 通过 |
| `… go build -tags desktop,production -ldflags "-H windowsgui" ./cmd/watchink` | 通过；PE 头的子系统字段为 2（GUI） |
| 同上但不带 `-H windowsgui` | 通过；子系统字段为 3（控制台） |
| `… go build -tags desktop,production ./cmd/_desktopsmoke` | 通过 |
| 每个包的 `GOOS=windows go test -c`，以及 `-tags desktop ./internal/desktop/` | 全部能编译（包括只在 Windows 上运行的 `procx` 测试） |
| `go build ./... && go test ./... && make lint`，以及带 `desktop` 标签的 `internal/desktop` 测试 | 通过，macOS 行为未变 |
| `bash scripts/build-windows.sh package`（交叉构建，含前端） | 生成 `dist/Watchink-0.1.0-dev-windows-amd64.zip`，内含两个各 22.6 MB 的程序；没有 whisper-cli，没有 `windres` 所以没有图标、清单和版本信息 |
| `go run scripts/make-ico.go` | 生成 9 个尺寸的 `.ico`，逐项检查了目录和位图头，并目视检查了 48 px 的图像 |
| `go run scripts/mcpsmoke.go bin/watchink`（macOS 的开发构建） | 通过；用来确认冒烟程序本身的逻辑 |
| CI 中 whisper 冒烟步骤的同一组参数，换成 macOS 的 whisper-cli | 通过：`ggml-tiny-q5_1`（sha256 实际核对）加 `jfk.wav`，放在中文目录下，输出中有预期的句子 |
| `bash -n`（两个脚本）、工作流 YAML 解析、清单 XML 解析 | 通过 |
| 与平台无关的单元测试：`windows-amd64` 清单条目、`.exe` 命名、共用压缩包只下载一次、Windows 引号规则、`watchink.com` 的查找、`doctor` 的 WebView 检查 | 通过 |

## 未验证

下面每一项都需要真实的 Windows。“CI”表示工作流里有对应的步骤，但工作流还没跑过。

| 项 | 现状 | 由谁验证 |
| --- | --- | --- |
| Job Object：启动、取消杀进程树、Engine 死亡后清理（S-08） | 能编译，逻辑未运行 | CI：`internal/procx` 的 Windows 测试；e2e 结束后检查无残留进程 |
| `CREATE_SUSPENDED` 后经线程快照恢复子进程 | 能编译，逻辑未运行。这一步一旦出错，所有外部工具都无法启动 | CI：同上 |
| 分离拉起 Engine、脱离宿主 Job、单例锁（S-09） | 能编译，逻辑未运行 | CI：cmd 步骤的冷启动、MCP 步骤；三客户端并发未覆盖 |
| S-02 标准 1：双击无控制台 | 推理 | CI 只检查 PE 子系统和“桌面进程没有 conhost 子进程”；**从资源管理器双击需要人工** |
| S-02 标准 2：cmd、PowerShell 5.1、PowerShell 7 | 推理 | CI：三个步骤分别检查退出码、完整 JSON、管道，以及不带扩展名时解析到 `.com` |
| S-02 标准 3：MCP stdio | 推理 | CI：`scripts/mcpsmoke.go`；**真实 MCP 客户端需要人工** |
| S-01：窗口、内嵌前端、绑定 | 只验证了能编译 | CI：启动 `Watchink.exe`，等它拉起 Engine（页面加载后才会调用绑定）；窗口外观和原生对话框需要人工 |
| S-04：`http://wails.localhost` 访问 `127.0.0.1`（CORS、私有网络预检、流式事件） | 未验证 | CI：在 runner 的真实 WebView2 里运行 `cmd/_desktopsmoke`（不含媒体播放检查） |
| S-05：WebView2 中的播放与跳转 | 未验证 | 人工 |
| S-06：whisper-cli 的 MSVC 构建、静态运行时、清单嵌入 | **构建脚本的 Windows 分支从未执行过**；`mt.exe` 的查找路径是按默认安装位置写的 | CI：构建，并在中文路径下做一次真实转写 |
| S-06：内嵌、释放、执行是否被 Defender 拦截；实时率和默认模型 | 未验证。默认模型暂为 `whisper-small-q5_1` | e2e 会在 runner 上走一遍释放和执行；用户机器上的 Defender / SmartScreen 需要人工 |
| S-07：按清单下载、校验、解压、冒烟、安装 | 清单内容已验证；安装流程在 Windows 上未运行 | CI e2e：`tools prepare` |
| 资源编译（`windres`）、图标、版本信息 | `.rc` 未编译过 | CI：检查两个文件的版本资源 |
| 全流程（导入 → 转写 → 笔记 → 导出） | 未验证 | CI e2e：真实 FFmpeg 和 whisper-cli、伪造在线模型，数据目录带空格和中文 |
| YouTube / Bilibili 下载，`yt-dlp.exe` + deno | 未验证 | 人工 |
| WebView2 缺失时的安装引导 | 未验证（Windows 11 自带运行时） | 在未安装运行时的 Windows 10 上人工确认 |
| 代码签名（§16.4） | 未实现，没有证书 | 待决 |
| `go test ./...` 在 Windows 上 | 未运行。已按阅读结果修改了本次涉及的包里明显依赖 Unix 的断言（权限位、`/` 开头的绝对路径、`HOME`）；其余包没有改动，可能还有失败 | CI：test 作业 |

## 写作本文时已知、尚未处理的问题

这些位于本次没有改动的包里，交由负责这些包的人处理：

- `internal/store/files.go` 的 `CommitFile` 以只读方式打开临时文件后调用 `Sync()`。Windows 的 `FlushFileBuffers` 要求句柄有写权限，会返回“拒绝访问”，于是**所有产物登记都会失败**。改成以读写方式打开即可。
- `internal/store/files.go` 的 `fileErr` 只认 `ENOSPC` / `EDQUOT`；Windows 的磁盘已满是 `ERROR_DISK_FULL`（112）和 `ERROR_HANDLE_DISK_FULL`（39）。
- 正在被读取的文件在 Windows 上不能被替换或删除：例如正在播放时重新生成 `playback.mp4`，或导出文件正被下载时再次导出。
- Windows 上默认的导入根目录 `~` 不包含其它盘符；这是策略问题，目前靠文档说明。

## 人工冒烟清单（Windows 11）

1. 解压 zip，从资源管理器双击 `Watchink.exe`：没有控制台闪现，窗口出现，SmartScreen 的提示与 `README.txt` 一致。
2. 在 cmd 和 PowerShell 中执行 `watchink jobs list --json`、`watchink doctor`，以及 `watchink jobs list --json | more`。
3. 导入一个本地视频并等待完成；处理中取消一次，确认任务管理器里没有残留的 ffmpeg / whisper-cli。
4. 处理中在任务管理器里结束 Engine 进程，确认子进程随之消失，重新连接后任务续跑。
5. 把 Agent 接入信息里的片段配置到一个真实的 MCP 客户端，调用 `list_videos`。
6. 在设置页选择文件、导出并“在资源管理器中显示”，播放视频并点击笔记里的时间引用。

## CI 首次运行结果（2026-10-10）

本文其余部分写于代码尚未在 Windows 上运行过的时候。之后 `.github/workflows/windows.yml` 在 GitHub 的 `windows-latest` runner 上运行并通过（提交 `453b197`），下列各项因此有了真机证据：

- whisper-cli 用 MSVC 静态运行时构建成功，并在含中文的路径下完成了一次真实转写。
- `go test ./...` 全部通过，包括 Job Object 进程管理的测试。
- 发行 zip 打包成功，内含 `Watchink.exe` 和 `watchink.com`。
- S-02 中可以自动化的部分：`watchink.com jobs list --json` 在 cmd 和 PowerShell 中输出完整并可接管道；`mcp` 能通过 stdio 应答 `initialize`。
- S-04：WebView2 中的页面能跨源访问本机 API（鉴权、预检、事件流）。
- 端到端流程通过。

仍然没有验证：从资源管理器双击启动（CI 只能从 PowerShell 启动）；窗口内的实际操作和视频播放；真实的 MCP 客户端；SmartScreen 和杀毒软件的反应；WebView2 缺失时的安装引导；YouTube / Bilibili 的真实下载；真实的在线模型；代码签名。

首次运行中发现并修复的问题：4 个 client 测试在 Engine 停止前就清理临时目录；两处测试写死了 Unix 路径或可执行位；桌面冒烟页假设了 Unix 路径；工作流里 Git Bash 的 curl 写不了非 ASCII 路径、`sha256sum` 对带反斜杠的路径会转义输出。
