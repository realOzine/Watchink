# v0 Linux（amd64）移植记录

记录日期：2026-10-10。

**一句话结论：Linux 的代码、构建脚本和 CI 已经写完，但没有任何一行在 Linux 上运行过。** 开发机是 macOS arm64，没有 Docker，也没有 Linux 机器。本文把“在 macOS 上实际验证过的”和“只能等 CI 或真机验证的”分开写，后者不应被当作已通过。

第一次真正的验证是 `.github/workflows/linux.yml` 在 Ubuntu 24.04 runner 上的运行。它跑完之前，S-01、S-03、S-05、S-06、S-07、S-08 在 Linux 上的状态都是“未执行”。

## 做了什么

| 部分 | 内容 |
| --- | --- |
| `internal/infra/procx` | `sys_linux.go`：新进程组加 `Pdeathsig=SIGKILL`；按 `/proc/<pid>/exe` 核对残留子进程；`xdg-open` 打开浏览器；`org.freedesktop.FileManager1.ShowItems`（`dbus-send`，其次 `gdbus`）定位文件，都不可用时用 `xdg-open` 打开所在目录。`sys_unix.go` 是从 `sys_darwin.go` 原样移出的公共部分（进程组信号、`Setsid` 分离启动、参数检查） |
| `internal/capability/asr` | `cores_linux.go`：从 `/sys/devices/system/cpu/cpu*/topology` 数不同的 `(physical_package_id, core_id)`，读不到时退回逻辑核数 |
| `internal/capability/tools` | 清单增加四个 `linux-amd64` 条目；新增纯 Go 的 xz / LZMA2 解码器 `xz.go`（标准库没有 xz，D-16 不允许新增依赖），`extract.go` 支持 `tar.xz` |
| `third_party/whisper/build.sh` | `linux amd64`：静态链接、仅 CPU、x86-64-v3 指令集基线 |
| 桌面 | `scripts/build-linux.sh`、Makefile 的 `desktop-linux` / `package-linux`；Wails 的 Linux 选项（窗口图标、程序名、GPU 策略）；没有图形会话时 `desktop` 给出说明而不是崩溃 |
| `watchink doctor` | 新包 `internal/infra/sysreq` 按文件查找 §16.3 的四个系统库，缺少时给出 apt 包名 |
| CI | `.github/workflows/linux.yml`，以及它调用的 `scripts/smoke-linux.sh`（S-03）和 `scripts/e2e.sh`（工具下载、全流程、S-08） |

## 在 macOS 上验证过的

| 项 | 命令 | 结果 |
| --- | --- | --- |
| Linux 交叉编译（无 cgo） | `GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -tags nogui ./...` | 通过 |
| Linux 静态检查（含测试文件） | `GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go vet -tags nogui ./...` | 通过 |
| Linux 专属测试能编译 | `GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go test -c ./internal/{procx,asr,tools,sysreq}/` | 通过（只编译，未运行） |
| 桌面代码对 Linux 的类型检查 | `GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go vet -tags desktop,webkit2_41 ./internal/entry/desktop/ ./cmd/watchink/` | 通过。不带 `production` 标签时 Wails 不引入 cgo 前端，所以这只说明我们自己的代码对得上 Wails 的 API |
| macOS 没有被改坏 | `go build ./... && go test ./... && make lint`；`go test -race -count=2 ./internal/infra/procx/`；`go build -tags desktop,production ./cmd/watchink` | 全部通过；桌面构建的 `doctor` 输出与之前相同 |
| xz 解码器对真实文件 | 解出清单里的 FFmpeg 压缩包（151 MB → 539 MB，22 个块，CRC64） | sha256 与 `xz -dc` 的输出一致；用时 10.5 s。经 `extractFiles` 取出的 `bin/ffprobe` 与 `tar -xJ` 取出的 sha256 一致 |
| xz 解码器的边界情况 | `go test ./internal/capability/tools/ -run 'XZ\|TarXz'` | 通过。样例由 XZ Utils 5.8.3 生成：多个 LZMA2 分块、4 KiB 字典加非默认 `lc/lp/pb`、未压缩分块、多块多流加流填充、空流、CRC32 / CRC64 / SHA-256 / 无校验。逐字节破坏和截断都得到错误而不是崩溃或错误输出 |
| 清单条目 | 下载每个文件、计算 sha256、列出压缩包内容 | 见下表 |
| 物理核数解析 | `cores_linux.go` 的测试临时去掉构建标签后在 macOS 上运行 | 通过（超线程、双路、离线 CPU、缺少 package id） |
| `doctor` 的系统库检查 | `go test ./internal/infra/sysreq/ ./internal/entry/cli/` | 通过（查找逻辑与平台无关，用临时目录测试） |
| 冒烟脚本的流程 | 用 macOS 的 nogui 构建运行 `scripts/smoke-linux.sh` | 通过：`serve`、CLI、`engine stop`、CLI 拉起分离的后台 Engine、MCP 的 initialize / tools/list / tools/call |
| 端到端脚本的流程 | 用 macOS 的 nogui 构建和已安装的 macOS 工具运行 `scripts/e2e.sh` | 通过：导入、转写、笔记、导出、取消、`kill -9` Engine 后子进程消失、重启续跑。这验证的是脚本本身和 macOS 的行为，不是 Linux 的行为 |
| 脚本与工作流语法 | `bash -n` 四个脚本；用 Ruby 的 YAML 解析 `linux.yml` | 通过。没有 shellcheck 和 actionlint |

