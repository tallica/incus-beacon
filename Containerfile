# Builds natively on the build host and cross-compiles, so a multi-arch build
# needs no emulation.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETOS TARGETARCH
ARG VERSION=dev COMMIT= DATE=
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
      -ldflags "-s -w \
        -X github.com/tallica/incus-beacon/internal/version.Version=$VERSION \
        -X github.com/tallica/incus-beacon/internal/version.Commit=$COMMIT \
        -X github.com/tallica/incus-beacon/internal/version.Date=$DATE" \
      -o /out/incus-beacon ./cmd/incus-beacon

# busybox rather than scratch: health checks need wget, and incus exec a shell.
FROM busybox:1.37-musl
ARG VERSION=dev COMMIT=
LABEL org.opencontainers.image.title="incus-beacon" \
      org.opencontainers.image.description="Reports newer registry images for Incus OCI instances" \
      org.opencontainers.image.source="https://github.com/tallica/incus-beacon" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.version="$VERSION" \
      org.opencontainers.image.revision="$COMMIT"
COPY --from=build /out/incus-beacon /usr/local/bin/incus-beacon
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY LICENSE THIRD_PARTY_NOTICES.md IMAGE_NOTICES.md /usr/share/doc/incus-beacon/
USER 65534:65534
EXPOSE 9153
HEALTHCHECK --interval=1m --timeout=5s --start-period=1m --start-interval=5s \
  CMD wget -q -O /dev/null http://127.0.0.1:9153/ready || exit 1
ENTRYPOINT ["/usr/local/bin/incus-beacon"]
CMD ["serve"]
