# Competitive Analysis: Nexus

## Competitors

### Bifrost
- **Strength**: High-performance Go implementation with semantic caching.
- **Weakness**: Limited protocol support (primarily OpenAI-compatible).

### Portkey
- **Strength**: PII data guarding and Circuit Breaker patterns.
- **Weakness**: Less focus on multi-protocol translation and billing integration.

### New API
- **Strength**: Supports multiple protocols with unified billing.
- **Weakness**: Potentially higher latency due to broader protocol handling.

### LiteLLM
- **Strength**: Multi-dimensional routing (cost, latency, fixed), error-based fallback, streaming support.
- **Weakness**: Not optimized for Go-native performance or semantic caching at scale.

**Conclusion**: Nexus integrates the best of all worlds: Go performance + semantic caching + PII protection + multi-protocol + advanced routing + billing.