### 清单条目（2026-10-10 下载并计算）

| 工具 | 版本 | 文件 | 大小 | sha256 |
| --- | --- | --- | --- | --- |
| ffmpeg、ffprobe | 9.0.2（上游 `n9.0.2-17-g2a571b6068`） | BtbN/FFmpeg-Builds `autobuild-2026-09-30-13-08` 的 `ffmpeg-n9.0.2-17-g2a571b6068-linux64-gpl-9.0.tar.xz` | 151017172 | `68ee646831adaae2495618346f3bba94ff207ff83bbd34d643e7004730d66269` |
| yt-dlp | 2026.08.19 | 官方 `yt-dlp_linux` | 40446224 | `58162f9bfdc27458ea47bfcb311cf47028f17d8154a8bf7d689861d46399230a` |
| deno | 2.9.7 | 官方 `deno-x86_64-unknown-linux-gnu.zip` | 41596794 | `c6527f24f4b16031d3ae4fa9f658d5f11534c8d84ce7dc8502420280919c3490` |

- 三个 sha256 都与发布方的校验文件一致（BtbN 的 `checksums.sha256`、yt-dlp 的 `SHA2-256SUMS`、deno 的 `.sha256sum`）。
- 压缩包内容与 `files` 映射一致：FFmpeg 包里是 `ffmpeg-n9.0.2-17-g2a571b6068-linux64-gpl-9.0/bin/{ffmpeg,ffprobe,ffplay}`；deno 的 zip 里只有 `deno`。取出的文件都是 x86-64 ELF。
- 版本与 macOS 条目相同。BtbN 没有“9.0.2 正式版”的构建，只有 9.0 发布分支的每日快照，这里用的是 9.0.2 之后第 17 个提交。
- **BtbN 的每日构建只保留约两周，每月最后一次构建长期保留**（发布列表里能看到 2024 年以来每个月末的标签）。所以锁定的是 9 月 30 日这个月末标签，而不是更新的每日标签。
- 这些二进制没有被执行过。

## 没有验证的

每一项都标了状态：**已对 Linux 编译**（类型检查通过，逻辑未运行）、**未编译**（连编译都没做）、**逻辑未测**（与平台无关的代码路径在 Linux 上的行为未知）。

