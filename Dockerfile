# syntax=docker/dockerfile:1

# Build a static binary.
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=unknown
ARG DATE=unknown
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags "-s -w -X github.com/tggo/sparql-server/internal/version.Version=${VERSION} -X github.com/tggo/sparql-server/internal/version.Commit=${COMMIT} -X github.com/tggo/sparql-server/internal/version.Date=${DATE}" \
    -o /out/sparql-server ./cmd/sparql-server
# The data directory must belong to the non-root user of the runtime image.
RUN mkdir -p /out/data && chown 65532:65532 /out/data

# Run as the distroless non-root user (65532).
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/sparql-server /usr/local/bin/sparql-server
COPY --from=build --chown=65532:65532 /out/data /data
ENV SPARQL_SERVER_LISTEN=:8080
EXPOSE 8080
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/sparql-server"]
CMD ["serve"]
