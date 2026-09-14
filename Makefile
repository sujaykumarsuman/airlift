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

# Deployment (Phase 8, docs/HOSTING.md). VPS is an ssh host alias; DOMAIN is the
# public hostname whose A record points at the VPS; PREFIX is the path airlift is
# mounted under (Caddy strips it, ADR 0012); PUBLIC_URL is what the tower advertises.
VPS        ?= airlift-vps
DOMAIN     ?= projects.sujaykumar.dev
PREFIX     ?= /airlift
PUBLIC_URL ?= https://$(DOMAIN)$(PREFIX)

.PHONY: all web airlift airlift-all airlift-linux docs-shots go-test go-lint web-test web-lint test lint pre-commit setup clean vps-bootstrap deploy

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

airlift-linux: web
	mkdir -p $(BIN)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="$(LDFLAGS)" \
	  -o $(BIN)/$(AIRLIFT)-linux-amd64 ./cmd/airlift

## ---- deploy (Phase 8; see docs/HOSTING.md) ----
# One-time host setup: install the unit + Caddyfile, create the service user and
# an 0600 config with an admin token, install Caddy. Idempotent.
vps-bootstrap:
	scp deploy/airlift.service $(VPS):/etc/systemd/system/airlift.service
	sed -e 's/{{DOMAIN}}/$(DOMAIN)/g' -e 's|{{PREFIX}}|$(PREFIX)|g' deploy/Caddyfile | ssh $(VPS) 'cat > /etc/caddy/Caddyfile'
	scp deploy/bootstrap.sh $(VPS):/tmp/airlift-bootstrap.sh
	ssh $(VPS) 'bash /tmp/airlift-bootstrap.sh "$(PUBLIC_URL)" && rm -f /tmp/airlift-bootstrap.sh'
	ssh $(VPS) 'systemctl restart airlift; systemctl reload caddy || systemctl restart caddy'

# Build the Linux binary and roll it out with a zero-downtime rename + restart.
# The tower reports the stamped VERSION (GET /api/info, the landing footer): a
# tagged HEAD gives a clean "v1.2.3"; an untagged one still deploys, but says so.
deploy: airlift-linux
	@echo "deploying $(VERSION)"
	@git describe --tags --exact-match >/dev/null 2>&1 || echo "  note: HEAD is not on a tag — the tower will report $(VERSION); tag a release for a clean version"
	scp $(BIN)/$(AIRLIFT)-linux-amd64 $(VPS):/usr/local/bin/airlift.new
	ssh $(VPS) 'chmod 755 /usr/local/bin/airlift.new && mv -f /usr/local/bin/airlift.new /usr/local/bin/airlift && systemctl restart airlift && sleep 1 && systemctl is-active airlift'

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
