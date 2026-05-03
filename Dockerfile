# syntax=docker/dockerfile:1

FROM golang:1.26-alpine AS build
WORKDIR /src
RUN apk add --no-cache ca-certificates git
COPY go.mod go.sum ./
COPY vendor ./vendor
COPY internal ./internal
COPY cmd ./cmd
ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags="-s -w" -mod=vendor -o /sockt-runtime ./cmd/sockt-runtime

FROM ubuntu:24.04 AS runtime
RUN apt-get update \
  && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends bash ca-certificates \
  && rm -rf /var/lib/apt/lists/*

RUN useradd -m -u 1000 -s /bin/bash sandbox

COPY --from=build /sockt-runtime /sockt-runtime

EXPOSE 8443
USER root
ENTRYPOINT ["/sockt-runtime"]
