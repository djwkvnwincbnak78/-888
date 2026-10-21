# Architecture Overview

## Layered Architecture

### 1. API Layer
- HTTP/REST endpoints
- OpenAI-compatible proxy
- gRPC support (planned)

### 2. Routing Layer
- Multi-dimensional router (cost/latency/fixed)
- Error-type based fallback
- Streaming support
- Rate limiting (4-layer)

### 3. Provider Layer
- OpenAI / Anthropic adapters
- Protocol conversion (request/response, streaming)
- Semantic caching layer

### 4. Data Layer
- PostgreSQL with time-based partitioning
- Semantic + exact caching
- Metrics & Observability

### 5. Billing & Security
- Token budget management
- SSRF protection
- Audit logging

### Key Features
- Race-free design (no append loops)
- Production-ready with Go build verification at each step
- Full observability