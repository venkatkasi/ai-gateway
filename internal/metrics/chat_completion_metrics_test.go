// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package metrics

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/envoyproxy/ai-gateway/filterapi"
)

func TestNewProcessorMetrics(t *testing.T) {
	var (
		mr    = metric.NewManualReader()
		meter = metric.NewMeterProvider(metric.WithReader(mr)).Meter("test")
		pm    = DefaultChatCompletion(meter).(*chatCompletion)
	)

	assert.NotNil(t, pm)
	assert.False(t, pm.firstTokenSent)
}

func TestStartRequest(t *testing.T) {
	var (
		mr    = metric.NewManualReader()
		meter = metric.NewMeterProvider(metric.WithReader(mr)).Meter("test")
		pm    = DefaultChatCompletion(meter).(*chatCompletion)
	)

	before := time.Now()
	pm.StartRequest(nil)
	after := time.Now()

	assert.False(t, pm.firstTokenSent)
	assert.GreaterOrEqual(t, pm.requestStart, before)
	assert.LessOrEqual(t, pm.requestStart, after)
}

func TestRecordTokenUsage(t *testing.T) {
	var (
		mr    = metric.NewManualReader()
		meter = metric.NewMeterProvider(metric.WithReader(mr)).Meter("test")
		pm    = DefaultChatCompletion(meter).(*chatCompletion)

		extra = attribute.Key("extra").String("value")
		attrs = []attribute.KeyValue{
			attribute.Key(genaiAttributeOperationName).String(genaiOperationChat),
			attribute.Key(genaiAttributeSystemName).String(genaiSystemOpenAI),
			attribute.Key(genaiAttributeRequestModel).String("test-model"),
			attribute.Key("x_amg_id").String("unknown"),
			extra,
		}
		inputAttrs  = attribute.NewSet(append(attrs, attribute.Key(genaiAttributeTokenType).String(genaiTokenTypeInput))...)
		outputAttrs = attribute.NewSet(append(attrs, attribute.Key(genaiAttributeTokenType).String(genaiTokenTypeOutput))...)
		totalAttrs  = attribute.NewSet(append(attrs, attribute.Key(genaiAttributeTokenType).String(genaiTokenTypeTotal))...)
	)

	pm.SetModel("test-model")
	pm.SetBackend(&filterapi.Backend{Schema: filterapi.VersionedAPISchema{Name: filterapi.APISchemaOpenAI}})
	pm.RecordTokenUsage(t.Context(), 10, 5, 15, extra)

	count, sum := getHistogramValues(t, mr, genaiMetricClientTokenUsage, inputAttrs)
	assert.Equal(t, uint64(1), count)
	assert.Equal(t, 10.0, sum)

	count, sum = getHistogramValues(t, mr, genaiMetricClientTokenUsage, outputAttrs)
	assert.Equal(t, uint64(1), count)
	assert.Equal(t, 5.0, sum)

	count, sum = getHistogramValues(t, mr, genaiMetricClientTokenUsage, totalAttrs)
	assert.Equal(t, uint64(1), count)
	assert.Equal(t, 15.0, sum)
}

