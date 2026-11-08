# One image, two targets. A plain build gives the gateway, the last stage;
# --target picks the mock provider instead. Run both from the repo root:
#
#   docker build -f deployments/Dockerfile -t llm-gateway:dev .
#   docker build -f deployments/Dockerfile --target mockprovider -t llm-gateway-mock:dev .

# The builder is on go.mod's Go line, which is also the one CI tests on. A newer
# Go would run encoding/json on json/v2, so the image would behave differently
# from what CI tested. The newest 1.26 release on hub.docker.com/_/golang,
# checked 2026-11-08.
FROM golang:1.26.10 AS builder
WORKDIR /src

# go.mod and go.sum alone first, so the dependency download is its own layer and
# stays cached until one of them changes.
COPY go.mod go.sum ./
RUN go mod download

# .dockerignore lets in only cmd/ and internal/ besides the two files above.
COPY . .

# CGO_ENABLED=0 builds static binaries, which the images below need: they have no
# libc. -trimpath drops this machine's paths from the binaries, and -s -w drops
# the symbol table and debug info, which makes them smaller.
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/ \
    ./cmd/gateway ./cmd/migrate ./cmd/mockprovider

# Distroless, not FROM scratch. It ships CA certificates, which every HTTPS call
# to a provider needs, and scratch has none, so each of those calls would fail.
# The :nonroot tag also runs as uid 65532 instead of root.
#
# Distroless has no version tags, so the digest pins it. It is the multi-platform
# index, so CI on amd64 and a Mac on arm64 build from the same release. The
# newest Debian line distroless publishes, checked 2026-11-08.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329 AS runtime

# The mock provider: fake OpenAI, Groq, Anthropic and Gemini APIs, so the stack
# runs with no real keys. It listens on every interface because its default,
# 127.0.0.1, is the container's own loopback, which nothing outside the
# container can reach.
FROM runtime AS mockprovider
COPY --from=builder /out/mockprovider /mockprovider
EXPOSE 9090
ENTRYPOINT ["/mockprovider", "-addr", ":9090"]

# The gateway, and /migrate, which applies the schema. Last, so it is the default
# target.
#
# No config is baked in. config.yaml binds 127.0.0.1, so no published port would
# reach the gateway, and it needs four provider keys, which a container doesn't
# have unless it is given them. Whoever runs the image mounts a config at the
# path below and sets the keys that config names.
FROM runtime AS gateway
COPY --from=builder /out/gateway /gateway
COPY --from=builder /out/migrate /migrate
ENV GATEWAY_CONFIG=/etc/llm-gateway/config.yaml
EXPOSE 8080 9091
ENTRYPOINT ["/gateway"]
