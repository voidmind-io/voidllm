# Stage 1: Build UI (arch-independent, runs on build platform)
FROM --platform=$BUILDPLATFORM node:20-alpine AS ui-builder
WORKDIR /app/ui
COPY ui/package.json ui/package-lock.json ./
RUN npm ci
COPY ui/ ./
RUN npm run build

# Stage 2: Build Go binary (cross-compiles on build platform)
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS go-builder
ARG TARGETOS
ARG TARGETARCH
RUN apk add --no-cache git
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=ui-builder /app/ui/dist ./ui/dist
ARG VERSION=0.0.9
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
    -ldflags="-s -w -X 'github.com/voidmind-io/voidllm/internal/api/health.Version=${VERSION}'" \
    -o /voidllm ./cmd/voidllm

# Stage 3: Runtime
FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S voidllm && adduser -S -G voidllm voidllm \
    && mkdir -p /data && chown voidllm:voidllm /data
COPY --from=go-builder /voidllm /usr/local/bin/voidllm
VOLUME ["/data"]
EXPOSE 8080 8443
USER voidllm
ENTRYPOINT ["voidllm"]