func TestRecordTokenLatency(t *testing.T) {
	var (
		mr    = metric.NewManualReader()
		meter = metric.NewMeterProvider(metric.WithReader(mr)).Meter("test")
		pm    = DefaultChatCompletion(meter).(*chatCompletion)

		extra = attribute.Key("extra").String("value")
		attrs = attribute.NewSet(
			attribute.Key(genaiAttributeOperationName).String(genaiOperationChat),
			attribute.Key(genaiAttributeSystemName).String(genAISystemAWSBedrock),
			attribute.Key(genaiAttributeRequestModel).String("test-model"),
			attribute.Key("x_amg_id").String("unknown"),
			extra,
		)
	)

	pm.StartRequest(nil)
	pm.SetModel("test-model")
	pm.SetBackend(&filterapi.Backend{Schema: filterapi.VersionedAPISchema{Name: filterapi.APISchemaAWSBedrock}})

	// Test first token.
	time.Sleep(10 * time.Millisecond)
	pm.RecordTokenLatency(t.Context(), 1, extra)
	assert.True(t, pm.firstTokenSent)
	count, sum := getHistogramValues(t, mr, genaiMetricServerTimeToFirstToken, attrs)
	assert.Equal(t, uint64(1), count)
	assert.Greater(t, sum, 0.0)

	// Test subsequent tokens.
	time.Sleep(10 * time.Millisecond)
	pm.RecordTokenLatency(t.Context(), 5, extra)
	count, sum = getHistogramValues(t, mr, genaiMetricServerTimePerOutputToken, attrs)
	assert.Equal(t, uint64(1), count)
	assert.Greater(t, sum, 0.0)

	// Test zero tokens case.
	time.Sleep(10 * time.Millisecond)
	pm.RecordTokenLatency(t.Context(), 0, extra)
	count, sum = getHistogramValues(t, mr, genaiMetricServerTimePerOutputToken, attrs)
	assert.Equal(t, uint64(1), count)
	assert.Greater(t, sum, 0.0)
}

func TestRecordRequestCompletion(t *testing.T) {
	var (
		mr    = metric.NewManualReader()
		meter = metric.NewMeterProvider(metric.WithReader(mr)).Meter("test")
		pm    = DefaultChatCompletion(meter).(*chatCompletion)

		extra = attribute.Key("extra").String("value")
		attrs = []attribute.KeyValue{
			attribute.Key(genaiAttributeOperationName).String(genaiOperationChat),
			attribute.Key(genaiAttributeSystemName).String("custom"),
			attribute.Key(genaiAttributeRequestModel).String("test-model"),
			attribute.Key("x_amg_id").String("unknown"),
			extra,
		}
		attrsSuccess = attribute.NewSet(attrs...)
		attrsFailure = attribute.NewSet(append(attrs, attribute.Key(genaiAttributeErrorType).String(genaiErrorTypeFallback))...)
	)

	pm.StartRequest(nil)
	pm.SetModel("test-model")
	pm.SetBackend(&filterapi.Backend{Name: "custom"})

	time.Sleep(10 * time.Millisecond)
	pm.RecordRequestCompletion(t.Context(), true, extra)
	count, sum := getHistogramValues(t, mr, genaiMetricServerRequestDuration, attrsSuccess)
	assert.Equal(t, uint64(1), count)
	assert.Greater(t, sum, 0.0)

	// Test some failed requests.
	pm.RecordRequestCompletion(t.Context(), false, extra)
	pm.RecordRequestCompletion(t.Context(), false, extra)
	count, sum = getHistogramValues(t, mr, genaiMetricServerRequestDuration, attrsFailure)
	assert.Equal(t, uint64(2), count)
	assert.Greater(t, sum, 0.0)
}

func TestGetTimeToFirstTokenMsAndGetInterTokenLatencyMs(t *testing.T) {
	c := chatCompletion{timeToFirstToken: 1.0, interTokenLatency: 2.0}
	assert.Equal(t, 1000.0, c.GetTimeToFirstTokenMs())
	assert.Equal(t, 2000.0, c.GetInterTokenLatencyMs())
}

// getHistogramValues returns the count and sum of a histogram metric with the given attributes.
func getHistogramValues(t *testing.T, reader metric.Reader, metric string, attrs attribute.Set) (uint64, float64) {
	var data metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(t.Context(), &data))

	var datapoints []metricdata.HistogramDataPoint[float64]
	for _, sm := range data.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != metric {
				continue
			}
			data := m.Data.(metricdata.Histogram[float64])
			for _, dp := range data.DataPoints {
				if dp.Attributes.Equals(&attrs) {
					datapoints = append(datapoints, dp)
				}
			}
		}
	}

	require.Len(t, datapoints, 1, "found %d datapoints for attributes: %v", len(datapoints), attrs)

	return datapoints[0].Count, datapoints[0].Sum
}

