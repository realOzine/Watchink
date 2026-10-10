Watchink __VERSION__ (Windows x64)
==================================

Watchink.exe   桌面应用。双击打开窗口。
watchink.com   命令行程序，与 Watchink.exe 是同一份代码。在 cmd 或
               PowerShell 中使用，也用于 MCP 客户端的配置。

两个文件请放在同一个文件夹里。把这个文件夹加入 PATH 后，直接输入
"watchink" 运行的是 watchink.com。

    watchink doctor
    watchink import D:\videos\lecture.mp4 --wait
    watchink mcp

系统要求
--------
- Windows 10 1903 或更高版本，64 位；在 Windows 11 上验证。
- WebView2 运行时（仅桌面窗口需要）。Windows 11 自带；缺少时
  Watchink.exe 会提示下载安装。

首次运行
--------
本程序没有代码签名。Windows 可能显示“Windows 已保护你的电脑”
（SmartScreen）：点击“更多信息”，再点击“仍要运行”。

FFmpeg、yt-dlp 和语音模型在首次使用时下载到
%LOCALAPPDATA%\Watchink\Data，并校验 sha256。安全软件可能会扫描或拦截
这些新下载的程序；被拦截时请把该文件夹加入排除项后重试。

默认只允许从用户文件夹导入本地视频。视频在其它盘时，先把所在文件夹
加入允许列表（逗号分隔，~ 表示用户文件夹）：

    watchink config set security.import_roots "~,D:\videos"

在 Windows PowerShell 5.1 中用管道处理中文输出前，先执行：

    [Console]::OutputEncoding = [Text.Encoding]::UTF8
