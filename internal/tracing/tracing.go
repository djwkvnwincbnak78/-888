package tracing

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	otelTrace "go.opentelemetry.io/otel/trace"
)

const tracerName = "gateway"

// Config controls the TracerProvider.
type Config struct {
	ServiceName  string
	Version      string
	Exporter     string  // none | stdout | otlp
	OTLPEndpoint string  // host:4318, used when Exporter=otlp
	SampleRatio  float64 // 0..1, default 1
}

var (
	setupOnce sync.Once
	provider  *sdktrace.TracerProvider
)

// ensureDefault installs a sampling TracerProvider so spans created in tests
// (and before Init) still carry valid W3C identifiers. It never exports.
func ensureDefault() {
	setupOnce.Do(installInMemory)
}

// installInMemory installs a sampling, non-exporting provider. It is also used
// after Shutdown so spans created later still carry valid identifiers (tests,
// or a process that shuts tracing down before exiting).
func installInMemory() {
	provider = sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.AlwaysSample())),
	)
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))
}

// Init installs the production TracerProvider and propagator. The returned
// function flushes spans and must be called on shutdown.
func Init(ctx context.Context, cfg Config) (func(context.Context) error, error) {
	ensureDefault()

	if cfg.ServiceName == "" {
		cfg.ServiceName = "super-llm-gateway"
	}
	if cfg.SampleRatio <= 0 || cfg.SampleRatio > 1 {
		cfg.SampleRatio = 1
	}

	res, err := resource.New(ctx, resource.WithAttributes(
		semconv.ServiceName(cfg.ServiceName),
		semconv.ServiceVersion(cfg.Version),
	))
	if err != nil {
		return nil, fmt.Errorf("tracing: resource: %w", err)
	}

	opts := []sdktrace.TracerProviderOption{
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SampleRatio))),
	}

	switch strings.ToLower(cfg.Exporter) {
	case "stdout":
		exp, err := stdouttrace.New(stdouttrace.WithPrettyPrint())
		if err != nil {
			return nil, fmt.Errorf("tracing: stdout exporter: %w", err)
		}
		opts = append(opts, sdktrace.WithBatcher(exp))
	case "otlp":
		endpoint := cfg.OTLPEndpoint
		if endpoint == "" {
			endpoint = os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
		}
		expOpts := []otlptracehttp.Option{}
		if endpoint != "" {
			expOpts = append(expOpts, otlptracehttp.WithEndpoint(endpoint))
		}
		// OTLP over plain HTTP for sidecars/collectors on a private network.
		expOpts = append(expOpts, otlptracehttp.WithInsecure())
		exp, err := otlptracehttp.New(ctx, expOpts...)
		if err != nil {
			return nil, fmt.Errorf("tracing: otlp exporter: %w", err)
		}
		opts = append(opts, sdktrace.WithBatcher(exp))
	default:
		// none: keep identifiers/propagation, export nothing.
	}

	tp := sdktrace.NewTracerProvider(opts...)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))
	provider = tp
	return func(ctx context.Context) error {
		err := tp.Shutdown(ctx)
		installInMemory()
		return err
	}, nil
}

// Span wraps an OTel span while exposing the identifiers the gateway logs and
// forwards upstream.
type Span struct {
	TraceID string
	SpanID  string
	Name    string
	Started time.Time
	Ended   time.Time

	sp otelTrace.Span
}

type ctxKey struct{}

// Start begins a child span, adopting a remote parent from ctx when present.
func Start(ctx context.Context, name string) (context.Context, *Span) {
	ensureDefault()
	ctx, sp := otel.Tracer(tracerName).Start(ctx, name)
	sc := sp.SpanContext()

	s := &Span{
		TraceID: sc.TraceID().String(),
		SpanID:  sc.SpanID().String(),
		Name:    name,
		Started: time.Now(),
		sp:      sp,
	}
	return context.WithValue(ctx, ctxKey{}, s), s
}

// End finishes the span.
func (s *Span) End() {
	if s == nil {
		return
	}
	s.Ended = time.Now()
	if s.sp != nil {
		s.sp.End()
	}
}

// Duration returns elapsed time.
func (s *Span) Duration() time.Duration {
	if s == nil {
		return 0
	}
	if s.Ended.IsZero() {
		return time.Since(s.Started)
	}
	return s.Ended.Sub(s.Started)
}

// Traceparent renders the W3C header value.
func (s *Span) Traceparent() string {
	if s == nil {
		return ""
	}
	return fmt.Sprintf("00-%s-%s-01", s.TraceID, s.SpanID)
}

// SetAttribute adds a span attribute.
func (s *Span) SetAttribute(key string, value any) {
	if s == nil || s.sp == nil {
		return
	}
	s.sp.SetAttributes(toAttr(key, value))
}

// FromContext returns the gateway span stored by Start.
func FromContext(ctx context.Context) (*Span, bool) {
	s, ok := ctx.Value(ctxKey{}).(*Span)
	return s, ok && s != nil
}

// TraceID returns the active trace id, or "".
func TraceID(ctx context.Context) string {
	if s, ok := FromContext(ctx); ok {
		return s.TraceID
	}
	return ""
}

// WithTraceparent extracts an inbound W3C header into ctx as a remote parent.
func WithTraceparent(ctx context.Context, header string) context.Context {
	ensureDefault()
	if strings.TrimSpace(header) == "" {
		return ctx
	}
	return otel.GetTextMapPropagator().Extract(ctx,
		propagation.MapCarrier{"traceparent": header})
}

// ParseTraceparent validates the W3C "00-<32hex>-<16hex>-<flags>" format.
func ParseTraceparent(h string) (traceID, spanID string, ok bool) {
	parts := strings.Split(strings.TrimSpace(h), "-")
	if len(parts) != 4 || parts[0] != "00" {
		return "", "", false
	}
	if len(parts[1]) != 32 || len(parts[2]) != 16 {
		return "", "", false
	}
	if _, err := hex.DecodeString(parts[1]); err != nil {
		return "", "", false
	}
	if _, err := hex.DecodeString(parts[2]); err != nil {
		return "", "", false
	}
	return parts[1], parts[2], true
}

func toAttr(key string, value any) attribute.KeyValue {
	switch v := value.(type) {
	case string:
		return attribute.String(key, v)
	case int:
		return attribute.Int(key, v)
	case int64:
		return attribute.Int64(key, v)
	case bool:
		return attribute.Bool(key, v)
	case float64:
		return attribute.Float64(key, v)
	default:
		return attribute.String(key, fmt.Sprint(v))
	}
}
