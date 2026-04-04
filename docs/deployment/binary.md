---
title: "Binary Deployment"
description: "Run VoidLLM as a standalone binary on Linux, macOS, or Windows - no Docker required."
section: deployment
order: 0
---
# Binary Deployment

VoidLLM ships as a single binary (~15 MB) with the web UI embedded. No runtime dependencies, no containers required.

## Download

Download the latest binary for your platform from the [releases page](https://github.com/voidmind-io/voidllm/releases/latest) or from [voidllm.ai/download](https://voidllm.ai/download).

### Linux

    curl -sL https://github.com/voidmind-io/voidllm/releases/latest/download/voidllm-linux-amd64.tar.gz | tar xz

For ARM64 (Raspberry Pi, AWS Graviton):

    curl -sL https://github.com/voidmind-io/voidllm/releases/latest/download/voidllm-linux-arm64.tar.gz | tar xz

### macOS

    # Apple Silicon (M1/M2/M3)
    curl -sL https://github.com/voidmind-io/voidllm/releases/latest/download/voidllm-darwin-arm64.tar.gz | tar xz

    # Intel
    curl -sL https://github.com/voidmind-io/voidllm/releases/latest/download/voidllm-darwin-amd64.tar.gz | tar xz

macOS may show a security warning on first run. Allow it in System Settings > Privacy & Security.

### Windows

Download `voidllm-windows-amd64.zip` from the [releases page](https://github.com/voidmind-io/voidllm/releases/latest) and extract it.

Windows SmartScreen may show "Windows protected your PC" on first run. Click "More info" then "Run anyway".

## Required Secrets

VoidLLM needs two secrets to start. Generate them once and keep them safe - changing the encryption key after data is stored will make encrypted values unreadable.

### Linux / macOS

    export VOIDLLM_ADMIN_KEY=$(openssl rand -base64 32)
    export VOIDLLM_ENCRYPTION_KEY=$(openssl rand -base64 32)
    ./voidllm

### Windows (PowerShell)

    $env:VOIDLLM_ADMIN_KEY = [Convert]::ToBase64String((1..32 | ForEach-Object { Get-Random -Max 256 }) -as [byte[]])
    $env:VOIDLLM_ENCRYPTION_KEY = [Convert]::ToBase64String((1..32 | ForEach-Object { Get-Random -Max 256 }) -as [byte[]])
    .\voidllm.exe

Save these values somewhere secure. You will need the encryption key if you move or restore the database.

## First Start

On first start, VoidLLM creates a SQLite database (`voidllm.db`) in the current directory and prints bootstrap credentials:

    ========================================
     BOOTSTRAP COMPLETE - COPY THESE NOW
    ========================================
      API Key:    vl_uk_a3f2...
      Email:      admin@voidllm.local
      Password:   <random>
    ========================================

Open http://localhost:8080, log in with the email and password above. These credentials are shown once.

## Configuration

Without a config file, VoidLLM uses sensible defaults:
- Database: `./voidllm.db` (SQLite in current directory)
- Port: 8080
- All features: community edition

For advanced configuration, create a `voidllm.yaml` in the same directory:

    server:
      proxy:
        port: 8080

    models:
      - name: my-model
        provider: ollama
        base_url: http://localhost:11434/v1

    settings:
      admin_key: ${VOIDLLM_ADMIN_KEY}
      encryption_key: ${VOIDLLM_ENCRYPTION_KEY}

VoidLLM auto-discovers `voidllm.yaml` in the current directory. Use `--config /path/to/config.yaml` to specify a different location.

## Environment Variables

For config-less operation (no YAML file), these environment variables are supported:

| Variable | Required | Description |
|---|---|---|
| `VOIDLLM_ADMIN_KEY` | First start | Bootstrap admin key (min 32 chars) |
| `VOIDLLM_ENCRYPTION_KEY` | Yes | AES-256-GCM key for encryption |
| `VOIDLLM_DATABASE_DSN` | No | Database path (default: `./voidllm.db`) |
| `VOIDLLM_DATABASE_DRIVER` | No | Database driver (default: `sqlite`, alternative: `postgres`) |
| `VOIDLLM_LICENSE` | No | Enterprise license JWT |

## Running as a Service

### Linux (systemd)

Create `/etc/systemd/system/voidllm.service`:

    [Unit]
    Description=VoidLLM LLM Proxy
    After=network.target

    [Service]
    Type=simple
    User=voidllm
    WorkingDirectory=/opt/voidllm
    ExecStart=/opt/voidllm/voidllm --config /opt/voidllm/voidllm.yaml
    Restart=on-failure
    RestartSec=5
    Environment=VOIDLLM_ADMIN_KEY=your-admin-key-here
    Environment=VOIDLLM_ENCRYPTION_KEY=your-encryption-key-here

    [Install]
    WantedBy=multi-user.target

Then:

    sudo systemctl daemon-reload
    sudo systemctl enable --now voidllm

### macOS (launchd)

Create `~/Library/LaunchAgents/io.voidmind.voidllm.plist` or use a process manager like `brew services`.

### Windows

Use NSSM (Non-Sucking Service Manager) or Task Scheduler to run `voidllm.exe` as a background service.

## Updating

Download the new binary and replace the old one. The database is preserved - no migration steps needed (migrations run automatically on startup).

    # Linux/macOS
    curl -sL https://github.com/voidmind-io/voidllm/releases/latest/download/voidllm-linux-amd64.tar.gz | tar xz
    # Restart the service

## Connecting to Ollama

If Ollama runs on the same machine, use `http://localhost:11434/v1` as the base URL. If VoidLLM runs in Docker but Ollama runs on the host, use `http://host.docker.internal:11434/v1` instead.
