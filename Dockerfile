# tigerbeetle-go uses cgo to link TigerBeetle's prebuilt native client library,
# so the build stage needs a C toolchain and CGO enabled.
FROM golang:1.23-bookworm AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal

RUN CGO_ENABLED=1 go build -o /out/ratelimiter ./cmd/ratelimiter

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*

COPY --from=build /out/ratelimiter /usr/local/bin/ratelimiter

EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/ratelimiter"]
