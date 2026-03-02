# syntax=docker/dockerfile:1

# ---- Build stage ----
FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS builder

ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace

# Cache dependencies first
COPY go.mod go.sum ./
RUN go mod download

# Copy all source (respects .dockerignore)
COPY . .

# Build the webhook binary
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
    -ldflags="-s -w" \
    -o /workspace/virthorn-webhook \
    ./cmd/webhook

# ---- Final stage ----
# Use distroless for a minimal, secure image
FROM gcr.io/distroless/static:nonroot

COPY --from=builder /workspace/virthorn-webhook /virthorn-webhook

USER 65532:65532

ENTRYPOINT ["/virthorn-webhook"]
