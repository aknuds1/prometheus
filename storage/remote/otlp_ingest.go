// Copyright The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package remote

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	deltatocumulative "github.com/open-telemetry/opentelemetry-collector-contrib/processor/deltatocumulativeprocessor"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/processor"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/prometheus/prometheus/config"
	"github.com/prometheus/prometheus/storage"
	otlptranslator "github.com/prometheus/prometheus/storage/remote/otlptranslator/prometheusremotewrite"
)

// rwExporter translates OTLP metrics into the Prometheus data model and appends
// them to storage.
type rwExporter struct {
	logger                  *slog.Logger
	appendable              storage.AppendableV2
	config                  func() config.Config
	allowDeltaTemporality   bool
	lookbackDelta           time.Duration
	enableTypeAndUnitLabels bool
	translationWarnings     *prometheus.CounterVec
	converterPool           sync.Pool
}

func (rw *rwExporter) ConsumeMetrics(ctx context.Context, md pmetric.Metrics) error {
	otlpCfg := rw.config().OTLPConfig
	app := rw.appendable.AppenderV2(ctx)
	converter := rw.converterPool.Get().(*otlptranslator.PrometheusConverter)
	converter.Reset(app)
	annots, err := converter.FromMetrics(ctx, md, otlptranslator.Settings{
		AddMetricSuffixes:                    otlpCfg.TranslationStrategy.ShouldAddSuffixes(),
		AllowUTF8:                            !otlpCfg.TranslationStrategy.ShouldEscape(),
		PromoteResourceAttributes:            otlptranslator.NewPromoteResourceAttributes(otlpCfg),
		KeepIdentifyingResourceAttributes:    otlpCfg.KeepIdentifyingResourceAttributes,
		ConvertHistogramsToNHCB:              otlpCfg.ConvertHistogramsToNHCB,
		PromoteScopeMetadata:                 otlpCfg.PromoteScopeMetadata,
		AllowDeltaTemporality:                rw.allowDeltaTemporality,
		LookbackDelta:                        rw.lookbackDelta,
		EnableTypeAndUnitLabels:              rw.enableTypeAndUnitLabels,
		LabelNameUnderscoreSanitization:      otlpCfg.LabelNameUnderscoreSanitization,
		LabelNamePreserveMultipleUnderscores: otlpCfg.LabelNamePreserveMultipleUnderscores,
	})

	defer func() {
		if err != nil {
			_ = app.Rollback()
		} else {
			err = app.Commit()
		}
		converter.Reset(nil)
		rw.converterPool.Put(converter)
	}()
	ws, _ := annots.AsStrings("", 0, 0)
	if len(ws) > 0 {
		for category, count := range otlptranslator.CountWarningsByCategory(annots) {
			rw.translationWarnings.WithLabelValues(string(category)).Add(float64(count))
		}
		rw.logger.Warn("Warnings translating OTLP metrics to Prometheus write request", "warnings", ws)
	}
	return err
}

func (*rwExporter) Capabilities() consumer.Capabilities {
	return consumer.Capabilities{MutatesData: false}
}

// metricsIngestConsumer is the OTLP-to-Prometheus ingestion path shared by all
// transports that carry OTLP metrics. Every transport must go through it so that
// identical input produces identical series, metadata, and error behavior
// regardless of how the data arrived.
type metricsIngestConsumer struct {
	logger *slog.Logger
	// plain appends delta samples as-is, or rejects them unless native delta
	// ingestion is enabled.
	plain consumer.Metrics
	// d2c converts delta temporality into cumulative. It is nil unless delta
	// conversion is enabled.
	d2c consumer.Metrics
}

