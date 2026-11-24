# Build the KelmaSync server as a small static binary.
FROM golang:1.26 AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

# Copy only server source. Runtime data, clients, Git metadata, and environment
# files are excluded both here and by .dockerignore.
COPY cmd ./cmd
COPY internal ./internal
COPY migrations ./migrations
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server && \
    mkdir -p /out/tmp && chmod 0700 /out/tmp

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/server /app/server
COPY --from=build /src/migrations /app/migrations
# Large media restores stage prepared TAR archives in a private temp directory.
COPY --from=build --chown=65532:65532 /out/tmp /tmp
ENV MIGRATIONS_DIR=/app/migrations
ENV TMPDIR=/tmp
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/app/server"]