| 项 | 状态 | 由谁来验证 |
| --- | --- | --- |
| `procx` 在 Linux 上的全部行为：取消杀进程树、`Pdeathsig`、`KillIfMatches`、分离启动 | 已对 Linux 编译 | CI 的 `go test`（`runner_unix_test.go`、`runner_linux_test.go`）和 `e2e.sh` |
| `xdg-open` 打开浏览器、D-Bus 定位文件 | 已对 Linux 编译；只有参数拼装有单元测试 | 需要人在真实桌面上操作 |
| 物理核数读取真实的 sysfs | 已对 Linux 编译 | CI 的 `go test` |
| 桌面 ELF 的 cgo 构建（`-tags desktop,production,webkit2_41`） | **未编译**。macOS 上没有 Linux 的 C 工具链和 GTK / WebKitGTK 开发文件 | CI 的构建步骤 |
| whisper-cli 的 Linux 构建 | **未编译**。`build.sh` 的 Linux 分支只做过 `bash -n` 和“非 Linux 主机拒绝运行”的检查；CMake 选项名对照过固定版本的源码 | CI 的 whisper 步骤 |
| `scripts/build-linux.sh` | 只做过 `bash -n` | CI |
| 四个下载的工具能否在 Ubuntu 24.04 上运行 | 逻辑未测 | `e2e.sh` 的 `tools prepare` |
| 没有图形会话时的行为（S-03） | 逻辑未测；脚本流程在 macOS 上跑通 | CI 的两个冒烟步骤 |
| 窗口能否打开并加载前端（S-01） | 未编译 | CI 的 Xvfb 步骤只能说明“窗口起来了、前端调用了绑定”，不拦截构建；窗口外观需要人看 |
| 窗口内视频播放（S-05、D-11） | 未编译 | 需要人在装好 GStreamer 插件的桌面上操作 |
| CPU 转写速度与默认模型（S-06） | 未测。Linux 沿用占位的 `whisper-small-q5_1` | 需要实测后决定 |
| `go test ./...` 中其余包在 Linux 上是否通过 | 已对 Linux 编译 | CI |
| `.github/workflows/linux.yml` 本身 | 只做过 YAML 解析；用到的 action 版本标签（`checkout@v5`、`setup-go@v6`、`setup-node@v5`、`upload-artifact@v6`、`download-artifact@v7`）确认存在且运行在 node24 上 | 第一次运行 |

## 决定与偏离

- **不新增依赖，自己写 xz 解码器。** BtbN 的 Linux 构建只有 `tar.xz`，标准库没有 xz。解码器连注释约 870 行，只支持 LZMA2 过滤器（`xz` 和 `tar -J` 的默认输出），遇到 BCJ / delta 过滤器链会报错。它只处理已经通过 sha256 校验的文件。
- **ffmpeg 和 ffprobe 各下载一次同一个压缩包**（共约 300 MB）。清单以工具为单位，两个条目指向同一个 URL；合并下载需要改安装流程，这次没有动。解压时只解到目标文件为止。
- **Pdeathsig 只作用于直接子进程。** Engine 被强杀时，内核杀掉进程组的组长（工具本身）；工具自己启动的后代（yt-dlp 调用的 ffmpeg / deno）收不到这个信号，而重启恢复按组长的 pid 核对，此时组长已经不在。正常取消不受影响（信号发给整个进程组）。另外 `yt-dlp_linux` 是 PyInstaller 单文件程序，真正的程序是引导进程的子进程；引导进程被 SIGKILL 后它是否退出没有查证。如果实测有残留，最简单的修法是让 Linux 也使用 `_guard`（`GuardMain` 的实现本身不依赖 macOS）。规格 §5.5 写的是 Pdeathsig，这里按规格实现。
- **Pdeathsig 绑定的是执行 fork 的线程**，不是进程。Go 运行时只在“锁定了线程的 goroutine 退出”时才结束线程，`Runner.Run` 不会在这种 goroutine 里调用，所以不做额外处理；`sys_linux.go` 里有说明。
- **`KillIfMatches` 在 Linux 上从不杀运行着 watchink 自身的进程**（macOS 上只杀其中的 `_guard`）。Linux 不用 guard，登记的是工具自己的路径。工具升级后旧文件被替换，`/proc/<pid>/exe` 会带 ` (deleted)` 后缀，这种情况仍然算匹配。
- **`xdg-open` 不等待结束。** 它可能一直占着前台直到浏览器退出，所以只观察 2 秒：这期间以错误退出才算失败。
- **whisper-cli 的指令集基线是 x86-64-v3**（AVX2、FMA、F16C、BMI2，大约 2013 年以后的 Intel 和 2015 年以后的 AMD）。更老的处理器会收到非法指令信号。完全静态链接（含 glibc），不依赖构建机的 glibc 版本。
- **桌面 ELF 在 Ubuntu 24.04 上构建**，所以需要 24.04 或更新的 glibc。规格 §16.2 写的是用 22.04 的 runner 以兼容更老的系统；22.04 也有 `libwebkit2gtk-4.1-dev`，把 `runs-on` 改掉即可，但那样 §16.3 的包名（`libgtk-3-0t64`）要相应调整。这次按任务要求用 24.04。
- **WebKitGTK 的 GPU 合成保持关闭**（`WebviewGpuPolicyNever`）。这是 Wails 在不传 Linux 选项时的默认值，原因是部分驱动下窗口空白；我们传了选项（图标、程序名），所以要显式写出来。
- **只提供 amd64 的无界面构建。** 规格 §16.2 还列了 linux-arm64；纯 Go 部分可以交叉编译，但 arm64 的 whisper-cli 没有构建，嵌入会缺失。
- **`doctor` 的检查是按文件名在库目录里找**（`LD_LIBRARY_PATH`、Debian 多架构目录、`lib64` / `lib`；GStreamer 插件另查 `GST_PLUGIN_PATH`），不启动子进程，也不解析 `ld.so.cache`。缺 WebKitGTK 或 GTK 记为失败，缺 GStreamer 插件记为警告。桌面 ELF 动态链接前两者，缺少时程序根本起不来，所以“失败”这一档实际只会在非标准安装位置下出现。
- **容器里没有 init 进程回收僵尸时**，被杀掉的孙进程会一直留在进程组里，`groupAlive` 会认为组还活着，取消会等满 3 秒的宽限期才返回。只影响带孙进程的工具，没有处理。

