GO      ?= go
FUZZTIME ?= 60s
PKGS     = $(shell $(GO) list ./... | grep -v /internal/ui/gui)
FUZZPKGS = internal/wire internal/invite internal/text internal/store internal/media internal/handshake
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

test-short:
	$(GO) test -race -short ./...

test-gui:
	$(GO) test -race -tags gui ./internal/ui/gui/...

lint:
	@test -z "$$(gofmt -l . | tee /dev/stderr)"
	$(GO) vet ./...
	$(BIN)/staticcheck ./...
	$(BIN)/gosec -quiet -exclude=G304 -exclude-dir=testdata ./...  # G304: file paths are our own validated store paths (§7)
	$(BIN)/govulncheck ./...
	$(BIN)/golangci-lint run ./...

fuzz:
	@for pkg in $(FUZZPKGS); do \
	  [ -d $$pkg ] || continue; \
	  for f in $$($(GO) test -list 'Fuzz.*' ./$$pkg 2>/dev/null | grep '^Fuzz'); do \
	    echo "== $$pkg/$$f"; $(GO) test -run '^$$' -fuzz="^$$f$$" -fuzztime=$(FUZZTIME) ./$$pkg || exit 1; \
	  done; \
	done

bench:
	$(GO) run ./bench/cmp -baseline bench/baseline.json -- $(GO) test -run '^$$' -bench . -benchmem ./internal/session ./internal/media ./internal/handshake

release:
	mkdir -p dist
	GOOS=linux   GOARCH=amd64 CGO_ENABLED=0 $(GO) build -trimpath -ldflags="-s -w -buildid=" -o dist/burrow-linux-amd64 ./cmd/burrow
	GOOS=darwin  GOARCH=arm64 CGO_ENABLED=0 $(GO) build -trimpath -ldflags="-s -w -buildid=" -o dist/burrow-darwin-arm64 ./cmd/burrow
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 $(GO) build -trimpath -ldflags="-s -w -buildid=" -o dist/burrow-windows-amd64.exe ./cmd/burrow
	cd dist && sha256sum burrow-* > SHA256SUMS

docs-check:
	$(GO) test -count=1 -run '^TestProtocolDocConstants$$' ./internal/wire/
