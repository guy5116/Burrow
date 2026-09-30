GO      ?= go
TAGS    ?=
FUZZTIME ?= 60s
FUZZPKGS = internal/wire internal/invite internal/text internal/store internal/media internal/transport/mdns
BIN      = $(shell $(GO) env GOPATH)/bin

.PHONY: tools build build-gui cross test test-short test-gui lint fuzz bench release docs-check

tools:
	$(GO) install honnef.co/go/tools/cmd/staticcheck@v0.8.1
	$(GO) install github.com/securego/gosec/v2/cmd/gosec@v2.29.0
	$(GO) install golang.org/x/vuln/cmd/govulncheck@v1.8.0
	$(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.14.0

build:
	$(GO) build ./cmd/burrow

build-gui:
	$(GO) build -tags gui ./cmd/burrow-gui

cross:
	GOOS=linux   GOARCH=amd64 CGO_ENABLED=0 $(GO) build -o /dev/null ./cmd/burrow
	GOOS=darwin  GOARCH=arm64 CGO_ENABLED=0 $(GO) build -o /dev/null ./cmd/burrow
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 $(GO) build -o /dev/null ./cmd/burrow

test:
	$(GO) test -race -count=1 ./...
	$(GO) test -count=1 -run 'Allocs' ./internal/...   # allocation budgets, measured without the race runtime

test-short:
	$(GO) test -race -short ./...

test-gui:
	$(GO) test -race -tags gui ./internal/ui/gui/...

# `make lint TAGS=gui` checks the GUI too (it needs Fyne's C libraries).
lint:
	@test -z "$$(gofmt -l . | tee /dev/stderr)"
	$(GO) vet -tags "$(TAGS)" ./...
	$(BIN)/staticcheck -tags "$(TAGS)" ./...
	$(BIN)/gosec -quiet -tags "$(TAGS)" -exclude=G304 -exclude-dir=testdata ./...  # G304: file paths are our own validated store paths (§7)
	$(BIN)/govulncheck -tags "$(TAGS)" ./...
	$(BIN)/golangci-lint run --build-tags "$(TAGS)" ./...

fuzz:
	@for pkg in $(FUZZPKGS); do \
	  list=$$($(GO) test -list 'Fuzz.*' ./$$pkg) || { echo "$$list"; echo "FAIL: $$pkg does not build"; exit 1; }; \
	  targets=$$(echo "$$list" | grep '^Fuzz') || { echo "FAIL: no fuzz targets in $$pkg"; exit 1; }; \
	  for f in $$targets; do \
	    echo "== $$pkg/$$f"; $(GO) test -run '^$$' -fuzz="^$$f$$" -fuzztime=$(FUZZTIME) ./$$pkg || exit 1; \
	  done; \
	done

bench:
	$(GO) run ./bench/cmp -baseline bench/baseline.json -- $(GO) test -run '^$$' -bench . -benchmem -count=5 ./internal/session ./internal/media ./internal/handshake

release:
	mkdir -p dist
	GOOS=linux   GOARCH=amd64 CGO_ENABLED=0 $(GO) build -trimpath -ldflags="-s -w -buildid=" -o dist/burrow-linux-amd64 ./cmd/burrow
	GOOS=darwin  GOARCH=arm64 CGO_ENABLED=0 $(GO) build -trimpath -ldflags="-s -w -buildid=" -o dist/burrow-darwin-arm64 ./cmd/burrow
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 $(GO) build -trimpath -ldflags="-s -w -buildid=" -o dist/burrow-windows-amd64.exe ./cmd/burrow
	cd dist && sha256sum burrow-* > SHA256SUMS

docs-check:
	$(GO) test -count=1 -run '^(TestProtocolDocConstants|TestConstantsMatchTheSpec)$$' ./internal/wire/
