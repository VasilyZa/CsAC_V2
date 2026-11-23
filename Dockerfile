# syntax=docker/dockerfile:1
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
RUN CGO_ENABLED=0 go build -trimpath -o /out/csacd ./cmd/csacd \
    && CGO_ENABLED=0 go build -trimpath -tags timetzdata -o /out/csac-migrate ./cmd/csac-migrate

FROM alpine:3.23
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/csacd /out/csac-migrate /usr/local/bin/
RUN mkdir -p /var/lib/csac/media /var/lib/csac/releases \
    && chown 65532:65532 /var/lib/csac/media /var/lib/csac/releases
USER 65532:65532
ENV GODEBUG=http2xconnect=1 CSAC_MEDIA_ROOT=/var/lib/csac/media
EXPOSE 8443/tcp 8443/udp
ENTRYPOINT ["/usr/local/bin/csacd"]