## 合并时要注意

- `internal/infra/procx/runner_darwin_test.go` 被拆成两个文件：公共用例移到 `runner_unix_test.go`，只有 guard 相关的四个用例和 `TestMain` 留在原文件。用例内容只改了一处：`TestKillIfMatches` 里不匹配的可执行文件从 `/bin/ls` 换成 `/bin/sh`。
- `Makefile` 和 `scripts/build-macos.sh` 开头、`sys_other.go`、`cores_other.go` 里的 `TODO(linux)` 注释没有动，以免和 Windows 分支冲突；它们现在已经过时。`README.md` 的状态行和 `docs/architecture.md` 同理。
- `manifest.json` 里 ffmpeg / ffprobe 的 `license` 字段仍然只提到 macOS 的构建来源。
- 新增的包 `internal/infra/sysreq` 和 `build/linux`（只含嵌入的图标）不在 `docs/architecture.md` 的包列表里。

## CI 首次运行结果（2026-10-10）

本文其余部分写于代码尚未在 Linux 上运行过的时候。之后 `.github/workflows/linux.yml` 在 GitHub 的 `ubuntu-24.04` runner 上运行并通过（提交 `453b197`），下列各项因此有了真机证据：

- whisper-cli 静态构建成功，产物无动态依赖，能运行。
- `go test ./...` 全部通过，包括进程组、`Pdeathsig` 和 `/proc/<pid>/exe` 相关的测试。
- 带 `desktop,production,webkit2_41` 标签的 cgo 桌面程序编译成功，链接的是 WebKitGTK 4.1。
- 无显示环境下 `serve`、CLI、`mcp` 可用（S-03）。
- 工具清单里的 ffmpeg、ffprobe、yt-dlp、deno 真实下载、校验、解压并通过冒烟测试；自带的 xz 解码器解开了真实的 ffmpeg 包。
- 端到端：导入本地视频 → CPU 转写 → 生成笔记（假模型）→ 导出，以及脚本里的取消与清理检查。

仍然没有验证：窗口的实际显示、交互和视频播放（Xvfb 下的窗口检查只是参考，不作为门槛）；`xdg-open` 和文件管理器定位；YouTube / Bilibili 的真实下载；真实的在线模型；Ubuntu 24.04 以外的发行版。

首次运行中发现并修复的问题：转写进度会被 stdout 上的字幕正文误触发（只在 Linux 的流到达顺序下暴露，三个平台都受影响）；端到端脚本没有显式安装 yt-dlp、没有把临时目录加入允许导入的范围。