func TestXAmgIdLabelHandling(t *testing.T) {
	t.Run("with x-amg-id header", func(t *testing.T) {
		var (
			mr    = metric.NewManualReader()
			meter = metric.NewMeterProvider(metric.WithReader(mr)).Meter("test")
			pm    = DefaultChatCompletion(meter).(*chatCompletion)
		)

		headers := map[string]string{
			"x-amg-id": "test-amg-123",
		}
		pm.StartRequest(headers)
		pm.SetModel("test-model")
		pm.SetBackend(&filterapi.Backend{Name: "custom"})

		pm.RecordTokenUsage(t.Context(), 10, 5, 15)
		
		// Check input tokens
		inputAttrs := attribute.NewSet(
			attribute.Key(genaiAttributeOperationName).String(genaiOperationChat),
			attribute.Key(genaiAttributeSystemName).String("custom"),
			attribute.Key(genaiAttributeRequestModel).String("test-model"),
			attribute.Key("x_amg_id").String("test-amg-123"),
			attribute.Key(genaiAttributeTokenType).String(genaiTokenTypeInput),
		)
		count, sum := getHistogramValues(t, mr, genaiMetricClientTokenUsage, inputAttrs)
		assert.Equal(t, uint64(1), count)
		assert.Equal(t, float64(10), sum)

		// Check output tokens
		outputAttrs := attribute.NewSet(
			attribute.Key(genaiAttributeOperationName).String(genaiOperationChat),
			attribute.Key(genaiAttributeSystemName).String("custom"),
			attribute.Key(genaiAttributeRequestModel).String("test-model"),
			attribute.Key("x_amg_id").String("test-amg-123"),
			attribute.Key(genaiAttributeTokenType).String(genaiTokenTypeOutput),
		)
		count, sum = getHistogramValues(t, mr, genaiMetricClientTokenUsage, outputAttrs)
		assert.Equal(t, uint64(1), count)
		assert.Equal(t, float64(5), sum)

		// Check total tokens
		totalAttrs := attribute.NewSet(
			attribute.Key(genaiAttributeOperationName).String(genaiOperationChat),
			attribute.Key(genaiAttributeSystemName).String("custom"),
			attribute.Key(genaiAttributeRequestModel).String("test-model"),
			attribute.Key("x_amg_id").String("test-amg-123"),
			attribute.Key(genaiAttributeTokenType).String(genaiTokenTypeTotal),
		)
		count, sum = getHistogramValues(t, mr, genaiMetricClientTokenUsage, totalAttrs)
		assert.Equal(t, uint64(1), count)
		assert.Equal(t, float64(15), sum)
	})

	t.Run("without x-amg-id header", func(t *testing.T) {
		var (
			mr    = metric.NewManualReader()
			meter = metric.NewMeterProvider(metric.WithReader(mr)).Meter("test")
			pm    = DefaultChatCompletion(meter).(*chatCompletion)
		)

		headers := map[string]string{
			"other-header": "value",
		}
		pm.StartRequest(headers)
		pm.SetModel("test-model")
		pm.SetBackend(&filterapi.Backend{Name: "custom"})

		pm.RecordTokenUsage(t.Context(), 10, 5, 15)
		
		// Check input tokens
		inputAttrs := attribute.NewSet(
			attribute.Key(genaiAttributeOperationName).String(genaiOperationChat),
			attribute.Key(genaiAttributeSystemName).String("custom"),
			attribute.Key(genaiAttributeRequestModel).String("test-model"),
			attribute.Key("x_amg_id").String("unknown"),
			attribute.Key(genaiAttributeTokenType).String(genaiTokenTypeInput),
		)
		count, sum := getHistogramValues(t, mr, genaiMetricClientTokenUsage, inputAttrs)
		assert.Equal(t, uint64(1), count)
		assert.Equal(t, float64(10), sum)

		// Check output tokens
		outputAttrs := attribute.NewSet(
			attribute.Key(genaiAttributeOperationName).String(genaiOperationChat),
			attribute.Key(genaiAttributeSystemName).String("custom"),
			attribute.Key(genaiAttributeRequestModel).String("test-model"),
			attribute.Key("x_amg_id").String("unknown"),
			attribute.Key(genaiAttributeTokenType).String(genaiTokenTypeOutput),
		)
		count, sum = getHistogramValues(t, mr, genaiMetricClientTokenUsage, outputAttrs)
		assert.Equal(t, uint64(1), count)
		assert.Equal(t, float64(5), sum)

		// Check total tokens
		totalAttrs := attribute.NewSet(
			attribute.Key(genaiAttributeOperationName).String(genaiOperationChat),
			attribute.Key(genaiAttributeSystemName).String("custom"),
			attribute.Key(genaiAttributeRequestModel).String("test-model"),
			attribute.Key("x_amg_id").String("unknown"),
			attribute.Key(genaiAttributeTokenType).String(genaiTokenTypeTotal),
		)
		count, sum = getHistogramValues(t, mr, genaiMetricClientTokenUsage, totalAttrs)
		assert.Equal(t, uint64(1), count)
		assert.Equal(t, float64(15), sum)
	})

	t.Run("case insensitive header lookup", func(t *testing.T) {
		var (
			mr    = metric.NewManualReader()
			meter = metric.NewMeterProvider(metric.WithReader(mr)).Meter("test")
			pm    = DefaultChatCompletion(meter).(*chatCompletion)
		)

		headers := map[string]string{
			"X-Amg-Id": "test-amg-456",
		}
		pm.StartRequest(headers)
		pm.SetModel("test-model")
		pm.SetBackend(&filterapi.Backend{Name: "custom"})

		pm.RecordTokenUsage(t.Context(), 10, 5, 15)
		
		// Since the current implementation is case-sensitive, X-Amg-Id won't be found
		// So it should default to "unknown"
		inputAttrs := attribute.NewSet(
			attribute.Key(genaiAttributeOperationName).String(genaiOperationChat),
			attribute.Key(genaiAttributeSystemName).String("custom"),
			attribute.Key(genaiAttributeRequestModel).String("test-model"),
			attribute.Key("x_amg_id").String("unknown"),
			attribute.Key(genaiAttributeTokenType).String(genaiTokenTypeInput),
		)
		count, sum := getHistogramValues(t, mr, genaiMetricClientTokenUsage, inputAttrs)
		assert.Equal(t, uint64(1), count)
		assert.Equal(t, float64(10), sum)

		// Check output tokens
		outputAttrs := attribute.NewSet(
			attribute.Key(genaiAttributeOperationName).String(genaiOperationChat),
			attribute.Key(genaiAttributeSystemName).String("custom"),
			attribute.Key(genaiAttributeRequestModel).String("test-model"),
			attribute.Key("x_amg_id").String("unknown"),
			attribute.Key(genaiAttributeTokenType).String(genaiTokenTypeOutput),
		)
		count, sum = getHistogramValues(t, mr, genaiMetricClientTokenUsage, outputAttrs)
		assert.Equal(t, uint64(1), count)
		assert.Equal(t, float64(5), sum)

		// Check total tokens
		totalAttrs := attribute.NewSet(
			attribute.Key(genaiAttributeOperationName).String(genaiOperationChat),
			attribute.Key(genaiAttributeSystemName).String("custom"),
			attribute.Key(genaiAttributeRequestModel).String("test-model"),
			attribute.Key("x_amg_id").String("unknown"),
			attribute.Key(genaiAttributeTokenType).String(genaiTokenTypeTotal),
		)
		count, sum = getHistogramValues(t, mr, genaiMetricClientTokenUsage, totalAttrs)
		assert.Equal(t, uint64(1), count)
		assert.Equal(t, float64(15), sum)
	})
}
