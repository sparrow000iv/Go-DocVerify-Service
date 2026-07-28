# --- Build stage -------------------------------------------------------------
# Compiles a fully static binary so the runtime image needs no libc.
FROM golang:1.23-alpine AS builder

WORKDIR /src

# Copy manifests first so dependency layers cache across source changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
        -trimpath \
        -ldflags="-s -w -X main.version=${VERSION}" \
        -o /out/server ./cmd/server

# Run the test suite inside the image build to fail fast on regressions.
RUN go vet ./... && go test -count=1 ./...

# --- Runtime stage -----------------------------------------------------------
# Distroless keeps the attack surface minimal and runs as a non-root user.
FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /
COPY --from=builder /out/server /server

# 8080 = REST + /metrics, 9090 = gRPC
EXPOSE 8080 9090

USER nonroot:nonroot

ENTRYPOINT ["/server"]
