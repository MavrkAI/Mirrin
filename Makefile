VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
# Build tags, e.g. `make build TAGS=nowhatsapp` for a program without WhatsApp
# and the GPL-3.0 library it needs (docs/licensing.md).
TAGS ?=
# The "Hey Maverick" wake-word model was trained on data not cleared for
# commercial use, so it is no part of a release (docs/wake-word.md). A local
# build carries it when the file is in this checkout: a maintainer's copy has
# it, the public repository doesn't.
WAKE_MODEL := internal/channels/voice/assets/hey_maverick.onnx
LOCAL_TAGS := $(strip $(TAGS) $(if $(wildcard $(WAKE_MODEL)),wakemodel))
comma := ,
space := $(subst ,, )
GOTAGS := $(if $(LOCAL_TAGS),-tags '$(subst $(space),$(comma),$(LOCAL_TAGS))')
# Release targets build both variants explicitly, and never with the wake
# model; TAGS and the model apply to local builds (build, install).
# Leave local paths out of release binaries.
RELEASE_FLAGS := -trimpath -ldflags '$(LDFLAGS)'

.PHONY: build test test-race lint notices run chat install sign-identity release dist-portable dist-darwin voice-bundle app dmg clean

UNAME := $(shell uname -s)

# macOS needs cgo for the menu bar icon; everything else builds static.
CGO ?= $(if $(filter Darwin,$(UNAME)),1,0)

# Built from any host without cgo. The macOS builds need cgo for the menu bar,
# so they are only made on a Mac (dist-darwin).
PORTABLE := linux/amd64 linux/arm64 windows/amd64 windows/arm64

build:
	CGO_ENABLED=$(CGO) go build $(GOTAGS) -ldflags '$(LDFLAGS)' -o bin/mirrin ./cmd/mirrin

test:
	go test ./...

test-race:
	go test -race ./...

# The gates CI runs. shellcheck runs when it's installed; staticcheck is
# built from scripts/lint, which pins it. Any finding fails, as in CI.
lint:
	go vet ./...
	go vet -tags nowhatsapp ./...
	@out=$$(gofmt -l cmd internal scripts packaging); if [ -n "$$out" ]; then echo "gofmt -w these files:"; echo "$$out"; exit 1; fi
	go mod tidy -diff
	go run ./scripts/licenses -check THIRD_PARTY_NOTICES -copy internal/notices/THIRD_PARTY_NOTICES
	@cmp -s LICENSE internal/notices/LICENSE || { echo "internal/notices/LICENSE isn't the same as LICENSE; run make notices"; exit 1; }
	go run ./scripts/licenses -tags nowhatsapp -mit >/dev/null
	@if command -v shellcheck >/dev/null; then shellcheck install.sh scripts/*.sh packaging/docker/*.sh packaging/voice/*.sh; \
	else echo "shellcheck is not installed, so the shell scripts weren't checked"; fi
	go build -C scripts/lint -o "$${TMPDIR:-/tmp}/mirrin-staticcheck" honnef.co/go/tools/cmd/staticcheck
	"$${TMPDIR:-/tmp}/mirrin-staticcheck" ./...

# THIRD_PARTY_NOTICES lists every module the release builds link, with its
# licence. Run this after changing dependencies; `make lint` says when. Every
# program embeds a copy of it and of LICENSE (internal/notices), which
# `mirrin licenses` prints.
notices:
	go run ./scripts/licenses -o THIRD_PARTY_NOTICES -copy internal/notices/THIRD_PARTY_NOTICES
	cp LICENSE internal/notices/LICENSE

run: build
	./bin/mirrin run

chat: build
	./bin/mirrin chat

# On macOS, sign with a local identity if one exists so the microphone permission
# survives rebuilds (see docs/skills.md → Microphone permission). Create one with
# `make sign-identity`. SIGN_ID names another identity (default mirrin-dev).
SIGN_ID ?=
SIGN_IDS = $(or $(SIGN_ID),mirrin-dev)

# go install puts the program in GOBIN, else GOPATH/bin.
install:
	CGO_ENABLED=$(CGO) go install $(GOTAGS) -ldflags '$(LDFLAGS)' ./cmd/mirrin
	@bin=$$(go env GOBIN); bin=$${bin:-$$(go env GOPATH)/bin}; \
	if [ "$$(uname -s)" = Darwin ]; then \
		ids=$$(security find-identity -v -p codesigning 2>/dev/null); id=; \
		for n in $(SIGN_IDS); do \
			if printf '%s\n' "$$ids" | grep -q "\"$$n\""; then id=$$n; break; fi; \
		done; \
		if [ -n "$$id" ]; then codesign -s "$$id" -f -i com.mirrin.mavrk "$$bin/mirrin" && echo "signed with $$id"; \
		else echo "warning: no $(firstword $(SIGN_IDS)) signing identity, so macOS asks for the microphone again after every rebuild. Make one with: make sign-identity"; fi; \
	fi

sign-identity:
	@scripts/make-sign-identity.sh $(firstword $(SIGN_IDS))

# Every binary this host can build, named the way install.sh, install.ps1 and
# the Homebrew formula ask for them (GOOS-GOARCH), checked and summed. The full
# set is assembled by .github/workflows/release.yml; see docs/maintainers-release.md.
HOST_DIST := $(PORTABLE) $(if $(filter Darwin,$(UNAME)),darwin/amd64 darwin/arm64)

release: dist-portable relay-release $(if $(filter Darwin,$(UNAME)),dist-darwin)
	@$(if $(filter Darwin,$(UNAME)),:,echo "macOS builds need a Mac (make dist-darwin); the release workflow makes them on a macOS runner")
	go run ./scripts/releasecheck -require '$(HOST_DIST) $(addsuffix /nowhatsapp,$(HOST_DIST)) relay/linux/amd64 relay/linux/arm64' -sums dist

dist-portable:
	@mkdir -p dist
	@for t in $(PORTABLE); do \
		os=$${t%/*}; arch=$${t#*/}; ext=""; [ $$os = windows ] && ext=.exe; \
		echo "building $$t"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build $(RELEASE_FLAGS) -o dist/mirrin-$$os-$$arch$$ext ./cmd/mirrin || exit 1; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build $(RELEASE_FLAGS) -tags nowhatsapp -o dist/mirrin-$$os-$$arch-nowhatsapp$$ext ./cmd/mirrin || exit 1; \
	done

