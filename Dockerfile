# Stage 1: Build UI (arch-independent, runs on build platform)
FROM --platform=$BUILDPLATFORM node:26-alpine@sha256:a2dc166a387cc6ca1e62d0c8e265e49ca985d6e60abc9fe6e6c3d6ce8e63f606 AS ui-builder
WORKDIR /app/ui
COPY ui/package.json ui/package-lock.json ./
RUN npm ci
COPY ui/ ./
RUN npm run build

# Stage 2: Build Go binary (cross-compiles on build platform)
FROM --platform=$BUILDPLATFORM golang:1.26-alpine@sha256:3ad57304ad93bbec8548a0437ad9e06a455660655d9af011d58b993f6f615648 AS go-builder
ARG TARGETOS
ARG TARGETARCH
RUN apk add --no-cache git
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=ui-builder /app/ui/dist ./ui/dist
ARG VERSION=0.0.21
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
    -ldflags="-s -w -X 'github.com/voidmind-io/voidllm/internal/api/health.Version=${VERSION}'" \
    -o /voidllm ./cmd/voidllm

# Stage 3: Runtime
FROM alpine:3.21@sha256:48b0309ca019d89d40f670aa1bc06e426dc0931948452e8491e3d65087abc07d
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
