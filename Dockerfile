# syntax=docker/dockerfile:1
# node:22-alpine
FROM node:22-alpine@sha256:0a7108bf6c7bf5de370ffb1a3ed6be93d405b43ff159f681a8d18c0e2bc2e402 AS web
ENV COREPACK_ENABLE_DOWNLOAD_PROMPT=0
WORKDIR /src
COPY package.json pnpm-lock.yaml pnpm-workspace.yaml ./
# pnpm fetch reads only the lockfile, so source edits keep the dependency layer cached.
RUN corepack enable && pnpm fetch
COPY gen/ gen/
COPY web/ web/
RUN pnpm install --offline --frozen-lockfile --filter harness-control-plane-web... \
 && pnpm --filter harness-control-plane-web... build

# golang:1.26-alpine
FROM golang:1.26-alpine@sha256:3082400e369fa24d5fc60bca20edab3f6d604e0c5a690ec66b295eff4dd87ade AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# web/embed.go embeds web/dist at compile time, so the UI must land before go build.
COPY --from=web /src/web/dist ./web/dist
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/harnessctl-server ./cmd/harnessctl-server \
 && mkdir -p /out/data/blobs

# gcr.io/distroless/static-debian12:nonroot
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/harnessctl-server /usr/local/bin/harnessctl-server
COPY --from=build --chown=nonroot:nonroot /out/data /data
# The binary links Go modules and embeds the admin UI bundle, so the image
# redistributes both, and their notices travel with it: third_party/licenses holds
# the UI's npm packages at its top level and the linked Go modules under go/.
COPY --from=build /src/LICENSE /src/NOTICE /usr/share/licenses/tetriz-harness/
COPY --from=build /src/third_party/licenses /usr/share/licenses/tetriz-harness/third_party/
ENV HARNESS_LISTEN_ADDR=:8080 \
    HARNESS_BLOB_BACKEND=fs \
    HARNESS_BLOB_DIR=/data/blobs
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/harnessctl-server"]
