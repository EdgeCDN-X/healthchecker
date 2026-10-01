package telemetry

import (
	"testing"

	"go.opentelemetry.io/otel/sdk/trace"
	oteltrace "go.opentelemetry.io/otel/trace"
)

func TestSamplerFromConfig(t *testing.T) {
	tests := []struct {
		name    string
		sampler string
		arg     string
		wantErr bool
	}{
		{name: "default sampler"},
		{name: "always on", sampler: "always_on"},
		{name: "always off", sampler: "always_off"},
		{name: "trace ratio", sampler: "traceidratio", arg: "0.25"},
		{name: "parent-based trace ratio", sampler: "parentbased_traceidratio", arg: "0.25"},
		{name: "ratio defaults to one", sampler: "traceidratio"},
		{name: "ratio below range", sampler: "traceidratio", arg: "-0.1", wantErr: true},
		{name: "ratio above range", sampler: "traceidratio", arg: "1.1", wantErr: true},
		{name: "invalid ratio", sampler: "traceidratio", arg: "invalid", wantErr: true},
		{name: "unsupported sampler", sampler: "unknown", wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sampler, err := samplerFromConfig(test.sampler, test.arg)
			if (err != nil) != test.wantErr {
				t.Fatalf("samplerFromConfig() error = %v, wantErr %v", err, test.wantErr)
			}
			if !test.wantErr && sampler == nil {
				t.Fatal("samplerFromConfig() returned a nil sampler")
			}
		})
	}
}

func TestTraceIDRatioSamplerDecisions(t *testing.T) {
	traceID := oteltrace.TraceID{1}
	tests := []struct {
		name     string
		ratio    string
		decision trace.SamplingDecision
	}{
		{name: "drop all", ratio: "0", decision: trace.Drop},
		{name: "sample all", ratio: "1", decision: trace.RecordAndSample},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sampler, err := samplerFromConfig("traceidratio", test.ratio)
			if err != nil {
				t.Fatalf("samplerFromConfig() error = %v", err)
			}

			result := sampler.ShouldSample(trace.SamplingParameters{TraceID: traceID})
			if result.Decision != test.decision {
				t.Fatalf("ShouldSample() decision = %v, want %v", result.Decision, test.decision)
			}
		})
	}
}
