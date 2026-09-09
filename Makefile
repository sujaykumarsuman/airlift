# airlift — build entry points. See docs/BUILD-PLAN.md.
BIN      := bin
TOWER    := airlift-tower
MODULE   := github.com/sujaykumarsuman/airlift
PLATFORMS := darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64

.PHONY: all web tower tower-all replay sender-test sender-lint go-test go-lint web-test web-lint test lint pre-commit setup clean

all: web tower

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

## ---- tower ----
tower: web
	mkdir -p $(BIN)
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $(BIN)/$(TOWER) ./cmd/tower

tower-all: web
	mkdir -p $(BIN)
	@for p in $(PLATFORMS); do \
	  os=$${p%/*}; arch=$${p#*/}; ext=""; \
	  [ "$$os" = windows ] && ext=".exe"; \
	  echo "  $$os/$$arch"; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags="-s -w" \
	    -o $(BIN)/$(TOWER)-$$os-$$arch$$ext ./cmd/tower || exit 1; \
	done

replay: tower
	$(BIN)/$(TOWER) --dest /tmp/airlift-out --replay sender/testdata/vectors.json --drop 0.2

go-lint:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt:"; echo "$$out"; exit 1; fi
	go vet ./...

go-test:
	go test ./...

## ---- sender ----
sender-lint:
	uv run --directory sender ruff check .
	uv run --directory sender ruff format --check .

sender-test:
	uv run --directory sender pytest -q

## ---- aggregate ----
lint: sender-lint go-lint web-lint
test: sender-test go-test web-test

pre-commit:
	pre-commit run --all-files

setup: web/node_modules
	uv sync --directory sender
	pre-commit install

clean:
	rm -rf $(BIN) web/dist/* sender/.venv
	@touch web/dist/.gitkeep
