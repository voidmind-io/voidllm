---
title: "OpenTelemetry"
description: "Distributed tracing with Jaeger, Tempo, or Datadog"
section: enterprise
order: 4
---
# OpenTelemetry Tracing

Export distributed traces to Jaeger, Grafana Tempo, Datadog, or any OTLP-compatible backend. Requires an Enterprise license.

## Enable

```yaml
settings:
  otel:
    enabled: true
    endpoint: "tempo:4317"        # OTLP gRPC endpoint
    insecure: true                # set to false for TLS
    sample_rate: 1.0              # 1.0 = trace every request
```

## What's Traced

| Span | Description |
|---|---|
| `proxy.handle` | Root span covering the entire proxy request lifecycle |
| `proxy.upstream` | Child span measuring time-to-first-byte from the LLM provider |

Attributes include: model name, provider, status code, token counts, duration, and request ID.

## Trace Propagation

Trace context (`traceparent` header) is propagated to upstream providers for end-to-end distributed tracing. If your LLM provider supports OpenTelemetry, you get a complete trace from client -> VoidLLM -> provider.

## Log Correlation

When tracing is active, every log line automatically includes `trace_id` and `span_id`. This makes it easy to find related logs for a specific trace in tools like Grafana Loki or Elasticsearch.

Example log entry:
```json
{
  "time": "2026-04-01T12:00:00Z",
  "level": "INFO",
  "msg": "proxy request",
  "trace_id": "abc123...",
  "span_id": "def456...",
  "model": "gpt-4o",
  "status": 200
}
```

## Sample Rate

For high-traffic deployments, reduce the sample rate to control tracing volume:

```yaml
settings:
  otel:
    sample_rate: 0.1    # trace 10% of requests
```

A sample rate of 1.0 traces every request. 0.1 traces 10%. 0.0 disables tracing while keeping the configuration in place.

## Backend Setup Examples

### Grafana Tempo

```yaml
settings:
  otel:
    enabled: true
    endpoint: "tempo:4317"
    insecure: true
```

### Jaeger

```yaml
settings:
  otel:
    enabled: true
    endpoint: "jaeger:4317"
    insecure: true
```

### Datadog

```yaml
settings:
  otel:
    enabled: true
    endpoint: "datadog-agent:4317"
    insecure: true
```

Datadog requires the OTLP ingest to be enabled on the Datadog Agent.
