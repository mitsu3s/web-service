package main

import (
	"context"

	amqp "github.com/rabbitmq/amqp091-go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

const (
	traceparentHeader = "traceparent"
	tracestateHeader  = "tracestate"
)

type amqpHeaderCarrier amqp.Table

func (c amqpHeaderCarrier) Get(key string) string {
	if value, ok := c[key]; ok {
		if text, ok := value.(string); ok {
			return text
		}
	}
	return ""
}

func (c amqpHeaderCarrier) Set(key, value string) {
	if value != "" {
		c[key] = value
	}
}

func (c amqpHeaderCarrier) Keys() []string {
	keys := make([]string, 0, len(c))
	for key := range c {
		keys = append(keys, key)
	}
	return keys
}

func traceContextFromContext(ctx context.Context) (string, string) {
	headers := amqp.Table{}
	otel.GetTextMapPropagator().Inject(ctx, amqpHeaderCarrier(headers))
	return stringHeader(headers, traceparentHeader), stringHeader(headers, tracestateHeader)
}

func publishOutboxEvent(ctx context.Context, publisher *amqpPublisher, evt OutboxEvent) error {
	ctx = otel.GetTextMapPropagator().Extract(ctx, propagation.MapCarrier{
		traceparentHeader: evt.Traceparent,
		tracestateHeader:  evt.Tracestate,
	})

	ctx, span := otel.Tracer("devboard/rabbitmq").Start(ctx, "publish "+evt.RoutingKey,
		trace.WithSpanKind(trace.SpanKindProducer),
		trace.WithAttributes(
			attribute.String("messaging.system", "rabbitmq"),
			attribute.String("messaging.destination.name", "devboard.events"),
			attribute.String("messaging.operation.name", "publish"),
			attribute.String("messaging.rabbitmq.routing_key", evt.RoutingKey),
			attribute.String("messaging.message.id", evt.EventID),
		),
	)
	defer span.End()

	headers := amqp.Table{}
	otel.GetTextMapPropagator().Inject(ctx, amqpHeaderCarrier(headers))
	if err := publisher.Publish(ctx, evt.RoutingKey, []byte(evt.Payload), headers); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	return nil
}

func stringHeader(headers amqp.Table, key string) string {
	if value, ok := headers[key]; ok {
		if text, ok := value.(string); ok {
			return text
		}
	}
	return ""
}
