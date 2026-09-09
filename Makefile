# airlift — build, test and package the single `airlift` binary. `make` never
# runs the application. See docs/BUILD-PLAN.md and prompts/002-go-cli-and-hosting.md.
BIN       := bin
AIRLIFT   := airlift
MODULE    := github.com/sujaykumarsuman/airlift
PLATFORMS := darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64

.PHONY: all web airlift airlift-all go-test go-lint web-test web-lint test lint pre-commit setup clean

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
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $(BIN)/$(AIRLIFT) ./cmd/airlift

airlift-all: web
	mkdir -p $(BIN)
	@for p in $(PLATFORMS); do \
	  os=$${p%/*}; arch=$${p#*/}; ext=""; \
	  [ "$$os" = windows ] && ext=".exe"; \
	  echo "  $$os/$$arch"; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags="-s -w" \
	    -o $(BIN)/$(AIRLIFT)-$$os-$$arch$$ext ./cmd/airlift || exit 1; \
	done

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
