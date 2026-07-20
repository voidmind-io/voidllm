---
title: "PII Anonymization"
description: "Opt-in identifier pseudonymization for outbound prompts - what it protects, what it does not"
section: security
order: 3
---
# PII Anonymization

VoidLLM can replace personal identifiers in outbound prompts with deterministic pseudonyms before the request reaches an external provider, and restore the original values in the response. The provider never sees the real identifier; your client sees its own data unchanged.

This is **opt-in and off by default** (`settings.pii.enabled`).

> **Status: early / beta.** The feature is new and still evolving. Detection today is regular expressions plus curated word lists - there is no NER model, so coverage is finite and names are only found if they are in a list. The zero-knowledge and fail-closed properties described here are implemented and tested, including against live provider endpoints, but the feature has limited production mileage. Treat it as a strong reduction of re-identification risk, not as a completed privacy guarantee, and validate it against your own data before relying on it.

> **Read the [limits](#what-it-does-not-protect) section before enabling this.** Identifier pseudonymization is frequently misunderstood, and the wrong mental model creates false confidence.

## How it works

```
client                    VoidLLM                     provider
  |                          |                            |
  |-- "Call Tina about   --->|                            |
  |    the invoice"          |-- "Call PII_NM_4f2c9a... ->|
  |                          |    about the invoice"      |
  |                          |                            |
  |                          |<-- "I called PII_NM_4f2c9a..."
  |<-- "I called Tina"  <----|                            |
```

1. The request body is scanned in memory for known identifier patterns.
2. Each match is replaced with a pseudonym: `PII_<2-char type>_<24 hex>` (31 characters).
3. The anonymized body goes upstream. The mapping exists only in RAM, for the lifetime of the request.
4. Pseudonyms in the response - including streamed content and tool-call arguments - are replaced with the original values before the response reaches the client.

Pseudonyms are derived as `HMAC-SHA256(secret, orgID || type || normalized value)`. They are stable: the same value always yields the same pseudonym within an organization, so multi-turn conversations stay coherent without storing any mapping.

Nothing is persisted. No prompt content, no PII, and no pseudonyms are ever written to the database, logs, or traces.

## What it does not protect

**This is an identifier firewall, not a confidentiality firewall.** It removes the link between a statement and a named person. It does not make the statement itself private.

Consider:

> "Tina has been diagnosed with hepatitis."

The provider receives:

> "PII_NM_4f2c9a1be07d35ac8b61e02d has been diagnosed with hepatitis."

The provider still learns that **someone has that diagnosis** - health data, a special category under GDPR Article 9. It only fails to learn **who**. In most sensitive sentences the name is the least sensitive part.

Three further limits worth internalizing:

- **You are not anonymous.** The API key, account, organization, and billing identity are known to the provider. Only identifiers of third parties *mentioned in the text* are masked.
- **Stable pseudonyms allow pseudonymous profiling.** Determinism is required for multi-turn coherence, but it also means a provider can accumulate everything ever said about `PII_NM_cb0aaf...` across requests - a dossier without a name attached.
- **Coverage is finite.** Only what the detectors recognize is masked. Everything else passes through in clear text.

If you need the *statement* withheld, pseudonymization is the wrong tool. Do not send the prompt to an external provider at all - route it to a self-hosted model instead.

## What is detected

**Structured identifiers (regex, always on when PII is enabled):**

| Type | Code | Notes |
|---|---|---|
| Email | `EM` | |
| IBAN | `IB` | German format (`DE` + 20 digits) |
| Phone | `PH` | German formats |
| Credit card | `CC` | 13-19 digits |
| Tax ID | `TX` | German 11-digit |

**Unstructured terms (gazetteer, separately opt-in):** names, places, company forms, and any term list you supply. Matching is whole-token and case-insensitive.

Names are only detected if they appear in a loaded list. A name that is not in any list - an uncommon given name, a surname, a nickname - is sent in clear text. There is no NER model; transformer-based detection is planned.

Not detected, by design: free-form facts, diagnoses, addresses, dates of birth, non-German identifier formats, and anything else without a pattern or list entry.

## Configuration

```yaml
settings:
  pii:
    enabled: true
    action: pseudonymize

    # optional: extra structured patterns on top of the built-ins
    patterns:
      - type: PASSPORT_NO
        regexp: '\b[A-Z]{1,3}[0-9]{6,9}\b'

    gazetteer:
      enabled: true

      # bundled, curated packs (opt-in by name)
      packs:
        - company-forms      # legal forms: GmbH, AG, Ltd, Inc, ...
        - de-firstnames      # common German given names
        - de-cities          # major German cities

      # your own term lists, any language
      dirs:
        - /etc/voidllm/gazetteers

      # inline terms, handy for a short blocklist
      terms:
        - type: NAME
          values: ["Ayşe", "Kwame"]
        - type: PROJECT
          values: ["Projekt Nordstern"]
        - type: CUSTOMER
          values: ["Nordwind Logistik", "Familie Özdemir"]
```

### Extending the lists

Bundled packs are deliberately small and conservative. The lists you add yourself are usually the highest-value ones, because you know your own sensitive terms: employee names, customer names, project code names, internal system names.

A gazetteer file in a `dirs` directory is plain text:

```
# locale: de
# type: CUSTOMER
Nordwind Logistik
Familie Özdemir
```

- One term per line; `#` starts a comment. Multi-word terms are supported.
- `# type:` is required. Any label works; unknown labels get a stable two-character code in the pseudonym (`PROJECT` becomes `PR`, `CUSTOMER` becomes `CU`).
- `# locale:` is metadata only. The matcher is language-neutral, so lists of any language can be loaded together.
- Files are read at startup. Restart to pick up changes.

## Per-model behavior

By default, anonymization applies to **public** destinations only. Private and loopback upstreams (a self-hosted vLLM, Ollama on the same network) receive the original text, because the data never leaves your trust boundary.

Override per model or deployment:

```yaml
models:
  - name: internal-llama
    provider: vllm
    base_url: http://vllm:8000/v1
    pii_filter: true      # anonymize even though the destination is private

  - name: trusted-partner
    provider: openai
    base_url: https://api.partner.example/v1
    pii_filter: false     # WARNING: sends PII in clear text to this upstream
```

## Operational notes

- **Models may notice pseudonyms.** A pseudonym is an odd-looking token. Models sometimes comment on it, refuse to repeat it, or handle it awkwardly. Expect some quality impact on text that is heavily pseudonymized.
- **False positives mask real text.** A gazetteer term that is also an everyday word will be replaced wherever it appears. The bundled lists exclude known collisions; apply the same care to your own lists.
- **Ambiguity fails closed.** If a request or response cannot be processed safely, it is rejected (HTTP 422) or the stream is aborted, rather than forwarding unprotected content. Enabling PII therefore trades some availability for safety.
- **Restoration covers response bodies and streams**, but not response headers. A provider that echoed a pseudonym into a header would pass it through. No realistic vector is known.

## Related

- [Privacy Architecture](privacy.md) - the zero-knowledge design this builds on
- [Configuration Reference](../configuration.md)
