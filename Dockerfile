# syntax=docker/dockerfile:1

ARG GO_VERSION=1.25
ARG ALPINE_VERSION=3.24

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod,sharing=locked \
    go mod download && go mod verify
COPY . .
RUN --mount=type=cache,target=/root/.cache/go-build,sharing=locked \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -buildvcs=false -ldflags='-s -w' -o /out/api ./cmd/api

FROM alpine:${ALPINE_VERSION}
RUN apk add --no-cache ca-certificates ffmpeg tzdata \
    && addgroup -S -g 10001 app \
    && adduser -S -D -H -u 10001 -G app app
WORKDIR /app
COPY --from=build --chown=app:app /out/api /app/api
EXPOSE 8080
USER app:app
ENTRYPOINT ["/app/api"]
