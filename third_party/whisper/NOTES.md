# whisper-cli notes (macOS arm64)

Measured 2026-10-09 on an Apple M3 Pro (11 cores, macOS 27.0.1, Metal), with
whisper.cpp v1.9.5 (commit d1be6fde) built by `build.sh darwin arm64`, models
`ggml-large-v3-turbo-q5_0.bin`, `ggml-small-q5_1.bin`, VAD `ggml-silero-v6.2.0.bin`.
One synthetic sample only (macOS `say`, voice Tingting): 256.3 s of Mandarin,
16 kHz mono s16le WAV, built as 118.28 s speech + 20 s digital silence + 118.03 s
speech; plus a 29.7 s English clip (voice Samantha). Not yet measured on real
lecture audio or on Windows/Linux.

## Build

- Only Xcode Command Line Tools are needed. `GGML_METAL_EMBED_LIBRARY=ON` embeds
  the shader *source*; it is compiled at runtime, so no `metal` compiler.
- `otool -L`: Accelerate, Foundation, Metal, MetalKit, CoreFoundation, libSystem,
  libc++, libobjc. Nothing else. Size 7.8 MB, ad-hoc signed.
- The binary is not byte-reproducible across checkout paths (the sha256 changed
  when the repo directory was renamed); it is stable for repeated builds in place.
- The very first run on a machine compiles the Metal libraries (4.8 s here);
  the OS caches them and later runs load them in ~0.01 s.
- Extracted from the Go embed and run from the data dir with Gatekeeper enabled
  (`spctl --status`: assessments enabled): not blocked. Files written by the
  program carry no `com.apple.quarantine` attribute.

## Flags that work in this version

```
whisper-cli -m <model> -f <audio.wav> -l <zh|en|auto> -t <threads> -oj -of <out-without-ext> -pp \
  [--prompt "<text>"] [--vad --vad-model <ggml-silero-v6.2.0.bin>]
```

- `-oj` writes `<out>.json`; `-ojf` adds a `tokens` array per segment.
- `-of` takes the path **without** extension.
- `-np` silences all stderr logging **including** the progress and the
  language line; do not combine it with `-pp` or `-dl`.
- `--version` prints `whisper.cpp version: 1.9.5`; `--help` exits 0.
- Exit codes seen: 0 ok, 2 audio file unreadable, 3 model failed to load.
- Other VAD flags: `-vt` threshold (0.50), `-vspd` min speech ms (250), `-vsd`
  min silence ms (100), `-vmsd` max speech s, `-vp` pad ms (30), `-vo` overlap s (0.10).

### Language detection

`whisper-cli -m <model> -f <audio> -l auto -dl [-oj -of <out>]` exits 0 after
looking at the first 30 s. stdout is empty. The result is

- on **stderr**: `whisper_full_with_state: auto-detected language: zh (p = 0.975601)`
- with `-oj`, in `<out>.json` at `.result.language` (`"transcription": []`).

A full run with `-l auto` prints the same stderr line and also fills
`.result.language`. Detection took 0.9 s (small) on a warm machine.

### Progress (`-pp`), on stderr

```
whisper_print_progress_callback: progress =  11%
whisper_print_progress_callback: progress =  22%
whisper_print_progress_callback: progress = 100%
```

Regex: `progress\s*=\s*(\d+)%`. Steps are irregular (about one line per 30 s
window; a 30 s clip printed only `92%` and `100%`). Transcript lines go to
stdout as `[00:00:00.030 --> 00:00:22.860]  text`.

### JSON output (`-oj`)

Top-level keys: `systeminfo`, `model`, `params`, `result`, `transcription`.

```json
{
	"params": {"model": "/…/ggml-large-v3-turbo-q5_0.bin", "language": "zh", "translate": false},
	"result": {"language": "zh"},
	"transcription": [
		{
			"timestamps": {"from": "00:00:00,030", "to": "00:00:22,860"},
			"offsets": {"from": 30, "to": 22860},
			"text": "同学们好,欢迎来到操作系统课程的第三讲,…"
		}
	]
}
```

- `offsets.from` / `offsets.to` are integer **milliseconds**; use them, not the
  `timestamps` strings (comma before the milliseconds).
