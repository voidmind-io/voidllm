---
title: "Load Balancing"
description: "Multi-deployment routing strategies, failover, and circuit breakers"
section: models
order: 2
---
# Load Balancing and Failover

Put multiple deployments behind a single model name. VoidLLM handles routing, failover, and health-aware traffic management.

## Multi-Deployment Models

```yaml
models:
  - name: gpt-4o
    strategy: round-robin
    max_retries: 2
    aliases: [default]
    deployments:
      - name: azure-east
        provider: azure
        base_url: https://eastus.openai.azure.com
        api_key: ${AZURE_EAST_KEY}
        azure_deployment: my-gpt4o-east
      - name: azure-west
        provider: azure
        base_url: https://westus.openai.azure.com
        api_key: ${AZURE_WEST_KEY}
        azure_deployment: my-gpt4o-west
      - name: openai-direct
        provider: openai
        base_url: https://api.openai.com/v1
        api_key: ${OPENAI_KEY}
```

Your app sends `model: "default"`. VoidLLM picks a deployment based on the strategy.

## Routing Strategies

| Strategy | Description | Best for |
|---|---|---|
| `round-robin` | Equal distribution, next in rotation | Default, even load |
| `least-latency` | Routes to the deployment with lowest recent P50 | Latency-sensitive apps |
| `weighted` | Distribute by configured weight | Cost optimization (cheap provider gets more) |
| `priority` | Always use highest-priority, fall back when down | Primary/backup scenarios |

### Weighted Example

```yaml
deployments:
  - name: cheap-provider
    weight: 3              # gets 75% of traffic
  - name: premium-provider
    weight: 1              # gets 25% of traffic
```

### Priority Example

```yaml
deployments:
  - name: primary
    priority: 1            # used first (lower = higher priority)
  - name: backup
    priority: 2            # used only when primary is down
```

## Automatic Failover

When a deployment returns 5xx, times out, or can't connect, VoidLLM retries on the next available deployment. The `max_retries` setting controls how many deployments to try before giving up.

This happens transparently - the client sees a normal response. Usage tracking records which deployment actually handled the request.

## Circuit Breakers

Each deployment has its own circuit breaker. After consecutive failures, the circuit opens and the deployment is temporarily skipped.

```yaml
settings:
  circuit_breaker:
    enabled: true
    threshold: 5           # failures before opening
    timeout: 30s           # how long to stay open
    half_open_max: 1       # test requests before closing
```

## Health-Aware Routing

VoidLLM continuously probes each deployment's health. Unhealthy deployments are excluded from routing automatically. If all deployments are unhealthy, VoidLLM falls back to trying them anyway (better than returning nothing).

```yaml
settings:
  health_check:
    health:
      enabled: true
      interval: 30s
    functional:
      enabled: true
      interval: 5m
```

The functional probe sends a minimal request to each deployment (e.g., a 1-token completion) to verify end-to-end connectivity. The health probe checks basic reachability.

## Important: Same Model Across Deployments

All deployments within a model should serve the same (or equivalent) LLM. VoidLLM sends the model name from the request to every upstream, so each deployment must recognize it.

This works for:
- Same model across regions (Azure East + West)
- Same model across providers (Azure + OpenAI direct)

This does **not** work for mixing different LLMs (e.g., GPT-4o + Llama 70B) because vLLM wouldn't recognize "gpt-4o" as a valid model name. Cross-model failover chains are on the Enterprise roadmap.

## Community Feature

Load balancing, failover, circuit breakers, and health probing are all included in the free Community tier.
