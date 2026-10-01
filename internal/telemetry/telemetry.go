package telemetry

import (
	"context"
	"errors"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/sdk/trace"
)

const serviceName = "healthchecker"

// setupOTelSDK bootstraps the OpenTelemetry pipeline.
// If it does not return an error, make sure to call shutdown for proper cleanup.
func SetupOtelSDK(ctx context.Context) (func(context.Context) error, error) {
	var shutdownFuncs []func(context.Context) error
	var err error

	// shutdown calls cleanup functions registered via shutdownFuncs.
	// The errors from the calls are joined.
	// Each registered cleanup will be invoked once.
	shutdown := func(ctx context.Context) error {
		var err error
		for _, fn := range shutdownFuncs {
			err = errors.Join(err, fn(ctx))
		}
		shutdownFuncs = nil
		return err
	}

	// handleErr calls shutdown for cleanup and makes sure that all errors are returned.
	handleErr := func(inErr error) {
		err = errors.Join(inErr, shutdown(ctx))
	}

	if os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != "" || os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" {
		prop := newPropagator()
		otel.SetTextMapPropagator(prop)

		sampler, err := samplerFromConfig(os.Getenv("OTEL_TRACES_SAMPLER"), os.Getenv("OTEL_TRACES_SAMPLER_ARG"))
		if err != nil {
			handleErr(err)
			return shutdown, err
		}

		tracerProvider, err := newTracerProvider(ctx, sampler)
		if err != nil {
			handleErr(err)
			return shutdown, err
		}
		shutdownFuncs = append(shutdownFuncs, tracerProvider.Shutdown)
		otel.SetTracerProvider(tracerProvider)
	}

	// Set up log export only when an explicit logs endpoint is configured.
	if os.Getenv("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT") != "" || os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" {
		loggerProvider, err := newLoggerProvider(ctx)
		if err != nil {
			handleErr(err)
			return shutdown, err
		}
		shutdownFuncs = append(shutdownFuncs, loggerProvider.Shutdown)
		global.SetLoggerProvider(loggerProvider)
	}

	return shutdown, err
}

func newPropagator() propagation.TextMapPropagator {
	return propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	)
}

func newTracerProvider(ctx context.Context, sampler trace.Sampler) (*trace.TracerProvider, error) {
	traceExporter, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, err
	}

	tracerProvider := trace.NewTracerProvider(
		trace.WithSampler(sampler),
		trace.WithResource(resource.NewSchemaless(attribute.String("service.name", serviceName))),
		trace.WithBatcher(traceExporter,
			// Default is 5s. Set to 1s for demonstrative purposes.
			trace.WithBatchTimeout(time.Second)),
	)
	return tracerProvider, nil
}

func samplerFromConfig(name, arg string) (trace.Sampler, error) {
	if name == "" {
		name = "parentbased_always_on"
	}

	switch strings.ToLower(strings.TrimSpace(name)) {
	case "always_on":
		return trace.AlwaysSample(), nil
	case "always_off":
		return trace.NeverSample(), nil
	case "parentbased_always_on":
		return trace.ParentBased(trace.AlwaysSample()), nil
	case "parentbased_always_off":
		return trace.ParentBased(trace.NeverSample()), nil
	case "traceidratio", "parentbased_traceidratio":
		ratio := 1.0
		if arg != "" {
			parsedRatio, err := strconv.ParseFloat(arg, 64)
			if err != nil {
				return nil, errors.New("OTEL_TRACES_SAMPLER_ARG must be a number between 0 and 1")
			}
			ratio = parsedRatio
		}
		if math.IsNaN(ratio) || math.IsInf(ratio, 0) || ratio < 0 || ratio > 1 {
			return nil, errors.New("OTEL_TRACES_SAMPLER_ARG must be a number between 0 and 1")
		}

		ratioSampler := trace.TraceIDRatioBased(ratio)
		if strings.EqualFold(strings.TrimSpace(name), "parentbased_traceidratio") {
			return trace.ParentBased(ratioSampler), nil
		}
		return ratioSampler, nil
	default:
		return nil, errors.New("unsupported OTEL_TRACES_SAMPLER value")
	}
}

func newLoggerProvider(ctx context.Context) (*log.LoggerProvider, error) {
	logExporter, err := otlploghttp.New(ctx)
	if err != nil {
		return nil, err
	}

	return log.NewLoggerProvider(
		log.WithProcessor(
			log.NewBatchProcessor(logExporter),
		),
	), nil
}
