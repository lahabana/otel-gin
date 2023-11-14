package observability

import (
	"context"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/version"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.21.0"

	"go.opentelemetry.io/otel/trace"
	"log/slog"
	"os"
	"time"
)

type OTLPFormat string

const (
	OTLPHttp = OTLPFormat("http")
	OTLPGrpc = OTLPFormat("grpc")
	OTLPNone = OTLPFormat("")
)

type Observability interface {
	Middleware() gin.HandlerFunc
	Logger() *slog.Logger
}

type obs struct {
	service string
	l       *slog.Logger
}

func (o *obs) Logger() *slog.Logger {
	return o.l
}

func Init(ctx context.Context, service string, logLevel slog.Level, metrics OTLPFormat, traces OTLPFormat) (Observability, error) {
	levelVar := &slog.LevelVar{}
	l := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		AddSource: true,
		Level:     levelVar,
	}))
	levelVar.Set(logLevel)
	host, _ := os.Hostname()
	res, err := resource.New(context.Background(),
		resource.WithAttributes(semconv.ServiceNameKey.String(service)),
		resource.WithAttributes(semconv.HostName(host)),
		resource.WithSchemaURL(semconv.SchemaURL),
	)
	if err != nil {
		return nil, err
	}
	traceProviderOpts := []sdktrace.TracerProviderOption{
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithResource(res),
	}
	switch traces {
	case OTLPHttp:
		l.InfoContext(ctx, "adding OTLP http to publish traces")
		traceExporter, err := otlptracehttp.New(ctx)
		if err != nil {
			return nil, err
		}
		traceProviderOpts = append(traceProviderOpts, sdktrace.WithBatcher(traceExporter))
	case OTLPGrpc:
		l.InfoContext(ctx, "adding OTLP grpc to publish traces")
		traceExporter, err := otlptracegrpc.New(ctx)
		if err != nil {
			return nil, err
		}
		traceProviderOpts = append(traceProviderOpts, sdktrace.WithBatcher(traceExporter))
	case OTLPNone:
		l.InfoContext(ctx, "no OTLP used to publish traces")
	default:
		return nil, fmt.Errorf("invalid otlp format for traces: %s valid formats: '', 'grpc' and 'http'", traces)
	}
	tracingProvider := sdktrace.NewTracerProvider(traceProviderOpts...)
	otel.SetTracerProvider(tracingProvider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))

	promExporter, err := prometheus.New()
	if err != nil {
		return nil, err
	}
	metricsProviderOpts := []sdkmetric.Option{
		sdkmetric.WithReader(promExporter),
		sdkmetric.WithResource(res),
	}
	switch metrics {
	case OTLPHttp:
		l.InfoContext(ctx, "adding OTLP http to publish metrics")
		metricsOtlpExporter, err := otlpmetrichttp.New(ctx)
		if err != nil {
			return nil, err
		}
		metricsProviderOpts = append(metricsProviderOpts,
			sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricsOtlpExporter, sdkmetric.WithInterval(time.Second))),
		)
	case OTLPGrpc:
		l.InfoContext(ctx, "adding OTLP grpc to publish metrics")
		metricsOtlpExporter, err := otlpmetricgrpc.New(ctx)
		if err != nil {
			return nil, err
		}
		metricsProviderOpts = append(metricsProviderOpts,
			sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricsOtlpExporter, sdkmetric.WithInterval(time.Second))),
		)
	case OTLPNone:
		l.InfoContext(ctx, "no OTLP used to publish metrics")
	default:
		return nil, fmt.Errorf("invalid otlp format for traces: %s valid formats: '', 'grpc' and 'http'", traces)
	}
	metricProvider := sdkmetric.NewMeterProvider(metricsProviderOpts...)
	otel.SetMeterProvider(metricProvider)

	o := &obs{l: l, service: service}
	go func() {
		<-ctx.Done()
		l.InfoContext(ctx, "Shutting down observability")
		if err := tracingProvider.Shutdown(context.Background()); err != nil {
			l.ErrorContext(ctx, "Error shutting down tracer provider", "error", err)
		}
		if err := metricProvider.Shutdown(context.Background()); err != nil {
			l.ErrorContext(ctx, "Error shutting down metric provider", "error", err)
		}
	}()
	return o, nil
}

func key(k interface{}) string {
	return fmt.Sprintf("gin.server.%s", k)
}

func attrsToArgs(in []attribute.KeyValue) []interface{} {
	var res []interface{}
	for _, entry := range in {
		res = append(res, string(entry.Key), entry.Value.AsInterface())
	}
	return res
}

