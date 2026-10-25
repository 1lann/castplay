# syntax=docker/dockerfile:1

# castplay builds on the machine's own architecture and cross-compiles for
# the target: no cgo, so no emulation.
FROM --platform=$BUILDPLATFORM golang:1.26 AS build

WORKDIR /src

# Dependencies change far less often than the code, so they get a layer of
# their own that survives an ordinary commit.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY cmd ./cmd
COPY internal ./internal

ARG TARGETOS TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/castplay ./cmd/castplay

# Execution container. Not distroless: castplay transcodes with an ffmpeg
# subprocess, and Alpine's ffmpeg includes libopus.
FROM alpine:3.22

RUN apk add --no-cache ca-certificates ffmpeg libcap-setcap \
	&& addgroup -g 1000 -S castplay \
	&& adduser -u 1000 -S castplay -G castplay \
	&& mkdir /state && chown castplay:castplay /state

COPY --from=build /out/castplay /castplay

# PTP needs UDP 319/320, below 1024; this lets the unprivileged user bind
# them without running as root.
RUN setcap cap_net_bind_service=+ep /castplay

USER castplay
VOLUME /state
ENTRYPOINT ["/castplay", "-state", "/state"]