- English `text` starts with a space; trim it.
- In all runs offsets were monotonic and `from < to`. Adjacent segments may
  share a boundary or leave a small gap.
- Segments are long: up to 40 s / 135 characters with large-v3-turbo. `-ml N`
  (optionally `-sow`) splits them, at the cost of sentence boundaries.
- `-ojf` token: `{"text","timestamps","offsets","id","p","t_dtw"}`; the first
  token of a segment is `[_BEG_]`.

With `--vad`, stderr also lists the detected speech in original-timeline seconds:

```
whisper_vad_segments_from_probs: VAD segment 26: start = 138.34, end = 140.61 (duration: 2.27)
whisper_vad: Reduced audio from 4101007 to 3777120 samples (7.9% reduction)
```

## Speed (256.3 s Mandarin, `-t 8`, `-l zh` with the zh prompt, Metal)

| model | VAD | wall | RTF | speed |
| --- | --- | --- | --- | --- |
| small-q5_1 | off | 9.96 s | 0.039 | 25.7x |
| small-q5_1 | on | 9.01 s | 0.035 | 28.5x |
| large-v3-turbo-q5_0 | off | 14.30 s | 0.056 | 17.9x |
| large-v3-turbo-q5_0 | on | 13.92 s | 0.054 | 18.4x |

Each number is a single run, model load included (~0.15–0.3 s). VAD itself took
0.5 s. CPU-only (`-ng`) small on the 30 s clip: 3.0 s vs 1.2 s with Metal.

## Quality seen on the sample

- large-v3-turbo, VAD off: complete, no hallucination across the 20 s silence.
  Punctuation is half-width (`,`) despite the prompt. A few homophone errors
  (队列→对列, 锁→索).
- small, VAD off: more errors (线程→现成, 响应→想应, 护航→互行) and a garbled,
  partly invented segment across the silence (120.4–141.4 s); the opening
  sentence of the second part was lost.
- small, VAD on: no garbage at the silence, but 22 s of speech (36.3–58.7 s)
  is missing from the output.
- large-v3-turbo, VAD on: two characters dropped at one boundary (“人们”).
- English, large-v3-turbo, no prompt: lowercase text with no
  punctuation at all. With `--prompt "Hello, welcome to the lecture. Today, we
  will discuss the topic."` casing and punctuation were normal. small punctuated
  English correctly without a prompt.

## VAD time mapping

Timestamps come back on the original timeline: the last segment ended at
255.96 s (VAD) vs 256.02 s (no VAD), and boundaries after the silence agree
within ~0.15 s between the two runs (e.g. 202.87 vs 203.12, 216.06 vs 216.06).

One defect: the first segment after a removed silence starts at the **beginning**
of the silence, not at the speech. The second part truly starts at 138.28 s
(VAD segment 26: 138.34). Reported segment start:

| run | start of “下面我们进入第二部分…” |
| --- | --- |
| large, VAD off | 137.62 s (correct) |
| large, VAD on | 118.06 s (20.2 s early) |
| small, VAD on | 118.17 s (20.2 s early) |

Token offsets inside that segment (`-ojf`) are shifted the same way (118–120 s
for the first words), so they cannot be used to repair it. The error equals the
length of the removed silence, so it is small for ordinary pauses and large
after long ones. A parser could repair it from the `VAD segment` stderr lines:
when a segment start lies within ~0.3 s of the end of VAD segment N and segment
N+1 starts more than ~1 s later, move the start to the start of N+1. This was
not implemented or tested.

## Recommendation for darwin-arm64

- Default model: `whisper-large-v3-turbo-q5_0` (18x real time here; clearly
  better text than small, which is only 1.4x faster).
  `tools.Manager.DefaultASRModel()` returns it for darwin-arm64.
- VAD default: **off**. The requirement “time mapping correct under VAD” is
  not met as-is (segment starts after long silences), VAD dropped content in
  both models on this sample, and the large model did not need it to get
  through 20 s of silence. Speed gain is ~3%. Revisit with real lecture audio
  and the stderr-based repair above.
- Consider an English prompt for large-v3-turbo; the engine sent none when this was measured.
