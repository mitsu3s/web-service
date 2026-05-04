package main

import (
	"context"

	amqp "github.com/rabbitmq/amqp091-go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
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

func startAMQPConsumerSpan(ctx context.Context, msg amqp.Delivery, eventType, eventID string, taskID, projectID uint) (context.Context, trace.Span) {
	ctx = otel.GetTextMapPropagator().Extract(ctx, amqpHeaderCarrier(msg.Headers))
	return otel.Tracer("devboard/rabbitmq").Start(ctx, "consume "+eventType,
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(
			attribute.String("messaging.system", "rabbitmq"),
			attribute.String("messaging.destination.name", "devboard.events"),
			attribute.String("messaging.operation.name", "process"),
			attribute.String("messaging.rabbitmq.routing_key", msg.RoutingKey),
			attribute.String("messaging.message.id", eventID),
			attribute.String("devboard.event.type", eventType),
			attribute.Int64("devboard.task.id", int64(taskID)),
			attribute.Int64("devboard.project.id", int64(projectID)),
		),
	)
}
