package meter

import (
	"context"
	"net/url"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"

	"github.com/yiyiSmallti/precinct-gatehouse/internal/config"
)

// OTLP exports usage as cumulative OTLP/HTTP metrics:
//
//	gatehouse.requests           {count}
//	gatehouse.tokens.prompt      {token}
//	gatehouse.tokens.completion  {token}
//	gatehouse.cost               {currency}
//
// with attributes gatehouse.token.id, gatehouse.token.name, gatehouse.model,
// gatehouse.target and http.response.status_code. Values are the same numbers
// written to SQLite, so `gatehouse report` and the collector agree.
type OTLP struct {
	provider   *sdkmetric.MeterProvider
	requests   metric.Int64Counter
	prompt     metric.Int64Counter
	completion metric.Int64Counter
	cost       metric.Float64Counter
}

// NewOTLP starts a periodic exporter.
func NewOTLP(cfg *config.OTLP, version string) (*OTLP, error) {
	u, err := url.Parse(cfg.Endpoint)
	if err != nil {
		return nil, err
	}
	opts := []otlpmetrichttp.Option{otlpmetrichttp.WithEndpoint(u.Host), otlpmetrichttp.WithHeaders(cfg.Headers)}
	if u.Scheme == "http" {
		opts = append(opts, otlpmetrichttp.WithInsecure())
	}
	if u.Path != "" && u.Path != "/" {
		opts = append(opts, otlpmetrichttp.WithURLPath(u.Path))
	}
	exp, err := otlpmetrichttp.New(context.Background(), opts...)
	if err != nil {
		return nil, err
	}
	res := resource.NewWithAttributes(semconv.SchemaURL, semconv.ServiceName("gatehouse"), semconv.ServiceVersion(version))
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp, sdkmetric.WithInterval(cfg.Interval.D(30*time.Second)))),
	)
	m := mp.Meter("github.com/yiyiSmallti/precinct-gatehouse")
	o := &OTLP{provider: mp}
	if o.requests, err = m.Int64Counter("gatehouse.requests", metric.WithUnit("{request}")); err != nil {
		return nil, err
	}
	if o.prompt, err = m.Int64Counter("gatehouse.tokens.prompt", metric.WithUnit("{token}")); err != nil {
		return nil, err
	}
	if o.completion, err = m.Int64Counter("gatehouse.tokens.completion", metric.WithUnit("{token}")); err != nil {
		return nil, err
	}
	if o.cost, err = m.Float64Counter("gatehouse.cost", metric.WithUnit("{currency}")); err != nil {
		return nil, err
	}
	return o, nil
}

// Export adds one record to the counters.
func (o *OTLP) Export(r Record) {
	ctx := context.Background()
	attrs := metric.WithAttributes(
		attribute.String("gatehouse.token.id", r.TokenID),
		attribute.String("gatehouse.token.name", r.TokenName),
		attribute.String("gatehouse.model", r.Model),
		attribute.String("gatehouse.target", r.Target),
		attribute.Int("http.response.status_code", r.Status),
	)
	o.requests.Add(ctx, 1, attrs)
	o.prompt.Add(ctx, int64(r.PromptTokens), attrs)
	o.completion.Add(ctx, int64(r.CompletionTokens), attrs)
	o.cost.Add(ctx, r.Cost, attrs)
}

// Shutdown flushes pending metrics.
func (o *OTLP) Shutdown(ctx context.Context) error { return o.provider.Shutdown(ctx) }
