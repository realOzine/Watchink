# Watchink build entry points (docs/v0.md §16). macOS arm64 only for now.
# TODO(windows) / TODO(linux): GUI packaging for the other platforms is not
# implemented (Linux needs the extra `webkit2_41` build tag, Windows depends
# on S-02); only the pure-Go `build` target is portable.

SHELL := /bin/bash
.DEFAULT_GOAL := build

WHISPER_BIN := internal/tools/embedded/bin/darwin-arm64/whisper-cli
# Wails needs these on macOS when building without the wails CLI.
MACOS_MIN := 13.0
DESKTOP_ENV := CGO_ENABLED=1 \
	CGO_CFLAGS="-mmacosx-version-min=$(MACOS_MIN)" \
	CGO_LDFLAGS="-framework UniformTypeIdentifiers -mmacosx-version-min=$(MACOS_MIN)"

.PHONY: frontend whisper build desktop package-macos icon test lint clean

## frontend: build the SPA into internal/webui/dist
frontend:
	cd frontend && corepack pnpm install --frozen-lockfile && corepack pnpm build

## whisper: build the embedded whisper-cli (skipped when already present)
whisper:
	@if [ -f $(WHISPER_BIN) ]; then echo "$(WHISPER_BIN) is up to date"; \
	else bash third_party/whisper/build.sh darwin arm64; fi

## build: pure-Go headless dev binary (no GUI, no cgo) -> bin/watchink
build:
	CGO_ENABLED=0 go build -tags nogui -o bin/watchink ./cmd/watchink

## desktop: dist/Watchink.app (frontend + whisper-cli + GUI build, ad-hoc signed)
desktop:
	bash scripts/build-macos.sh app

## package-macos: dist/Watchink-<ver>-macos-arm64.zip + SHA256SUMS
package-macos:
	bash scripts/build-macos.sh package

# --- Linux amd64 (run on Linux; see scripts/build-linux.sh for the packages) ---
.PHONY: whisper-linux desktop-linux package-linux lint-linux

## whisper-linux: build the embedded static whisper-cli (CPU) for linux-amd64
whisper-linux:
	bash third_party/whisper/build.sh linux amd64

## desktop-linux: dist/watchink-<ver>-linux-amd64 (frontend + whisper-cli + GUI build)
desktop-linux:
	bash scripts/build-linux.sh desktop

## package-linux: the GUI ELF, dist/watchink-headless-<ver>-linux-amd64 and SHA256SUMS
package-linux:
	bash scripts/build-linux.sh package

## lint-linux: gofmt + go vet on Linux, including the desktop-tagged code
lint-linux:
	@out="$$(gofmt -l cmd internal scripts build)"; \
	if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	go vet ./...
	CGO_ENABLED=1 go vet -tags desktop,webkit2_41 ./...

## icon: regenerate build/appicon.png and build/darwin/Watchink.icns
icon:
	go run scripts/genicon.go build/appicon.png
	bash scripts/make-icns.sh

## test: unit tests of the default (non-GUI) build
test:
	go test ./...

## lint: gofmt + go vet, including the desktop-tagged code
lint:
	@out="$$(gofmt -l cmd internal scripts)"; \
	if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	go vet ./...
	$(DESKTOP_ENV) go vet -tags desktop ./...

## clean: remove build outputs (keeps the embedded whisper-cli and the SPA)
clean:
	rm -rf bin dist

# --- Windows amd64 ----------------------------------------------------------
# The release is built on Windows under Git Bash (.github/workflows/windows.yml).
# The same targets cross-compile from macOS or Linux, because the Windows GUI
# build needs no cgo; such a build has no embedded whisper-cli and, without a
# MinGW windres, no icon or manifest. See docs/spikes/v0-windows.md.

.PHONY: whisper-windows desktop-windows package-windows icon-windows lint-windows

## whisper-windows: build the embedded whisper-cli.exe (Windows host only)
whisper-windows:
	bash third_party/whisper/build.sh windows amd64

## desktop-windows: dist/Watchink/{Watchink.exe (GUI), watchink.com (console)}
desktop-windows:
	bash scripts/build-windows.sh app

## package-windows: dist/Watchink-<ver>-windows-amd64.zip + SHA256SUMS
package-windows:
	bash scripts/build-windows.sh package

## icon-windows: regenerate build/windows/Watchink.ico from build/appicon.png
icon-windows:
	go run scripts/make-ico.go build/appicon.png build/windows/Watchink.ico

## lint-windows: go vet of the Windows build, from any host
lint-windows:
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go vet -tags nogui ./...
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go vet -tags desktop ./...
