package tools

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/iotest"
)

// The fixtures in testdata/xz were written by XZ Utils 5.8.3 (`xz -c`), so the
// decoder is checked against the reference encoder, not against itself:
//
//	chunks-crc64.xz       -6; 2.6 MB of compressible text: several LZMA2
//	                      chunks sharing one dictionary, CRC64 check
//	dict4k-lp2-sha256.xz  --check=sha256 --lzma2=preset=6,dict=4KiB,lc=0,lp=2,pb=0
//	stored-crc32.xz       -0 --check=crc32; random bytes, stored uncompressed
//	multi-stream.xz       a --check=none --block-size=65536 stream (3 blocks),
//	                      8 bytes of stream padding, then stored-crc32.xz
//	empty.xz              the empty input: a stream without blocks
//	pkg.tar.xz            a tar laid out like the FFmpeg builds (pkg-1.0/bin/…)
var xzFixtures = []struct {
	name   string
	size   int64
	sha256 string
}{
	{"chunks-crc64.xz", 2656465, "2cd2d7c423858f7c9f395db8325d4947ec91b5154b47b61a88e57bf00491689a"},
	{"dict4k-lp2-sha256.xz", 341300, "1b45ee09152b300aa4fd202602be36690140d1b100d3e296b87e2e4499223de5"},
	{"empty.xz", 0, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
	{"multi-stream.xz", 157475, "1c705773607d3135224aaa5182e37e3b0e49440d9ea74ce91b93731fd90d6457"},
	{"pkg.tar.xz", 122880, "8debd12079f12f6804de39310f46afc69ab8ffbf4902c25145332a4bc5437dcb"},
	{"stored-crc32.xz", 6000, "5b34ff6197d3ba0fdb026ae2a039ef387bcceed7be927c354d959e5917c34a3a"},
}

func readXZFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "xz", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestXZDecode(t *testing.T) {
	for _, fx := range xzFixtures {
		t.Run(fx.name, func(t *testing.T) {
			packed := readXZFixture(t, fx.name)
			// Once in one go, once through a reader that returns a byte at a
			// time, which exercises every refill path of the container parser.
			for _, src := range []io.Reader{bytes.NewReader(packed), iotest.OneByteReader(bytes.NewReader(packed))} {
				h := sha256.New()
				n, err := io.Copy(h, newXZReader(src))
				if err != nil {
					t.Fatalf("decode: %v", err)
				}
				if got := hex.EncodeToString(h.Sum(nil)); n != fx.size || got != fx.sha256 {
					t.Errorf("decoded %d bytes with sha256 %s, want %d / %s", n, got, fx.size, fx.sha256)
				}
			}
		})
	}
}

// Damaged input must end in an error, never in a panic or in silently wrong
// output. The check values make that reliable for every fixture but the
// --check=none stream.
func TestXZRejectsCorruptInput(t *testing.T) {
	for _, name := range []string{"chunks-crc64.xz", "dict4k-lp2-sha256.xz", "stored-crc32.xz", "pkg.tar.xz"} {
		packed := readXZFixture(t, name)
		want, err := io.ReadAll(newXZReader(bytes.NewReader(packed)))
		if err != nil {
			t.Fatal(err)
		}
		step := max(1, len(packed)/100)
		for off := 0; off < len(packed); off += step {
			bad := bytes.Clone(packed)
			bad[off] ^= 0x41
			got, err := io.ReadAll(newXZReader(bytes.NewReader(bad)))
			if err == nil {
				t.Errorf("%s: flipped byte %d was accepted (output equal: %v)", name, off, bytes.Equal(got, want))
			}
		}
		for _, cut := range []int{0, 5, 12, 13, len(packed) / 2, len(packed) - 13, len(packed) - 1} {
			_, err := io.ReadAll(newXZReader(bytes.NewReader(packed[:cut])))
			if !errors.Is(err, errXZ) {
				t.Errorf("%s: truncated at %d: err = %v, want a corrupt-input error", name, cut, err)
			}
		}
	}

	for _, junk := range []string{"", "not an xz file at all", "\xfd7zXZ\x00\x00\x04"} {
		if _, err := io.ReadAll(newXZReader(strings.NewReader(junk))); !errors.Is(err, errXZ) {
			t.Errorf("junk %q: err = %v", junk, err)
		}
	}
	// Trailing garbage after a complete stream is not stream padding.
	packed := append(readXZFixture(t, "stored-crc32.xz"), "xx"...)
	if _, err := io.ReadAll(newXZReader(bytes.NewReader(packed))); err == nil {
		t.Error("trailing garbage was accepted")
	}
}

func TestExtractTarXz(t *testing.T) {
	payload := filepath.Join("testdata", "xz", "pkg.tar.xz")
	dir := t.TempDir()
	out, err := extractFiles(payload, ArchiveTarXz, map[string]string{"pkg-1.0/bin/tool": "tool"}, dir)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	const want = "#!/bin/sh\necho tool 1.0\n"
	sum := sha256.Sum256([]byte(want))
	if len(out) != 1 || out["tool"].size != int64(len(want)) || out["tool"].sha256 != hex.EncodeToString(sum[:]) {
		t.Errorf("extracted = %+v", out)
	}
	data, err := os.ReadFile(filepath.Join(dir, "tool"))
	if err != nil || string(data) != want {
		t.Errorf("tool = %q, %v", data, err)
	}
	// Windows has no executable bit; the mode it reports is unrelated.
	if st, err := os.Stat(filepath.Join(dir, "tool")); err != nil || (runtime.GOOS != "windows" && st.Mode().Perm() != 0o755) {
		t.Errorf("tool mode = %v, %v", st, err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("unlisted entries were extracted: %v", entries)
	}

	// Two files from one archive, and a file that is not there.
	out, err = extractFiles(payload, ArchiveTarXz, map[string]string{"pkg-1.0/bin/tool": "a", "pkg-1.0/doc/big.txt": "b"}, t.TempDir())
	if err != nil || len(out) != 2 || out["b"].size < 1000 {
		t.Errorf("two files: %+v, %v", out, err)
	}
	if _, err := extractFiles(payload, ArchiveTarXz, map[string]string{"pkg-1.0/bin/missing": "x"}, t.TempDir()); err == nil {
		t.Error("missing entry was accepted")
	}
	// A directory entry never satisfies a file mapping.
	if _, err := extractFiles(payload, ArchiveTarXz, map[string]string{"pkg-1.0/bin": "x"}, t.TempDir()); err == nil {
		t.Error("directory entry was extracted as a file")
	}
}
