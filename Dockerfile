# syntax=docker/dockerfile:1@sha256:4edf897a3ffa55b89f906fc8cc78afdb3f1834cc9c7083565e611a8a7d5fe99e
FROM --platform=$BUILDPLATFORM golang:1.27.3-bookworm@sha256:d9b6a1beb9474adc2a1ce842573639b44b2a0dd49b6669f51d3821baf9ec37c9 AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/later-server ./cmd/later-server
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/later ./cmd/later

FROM gcr.io/distroless/static-debian12:latest@sha256:d75cdd72874d4790092fcb1b058493ecf6bb5bf2b2b897045b00ff01d91843f2

COPY --from=builder /out/later-server /later-server
COPY --from=builder /out/later /later

VOLUME /data

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD ["/later", "healthcheck"]

ENTRYPOINT ["/later-server"]