# Both Mac architectures, with cgo; Apple's clang cross-compiles either way.
dist-darwin:
	@[ "$(UNAME)" = Darwin ] || { echo "dist-darwin needs a Mac: the menu bar is built with cgo against the macOS SDK"; exit 1; }
	@mkdir -p dist
	@for arch in amd64 arm64; do \
		echo "building darwin/$$arch (cgo, menu bar)"; \
		CGO_ENABLED=1 GOOS=darwin GOARCH=$$arch go build $(RELEASE_FLAGS) -o dist/mirrin-darwin-$$arch ./cmd/mirrin || exit 1; \
		CGO_ENABLED=1 GOOS=darwin GOARCH=$$arch go build $(RELEASE_FLAGS) -tags nowhatsapp -o dist/mirrin-darwin-$$arch-nowhatsapp ./cmd/mirrin || exit 1; \
	done

# whisper-server and sox for this machine, from pinned sources, as
# dist/mirrin-voice-<os>-<arch>.tar.gz: what one-click voice setup downloads.
# macOS and Linux; the release workflow builds one on each kind of machine
# and releasecheck requires all four (packaging/voice/build.sh).
voice-bundle:
	sh packaging/voice/build.sh

# macOS app bundle (menu bar app with proper Info.plist and permissions strings).
app:
	scripts/build-app.sh

# Universal app, DMG and both variants of each Mac CLI; signed and notarized when Apple credentials are in the environment.
dmg:
	scripts/release-macos.sh

clean:
	rm -rf bin dist

# mirrin-relay, the SNI passthrough relay (docs/relay-selfhost.md). It runs on
# Linux servers, so releases are static linux/amd64 and linux/arm64 builds.
.PHONY: relay relay-release relay-bench
relay:
	CGO_ENABLED=0 go build -ldflags '$(LDFLAGS)' -o bin/mirrin-relay ./cmd/mirrin-relay

relay-release:
	@mkdir -p dist
	@for arch in amd64 arm64; do \
		echo "building mirrin-relay linux/$$arch"; \
		CGO_ENABLED=0 GOOS=linux GOARCH=$$arch go build -trimpath -ldflags '$(LDFLAGS)' -o dist/mirrin-relay-linux-$$arch ./cmd/mirrin-relay || exit 1; \
	done

# 10,000 idle tunnels must stay under 1 GiB of relay RSS.
relay-bench:
	go test -run '^$$' -bench BenchmarkIdleTunnels -benchtime 1x ./internal/relay/server

# mirrin-cloud, the Mirrin Cloud control plane: the nested module in cloud/
# (cloud/README.md). cloud-dev runs it on a fake merchant of record and fake
# DNS, for linking a daemon built with -tags mirrin_devkeys.
.PHONY: cloud cloud-test cloud-dev
cloud:
	cd cloud && CGO_ENABLED=0 go build -ldflags '$(LDFLAGS)' -o ../bin/mirrin-cloud ./cmd/mirrin-cloud

cloud-test:
	cd cloud && go vet ./... && go test ./...

cloud-dev:
	cd cloud && go run ./cmd/mirrin-cloud serve --dev
