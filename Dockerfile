# syntax=docker/dockerfile:1
FROM golang:1.26.5-bookworm AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/later-server ./cmd/later-server
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/later ./cmd/later

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /out/later-server /later-server
COPY --from=builder /out/later /later

VOLUME /data

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD ["/later", "healthcheck"]

ENTRYPOINT ["/later-server"]
