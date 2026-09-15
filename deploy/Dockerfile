# airlift tower container image. Multi-stage, mirroring the Makefile: build the
# web UI (npm), embed web/dist, then a static CGO-free Go binary stamped with
# VERSION, onto distroless static (nonroot). Build on the VPS and import into
# k3s' containerd (no registry) — see docs/build-plan/k3s-migration.md §8.
#
#   docker build -f deploy/Dockerfile --build-arg VERSION=$(git describe --tags) \
#     -t airlift:$(git describe --tags) .
#   docker save airlift:$(git describe --tags) | k3s ctr images import -

# --- web build (Vite → web/dist) ---
FROM node:22-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run --silent build

# --- go build (embeds web/dist) ---
FROM golang:1.26-alpine AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Overwrite the .gitkeep-only web/dist with the built assets go:embed needs.
COPY --from=web /src/web/dist ./web/dist
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
      -ldflags="-s -w -X github.com/sujaykumarsuman/airlift.Version=${VERSION}" \
      -o /out/airlift ./cmd/airlift

# --- runtime ---
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/airlift /usr/local/bin/airlift
# AIRLIFT_HOME is a writable volume in k8s; config is mounted under it (subPath).
ENV AIRLIFT_HOME=/var/lib/airlift/.airlift
USER nonroot:nonroot
EXPOSE 8443
ENTRYPOINT ["/usr/local/bin/airlift"]
CMD ["tower"]
