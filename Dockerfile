# Stage 1: Build UI (arch-independent, runs on build platform)
FROM --platform=$BUILDPLATFORM node:24-alpine@sha256:a0b9bf06e4e6193cf7a0f58816cc935ff8c2a908f81e6f1a95432d679c54fbfd AS ui-builder
WORKDIR /app/ui
COPY ui/package.json ui/package-lock.json ./
RUN npm ci
COPY ui/ ./
RUN npm run build

# Stage 2: Build Go binary (cross-compiles on build platform)
FROM --platform=$BUILDPLATFORM golang:1.27-alpine@sha256:4c9fe60190a2a3350ddc51de80d0224b8a6698d12bdfc999fee45ea9d6c46dbc AS go-builder
ARG TARGETOS
ARG TARGETARCH
RUN apk add --no-cache git
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=ui-builder /app/ui/dist ./ui/dist
ARG VERSION=0.0.25
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
    -ldflags="-s -w -X 'github.com/voidmind-io/voidllm/internal/api/health.Version=${VERSION}'" \
    -o /voidllm ./cmd/voidllm

# Stage 3: Runtime
FROM alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b
RUN apk upgrade --no-cache \
    && apk add --no-cache ca-certificates tzdata \
    && addgroup -S voidllm && adduser -S -G voidllm voidllm \
    && mkdir -p /data && chown voidllm:voidllm /data
COPY --from=go-builder /voidllm /usr/local/bin/voidllm
VOLUME ["/data"]
ENV VOIDLLM_DATABASE_DSN=/data/voidllm.db
EXPOSE 8080 8443
USER voidllm
ENTRYPOINT ["voidllm"]
