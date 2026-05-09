package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func initTracing(serviceName string) func() {
	if tracingDisabled() {
		return func() {}
	}

	ctx := context.Background()
	exporter, err := otlptracehttp.New(ctx)
	if err != nil {
		log.Printf("OpenTelemetry tracing disabled: %v", err)
		return func() {}
	}

	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
		"",
		attribute.String("service.name", otelEnv("OTEL_SERVICE_NAME", serviceName)),
		attribute.String("service.namespace", "devboard"),
	))
	if err != nil {
		log.Printf("OpenTelemetry resource setup failed: %v", err)
		return func() {}
	}

	provider := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithSampler(traceSampler()),
		sdktrace.WithBatcher(exporter),
	)
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := provider.Shutdown(ctx); err != nil {
			log.Printf("OpenTelemetry shutdown failed: %v", err)
		}
	}
}

func otelEnv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func tracingDisabled() bool {
	if strings.EqualFold(os.Getenv("OTEL_TRACES_EXPORTER"), "none") {
		return true
	}
	return os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" && os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") == ""
}

func traceSampler() sdktrace.Sampler {
	ratio := 1.0
	if raw := os.Getenv("OTEL_TRACES_SAMPLER_ARG"); raw != "" {
		if parsed, err := strconv.ParseFloat(raw, 64); err == nil && parsed >= 0 && parsed <= 1 {
			ratio = parsed
		}
	}

	switch strings.ToLower(os.Getenv("OTEL_TRACES_SAMPLER")) {
	case "always_off":
		return sdktrace.NeverSample()
	case "traceidratio":
		return sdktrace.TraceIDRatioBased(ratio)
	case "parentbased_traceidratio":
		return sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))
	default:
		return sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))
	}
}

func tracedHTTPHandler(_ string, handler http.Handler) http.Handler {
	return otelhttp.NewHandler(handler, "http.server",
		otelhttp.WithFilter(traceRequestFilter),
		otelhttp.WithSpanNameFormatter(httpSpanName),
	)
}

func traceRequestFilter(r *http.Request) bool {
	return r.URL.Path != "/health" && r.URL.Path != "/metrics"
}

func httpSpanName(_ string, r *http.Request) string {
	return r.Method + " " + normalizedPath(r.URL.Path)
}

func normalizedPath(path string) string {
	if path == "" || path == "/" {
		return "/"
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	for i, part := range parts {
		if _, err := strconv.ParseUint(part, 10, 64); err == nil {
			parts[i] = ":id"
		}
	}
	return "/" + strings.Join(parts, "/")
}
