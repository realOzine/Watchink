<p align="center"><img src="build/appicon.png" width="112" alt="Watchink icon"></p>

# Watchink

[简体中文](README.md) · **English**

[![macOS](https://github.com/realOzine/Watchink/actions/workflows/macos.yml/badge.svg)](https://github.com/realOzine/Watchink/actions/workflows/macos.yml)
[![Linux](https://github.com/realOzine/Watchink/actions/workflows/linux.yml/badge.svg)](https://github.com/realOzine/Watchink/actions/workflows/linux.yml)
[![Windows](https://github.com/realOzine/Watchink/actions/workflows/windows.yml/badge.svg)](https://github.com/realOzine/Watchink/actions/workflows/windows.yml)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

Turn lecture videos into notes with time citations. Every point in a note links back to the second it came from.

One file to download, nothing else to install.

> The interface and the generated notes are in Simplified Chinese in v0. Lectures in Chinese and English are supported as input.

## Install

**No Go, Node, Python, Docker or database to install.** One file per platform: download it and run it. FFmpeg, yt-dlp and the speech model are downloaded and checksum-verified the first time they are needed.

| Platform | Download | Run |
| --- | --- | --- |
| macOS (Apple silicon) | `Watchink-<version>-macos-arm64.zip` | Unzip, open `Watchink.app` |
| Windows (x64) | `Watchink-<version>-windows-amd64.zip` | Unzip, double-click `Watchink.exe` |
| Linux (x64) | `watchink-<version>-linux-amd64` | `chmod +x`, then run |
| Linux server / container | `watchink-headless-<version>-linux-amd64` | Same; no system dependencies, no desktop window |

**[Download the latest release →](https://github.com/realOzine/Watchink/releases/latest)**  Verify the download with `SHA256SUMS` from the same page.

One thing to know per platform:

- **macOS**: the app is not signed by Apple. If the first launch is blocked, go to System Settings → Privacy & Security and choose "Open Anyway".
- **Windows**: the desktop window needs WebView2 (included in Windows 11; you are prompted to install it when missing). If SmartScreen blocks the app, choose "More info → Run anyway".
- **Linux**: the desktop build needs WebKitGTK. On Ubuntu 24.04:
  `sudo apt install libwebkit2gtk-4.1-0 libgtk-3-0t64 gstreamer1.0-plugins-good gstreamer1.0-libav`.
  Or use the headless build with the browser UI.

Run `watchink doctor` afterwards to check the environment.

## Three steps

1. **Open the app.** The first-run wizard walks you through downloading the transcription tools and the speech model (about 200–600 MB depending on the platform) with one click.
2. **Enter an online model.** Any OpenAI-compatible service works: base URL, model name and API key. It is used only to turn the transcript into notes.
3. **Import a video.** Pick a local file, or paste a link to a single YouTube or Bilibili video.

When processing finishes, the video is on the left and the note on the right. Click a time in the note and the video jumps there.

## Features

- **Citations you can check.** The model only cites transcript segment numbers; times and links are computed and verified by the program, so there are no invented timestamps.
- **Local transcription.** whisper.cpp, with Metal acceleration on macOS.
- **Long videos are not truncated.** Processed in chunks and then merged; every valid point ends up in the note.
- **Resumable.** After a cancel, a failure, or even a killed process, a job continues where it stopped; finished downloads and transcriptions are not repeated.
- **Four ways in, one library.** Desktop window, browser, command line and MCP all see the same data.
- **Export.** Markdown, SRT, VTT, TXT, JSON.

## Command line

The same executable is the command-line tool. On macOS it is `Watchink.app/Contents/MacOS/Watchink`; on Windows use `watchink.com` from the zip.

```sh
# First-time setup: the online model, plus tools and the speech model
printf '%s' "$API_KEY" | watchink setup \
  --llm-base-url https://api.example.com/v1 --llm-model <model> \
  --llm-api-key-stdin --prepare

watchink import ./lecture.mp4 --wait     # or a video link
watchink note get <video_id>
watchink export <video_id> --format md --out ./
watchink web                             # use it in the browser
```

Every command accepts `--json` and has stable exit codes, so it scripts well.

## For agents

Paste the text below into your agent (Claude Code, Cursor, or any agent that can run commands). It downloads Watchink and connects the MCP server by itself:

```text
Please install Watchink and connect its MCP server to yourself.

1. Open https://github.com/realOzine/Watchink/releases/latest and download the file for my system:
   - macOS (Apple silicon): Watchink-<version>-macos-arm64.zip; unzip it and put Watchink.app in /Applications.
     The executable is /Applications/Watchink.app/Contents/MacOS/Watchink
   - Windows (x64): Watchink-<version>-windows-amd64.zip; unzip it to %LOCALAPPDATA%\Programs\Watchink.
     The executable is watchink.com inside it
   - Linux (x64): watchink-headless-<version>-linux-amd64 when there is no graphical session or
     WebKitGTK is not installed, otherwise watchink-<version>-linux-amd64; save it as
     ~/.local/bin/watchink and make it executable
2. Verify the download against SHA256SUMS from the same page.
3. Run `<executable> doctor` and tell me the result.
4. Add Watchink to your MCP configuration: name watchink, transport stdio,
   command = the absolute path of the executable above, argument = mcp.
5. Confirm that you can see the tools import_video, get_job, get_note and get_transcript,
   and tell me whether a restart is needed.

Generating notes needs an OpenAI-compatible online model. Ask me for the base URL, the model
name and the API key; do not make them up. Then run
`<executable> setup --llm-base-url <url> --llm-model <model> --llm-api-key-stdin --prepare`,
passing the API key on standard input, never on the command line or in a file.
```

Once connected, you can ask things like:

- Import this video into Watchink and make notes: `https://www.youtube.com/watch?v=…`
- List the videos in Watchink.
- Read the note of "<video title>", give me three takeaways with their timestamps.
- Find where "<keyword>" is explained in the transcript of "<video title>" and tell me the time.
- Export the note of "<video title>" as Markdown.

If Watchink is already installed, only steps 4 and 5 are needed; the desktop app has a version with the path filled in under Settings → Agent. Notes are generated by the online model you configured, not by the agent's own model. (Notes are written in Simplified Chinese in v0.)

## Privacy

- Only the **transcript text, the video title and its duration** are sent to the online model. Audio and video are never uploaded.
- The program listens on the loopback address only and collects no usage data.
- The API key is stored in a local file with mode 0600 and is kept out of logs and exports.

## Status

Early release (v0.0.1).

| Platform | Automated checks | Checked by hand |
| --- | --- | --- |
| macOS arm64 | Build, tests, packaging | Full flow, including one YouTube and one Bilibili video |
| Linux amd64 | Build, tests, end to end | Not yet |
| Windows amd64 | Build, tests, end to end | Not yet |

Not verified yet: note quality with a real online model (the automated tests use a fake one), and using the window and playback on Linux and Windows. v0 has no search, no visual understanding and no note editing; see the [roadmap](docs/roadmap.md).

## Documentation

The documents below are written in Chinese.

- [Building from source](docs/building.md)
- [Architecture and code guide](docs/architecture.md)
- [Benchmarks](docs/benchmark.md)
- [v0 design](docs/v0.md) · [Roadmap](docs/roadmap.md)
- Platform verification records: [macOS](docs/spikes/v0-p0.md) · [Linux](docs/spikes/v0-linux.md) · [Windows](docs/spikes/v0-windows.md)

## License

[MIT](LICENSE). The third-party tools downloaded at run time (FFmpeg, yt-dlp, Deno) and the Whisper models keep their upstream licenses; this project does not redistribute them.