// NewMetricsConsumer returns the consumer implementing the shared OTLP-to-Prometheus
// ingestion path. The returned consumer is safe for concurrent use only to the same
// extent as the underlying appender, which is per-call.
func NewMetricsConsumer(logger *slog.Logger, reg prometheus.Registerer, appendable storage.AppendableV2, configFunc func() config.Config, opts OTLPOptions) consumer.Metrics {
	if opts.NativeDelta && opts.ConvertDelta {
		// This should be validated when iterating through feature flags, so not expected to fail here.
		panic("cannot enable native delta ingestion and delta2cumulative conversion at the same time")
	}

	ex := &rwExporter{
		logger:                  logger,
		appendable:              newOTLPInstrumentedAppendable(reg, appendable),
		config:                  configFunc,
		allowDeltaTemporality:   opts.NativeDelta,
		lookbackDelta:           opts.LookbackDelta,
		enableTypeAndUnitLabels: opts.EnableTypeAndUnitLabels,
		converterPool: sync.Pool{
			New: func() any {
				return otlptranslator.NewPrometheusConverter(nil)
			},
		},
		translationWarnings: promauto.With(reg).NewCounterVec(prometheus.CounterOpts{
			Namespace: "prometheus",
			Subsystem: "api",
			Name:      "otlp_translation_warnings_total",
			Help:      "The total number of warnings produced while translating OTLP metrics to the Prometheus model, by category.",
		}, []string{"category"}),
	}

	c := &metricsIngestConsumer{logger: logger, plain: ex}

	if opts.ConvertDelta {
		fac := deltatocumulative.NewFactory()
		set := processor.Settings{
			ID:                component.NewID(fac.Type()),
			TelemetrySettings: component.TelemetrySettings{MeterProvider: noop.NewMeterProvider()},
		}
		d2c, err := fac.CreateMetrics(context.Background(), set, fac.CreateDefaultConfig(), c.plain)
		if err != nil {
			// fac.CreateMetrics directly calls [deltatocumulativeprocessor.createMetricsProcessor],
			// which only errors if:
			//   - cfg.(type) != *Config
			//   - telemetry.New fails due to bad set.TelemetrySettings
			//
			// both cannot be the case, as we pass a valid *Config and valid TelemetrySettings.
			// as such, we assume this error to never occur.
			// if it is, our assumptions are broken in which case a panic seems acceptable.
			panic(fmt.Errorf("failed to create metrics processor: %w", err))
		}
		if err := d2c.Start(context.Background(), nil); err != nil {
			// deltatocumulative does not error on start. see above for panic reasoning
			panic(err)
		}
		c.d2c = d2c
	}

	return c
}

// ConsumeMetrics appends the given OTLP metrics to storage.
func (c *metricsIngestConsumer) ConsumeMetrics(ctx context.Context, md pmetric.Metrics) error {
	// If deltatocumulative conversion enabled AND delta samples exist, use slower conversion path.
	// While deltatocumulative can also accept cumulative metrics (and then just forwards them as-is), it currently
	// holds a sync.Mutex when entering ConsumeMetrics. This is slow and not necessary when ingesting cumulative metrics.
	if c.d2c != nil && hasDelta(md) {
		return c.d2c.ConsumeMetrics(ctx, md)
	}
	// Otherwise use the plain consumer (alongside cumulative samples, this will accept delta samples and write as-is
	// if native-delta-support is enabled).
	return c.plain.ConsumeMetrics(ctx, md)
}

func (*metricsIngestConsumer) Capabilities() consumer.Capabilities {
	return consumer.Capabilities{MutatesData: false}
}

func hasDelta(md pmetric.Metrics) bool {
	for i := range md.ResourceMetrics().Len() {
		sms := md.ResourceMetrics().At(i).ScopeMetrics()
		for i := range sms.Len() {
			ms := sms.At(i).Metrics()
			for i := range ms.Len() {
				temporality := pmetric.AggregationTemporalityUnspecified
				m := ms.At(i)
				switch ms.At(i).Type() {
				case pmetric.MetricTypeSum:
					temporality = m.Sum().AggregationTemporality()
				case pmetric.MetricTypeExponentialHistogram:
					temporality = m.ExponentialHistogram().AggregationTemporality()
				case pmetric.MetricTypeHistogram:
					temporality = m.Histogram().AggregationTemporality()
				}
				if temporality == pmetric.AggregationTemporalityDelta {
					return true
				}
			}
		}
	}
	return false
}
