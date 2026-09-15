# airlift — build, test and package the single `airlift` binary. `make` never
# runs the application. See prompts/002-go-cli-and-hosting.md.
BIN       := bin
AIRLIFT   := airlift
MODULE    := github.com/sujaykumarsuman/airlift
PLATFORMS := darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64

# The version stamped into the binary (GET /api/info, the landing footer): the
# nearest tag from git, or pass VERSION=v1.2.3 — the release workflow passes the tag.
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS   := -s -w -X $(MODULE).Version=$(VERSION)

.PHONY: all web airlift airlift-all airlift-linux docs-shots scan-e2e go-test go-lint web-test web-lint test lint pre-commit setup clean

all: web airlift

## ---- web ----
web/node_modules: web/package.json web/package-lock.json
	npm --prefix web ci
	@touch web/node_modules

web: web/node_modules
	npm --prefix web run --silent build
	@touch web/dist/.gitkeep

web-lint: web/node_modules
	npm --prefix web run --silent typecheck
	npm --prefix web run --silent lint

web-test: web/node_modules
	npm --prefix web run --silent test

## ---- airlift (Go) ----
airlift: web
	mkdir -p $(BIN)
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o $(BIN)/$(AIRLIFT) ./cmd/airlift

airlift-all: web
	mkdir -p $(BIN)
	@for p in $(PLATFORMS); do \
	  os=$${p%/*}; arch=$${p#*/}; ext=""; \
	  [ "$$os" = windows ] && ext=".exe"; \
	  echo "  $$os/$$arch"; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags="$(LDFLAGS)" \
	    -o $(BIN)/$(AIRLIFT)-$$os-$$arch$$ext ./cmd/airlift || exit 1; \
	done

# Regenerate the docs page's screenshots from the real app (a throw-away tower,
# a demo beam, headless Chrome over CDP): web/public/docs/*.webp. Rebuild after.
docs-shots: airlift
	node web/tools/docs-shots.mjs

# Drive a real beam through the real scanner without a phone: the player's
# frames recorded into an MJPEG file and fed to headless Chrome as a fake camera
# on the scan page of a throw-away tower (BEAM=folder to beam something else).
scan-e2e: airlift
	node web/tools/scan-e2e.mjs

airlift-linux: web
	mkdir -p $(BIN)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="$(LDFLAGS)" \
	  -o $(BIN)/$(AIRLIFT)-linux-amd64 ./cmd/airlift

## ---- deploy ----
# Deployment is GitOps: CI (.github/workflows/deploy.yml) builds the image to GHCR
# on a release tag, and Flux deploys it to the k3s cluster. See docs/HOSTING.md and
# the sujaykumarsuman/infra repo. `make` only builds; it never deploys.

go-lint:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt:"; echo "$$out"; exit 1; fi
	go vet ./...

go-test:
	go test ./...

## ---- aggregate ----
lint: go-lint web-lint
test: go-test web-test

pre-commit:
	pre-commit run --all-files

setup: web/node_modules
	pre-commit install

clean:
	rm -rf $(BIN) web/dist/*
	@touch web/dist/.gitkeep
