# syntax=docker/dockerfile:1

# One Dockerfile builds all three binaries; the SERVICE build argument selects
# which cmd/ package is compiled. Keeping it to a single file means the build,
# the base image and the hardening choices cannot drift between services.
ARG GO_VERSION=1.26

FROM golang:${GO_VERSION}-alpine AS build
WORKDIR /src

# Dependencies are copied first so editing Go code does not invalidate the
# module download layer.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG SERVICE
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/service ./cmd/${SERVICE}

FROM alpine:3.22

# ca-certificates for the venue TLS calls added in Parts 4 to 6. The health check
# uses busybox wget, already present in the base image. The IANA timezone database
# is embedded in the binary (internal/config), so tzdata is not installed here.
RUN apk add --no-cache ca-certificates

# Nothing in these binaries needs root.
RUN adduser -D -u 10001 carry
USER carry

COPY --from=build /out/service /usr/local/bin/service

ENTRYPOINT ["/usr/local/bin/service"]
