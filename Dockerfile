# syntax=docker/dockerfile:1
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/aoc ./cmd/aoc

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata wget \
    && adduser -D -u 10001 aoc \
    && mkdir -p /var/log/agent-of-chaos \
    && chown aoc:aoc /var/log/agent-of-chaos
COPY --from=build /out/aoc /usr/local/bin/aoc
COPY profiles /opt/aoc/profiles
USER aoc
WORKDIR /home/aoc
VOLUME ["/var/log/agent-of-chaos"]
EXPOSE 8282 10516
ENTRYPOINT ["aoc"]
CMD ["generate"]
