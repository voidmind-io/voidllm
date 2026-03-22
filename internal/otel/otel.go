// Package otel initialises the OpenTelemetry SDK for VoidLLM.
// It is intentionally thin: it wires a single OTLP/gRPC exporter, sets up the
// global TracerProvider and propagator, and returns a shutdown function the
// caller must invoke during application shutdown to flush buffered spans.
package otel

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Setup initialises the OpenTelemetry TracerProvider with an OTLP gRPC exporter
// and registers it as the global provider. The returned shutdown function must
// be called during application shutdown to flush and close the exporter. On
// error, a no-op shutdown function is returned alongside the error so the
// caller can always safely defer the result without a nil check.
func Setup(ctx context.Context, endpoint string, insecureTLS bool, sampleRate float64, serviceName, serviceVersion string) (func(context.Context) error, error) {
	var dialOpts []grpc.DialOption
	if insecureTLS {
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}

	exporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(endpoint),
		otlptracegrpc.WithDialOption(dialOpts...),
	)
	if err != nil {
		return noop, fmt.Errorf("otel: create otlp exporter: %w", err)
	}

	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName(serviceName),
			semconv.ServiceVersion(serviceVersion),
		),
	)
	if err != nil {
		// resource.Merge only fails when schema URLs conflict — treat it as fatal
		// so the operator knows the resource descriptor is malformed.
		_ = exporter.Shutdown(ctx) //nolint:errcheck // best-effort on error path
		return noop, fmt.Errorf("otel: build resource: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.TraceIDRatioBased(sampleRate)),
	)

	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	return tp.Shutdown, nil
}

// noop is the no-op shutdown function returned on error paths so callers can
// always safely defer the result of Setup without a nil check.
func noop(_ context.Context) error { return nil }
