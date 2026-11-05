# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine@sha256:d200d2dd76b03a99b52651fd1325aff7dcd35422c35c01bac56571b0507f3deb AS source
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

FROM source AS test
RUN go test -count=1 ./... && go vet ./...

FROM source AS build
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/tabsgate ./cmd/tabsgate
RUN mkdir -p /out/data && chown 65532:65532 /out/data

FROM scratch AS runtime
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/tabsgate /tabsgate
COPY --from=build --chown=65532:65532 /out/data /data
USER 65532:65532
ENV TABSGATE_ADDR=:8080 TABSGATE_DB_PATH=/data/tabsgate.db
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 CMD ["/tabsgate", "healthcheck"]
ENTRYPOINT ["/tabsgate"]
