package main

import (
	"context"

	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var logger *zap.Logger

func initLogging(serviceName string) func() {
	config := zap.NewProductionConfig()
	config.EncoderConfig.TimeKey = "time"
	config.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	config.EncoderConfig.MessageKey = "msg"
	config.OutputPaths = []string{"stdout"}

	var err error
	logger, err = config.Build(zap.Fields(
		zap.String("service", serviceName),
	))
	if err != nil {
		panic(err)
	}

	return func() { _ = logger.Sync() }
}

func logFromContext(ctx context.Context) *zap.Logger {
	if logger == nil {
		l, _ := zap.NewProduction()
		return l
	}
	span := trace.SpanFromContext(ctx)
	if !span.SpanContext().IsValid() {
		return logger
	}
	return logger.With(
		zap.String("trace_id", span.SpanContext().TraceID().String()),
		zap.String("span_id", span.SpanContext().SpanID().String()),
	)
}

func sugarFromContext(ctx context.Context) *zap.SugaredLogger {
	return logFromContext(ctx).Sugar()
}