func (o *obs) Middleware() gin.HandlerFunc {
	l := o.l.With("name", "gin-observability")
	meter := otel.Meter("github.com/lahabana/otel-gin", metric.WithInstrumentationVersion("1.0.0"))
	requestCount, _ := meter.Int64UpDownCounter(key("http.request.count"), metric.WithDescription("Number of Requests"), metric.WithUnit("Count"))
	totalDuration, _ := meter.Int64Histogram(key("http.request.duration"), metric.WithDescription("Time Taken by request"), metric.WithUnit("Milliseconds"))
	activeRequestsCounter, _ := meter.Int64UpDownCounter(key("http.request.inflight"), metric.WithDescription("Number of requests inflight"), metric.WithUnit("Count"))
	requestSize, _ := meter.Int64Histogram(key(semconv.HTTPRequestBodySizeKey), metric.WithDescription("Request Size"), metric.WithUnit("Bytes"))
	responseSize, _ := meter.Int64Histogram(key(semconv.HTTPResponseBodySizeKey), metric.WithDescription("Response Size"), metric.WithUnit("Bytes"))

	promHandler := promhttp.Handler()
	tracer := otel.GetTracerProvider().Tracer(
		"github.com/lahabana/otel-gin",
		trace.WithInstrumentationVersion(version.Version),
	)
	propagators := otel.GetTextMapPropagator()
	tracerKey := "gin-observability-trace-key"

	return func(c *gin.Context) {
		if c.Request.URL.Path == "/metrics" {
			promHandler.ServeHTTP(c.Writer, c.Request)
			return
		}
		c.Set(tracerKey, tracer)
		savedCtx := c.Request.Context()
		ctx := propagators.Extract(savedCtx, propagation.HeaderCarrier(c.Request.Header))

		route := c.FullPath()
		if route == "" {
			route = "unknown-route"
		}
		commonAttrs := []attribute.KeyValue{
			semconv.HTTPMethodKey.String(c.Request.Method),
			semconv.HTTPRoute(route),
			semconv.ServiceName(o.service),
		}
		traceAttrs := append([]attribute.KeyValue{
			semconv.URLFull(c.Request.URL.String()),
			semconv.URLPath(c.Request.URL.Path),
			semconv.URLQuery(c.Request.URL.RawQuery),
		}, commonAttrs...)
		opts := []trace.SpanStartOption{
			trace.WithAttributes(traceAttrs...),
			trace.WithSpanKind(trace.SpanKindServer),
		}
		ctx, span := tracer.Start(ctx, route, opts...)
		// Change the context to include the span
		c.Request = c.Request.WithContext(ctx)

		start := time.Now()
		activeRequestsCounter.Add(ctx, 1, metric.WithAttributes(commonAttrs...))

		defer func() {
			latency := time.Since(start)
			c.Writer.Size()
			activeRequestsCounter.Add(ctx, -1, metric.WithAttributes(commonAttrs...))

			metricsAttrs := append([]attribute.KeyValue{
				semconv.HTTPStatusCode(c.Writer.Status()),
			}, commonAttrs...)
			extraAttrs := []attribute.KeyValue{
				semconv.HTTPRequestBodySize(int(c.Request.ContentLength)),
				semconv.HTTPResponseBodySize(c.Writer.Size()),
				semconv.SourceAddress(c.ClientIP()),
				attribute.String("latency", time.Since(start).String()),
			}

			requestCount.Add(ctx, 1, metric.WithAttributes(metricsAttrs...))
			totalDuration.Record(ctx, latency.Milliseconds(), metric.WithAttributes(metricsAttrs...))
			requestSize.Record(ctx, c.Request.ContentLength, metric.WithAttributes(metricsAttrs...))
			responseSize.Record(ctx, int64(c.Writer.Size()), metric.WithAttributes(metricsAttrs...))
			if len(c.Errors) > 0 {
				extraAttrs = append(extraAttrs, attribute.String("gin.errors", c.Errors.String()))
			}

			l.DebugContext(c.Request.Context(), "request handled",
				append(attrsToArgs(traceAttrs), attrsToArgs(extraAttrs)...)...,
			)

			if c.Writer.Status() < 100 || c.Writer.Status() >= 600 {
				span.SetStatus(codes.Unset, fmt.Sprintf("Invalid HTTP status %d", c.Writer.Status()))
			} else if c.Writer.Status() < 300 {
				span.SetStatus(codes.Ok, fmt.Sprintf("HTTP status %d", c.Writer.Status()))
			} else {
				span.SetStatus(codes.Error, fmt.Sprintf("HTTP status: %d", c.Writer.Status()))
			}
			span.SetAttributes(extraAttrs...)
			if len(c.Errors) > 0 {
				span.SetAttributes(attribute.String("gin.errors", c.Errors.String()))
			}
			span.End()
			c.Request = c.Request.WithContext(savedCtx)
		}()

		// Process the request
		c.Next()
	}
}
