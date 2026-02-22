FROM --platform=$BUILDPLATFORM golang:alpine AS builder

ARG VERSION="dev"
ARG GIT_COMMIT

ARG TARGETOS
ARG TARGETARCH

ENV GOOS=$TARGETOS \
    GOARCH=$TARGETARCH \
    GO111MODULE=on \
    CGO_ENABLED=0

RUN apk add --no-cache ca-certificates && update-ca-certificates

WORKDIR /src
COPY . /src

RUN go build -trimpath \
    -ldflags "-w -X server.serverVersion=${VERSION} -X server.gitCommit=${GIT_COMMIT}" \
    -o /out/pubsub .

FROM --platform=$TARGETPLATFORM alpine:latest

LABEL org.opencontainers.image.title="Hanzo PubSub"
LABEL org.opencontainers.image.description="NATS-compatible pub/sub messaging — Hanzo PubSub"
LABEL org.opencontainers.image.vendor="Hanzo AI"
LABEL org.opencontainers.image.source="https://github.com/hanzoai/pubsub"

COPY docker/nats-server.conf                  /pubsub/conf/server.conf
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /out/pubsub               /bin/pubsub

EXPOSE 4222 8222 6222 5222

ENTRYPOINT ["/bin/pubsub"]
CMD ["-c", "/pubsub/conf/server.conf"]
