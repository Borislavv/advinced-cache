package metrics

import (
	"github.com/Borislavv/advanced-cache/pkg/prometheus/metrics/keyword"
	"github.com/VictoriaMetrics/metrics"
	"strconv"
	"time"
	"unsafe"
)

// Meter defines methods for recording application metrics.
type Meter interface {
	IncTotal(path, method, status []byte)
	IncStatus(path, method, status []byte)
	NewResponseTimeTimer(path, method []byte) *Timer
	FlushResponseTimeTimer(t *Timer)
	SetCacheLength(count int64)
	SetCacheMemory(bytes int64)
}

// Metrics implements Meter using VictoriaMetrics metrics.
type Metrics struct{}

// New creates a new Metrics instance.
func New() *Metrics {
	return &Metrics{}
}

// Precompute status code strings for performance.
var statuses [599]string

func init() {
	for i := 100; i < len(statuses); i++ {
		statuses[i] = strconv.Itoa(i)
	}
}

// IncTotal increments total requests or responses depending on status.
func (m *Metrics) IncTotal(path, method, status []byte) {
	name := keyword.TotalHttpRequestsMetricName
	if len(status) > 0 {
		name = keyword.TotalHttpResponsesMetricName
	}
	buf := make([]byte, 0, 128)

	buf = append(buf, name...)
	buf = append(buf, `{path="`...)
	buf = append(buf, path...)
	buf = append(buf, `",method="`...)
	buf = append(buf, method...)
	buf = append(buf, `"`...)

	if len(status) > 0 {
		buf = append(buf, `,status="`...)
		buf = append(buf, status...)
		buf = append(buf, `"`...)
	}
	buf = append(buf, `}`...)
	bufStr := *(*string)(unsafe.Pointer(&buf))

	metrics.GetOrCreateCounter(bufStr).Inc()
}

// IncStatus increments a counter for HTTP response statuses.
func (m *Metrics) IncStatus(path, method, status []byte) {
	buf := make([]byte, 0, 128)
	buf = append(buf, keyword.HttpResponseStatusesMetricName...)
	buf = append(buf, `{path="`...)
	buf = append(buf, path...)
	buf = append(buf, `",method="`...)
	buf = append(buf, method...)
	buf = append(buf, `",status="`...)
	buf = append(buf, status...)
	buf = append(buf, `"}`...)
	bufStr := *(*string)(unsafe.Pointer(&buf))

	metrics.GetOrCreateCounter(bufStr).Inc()
}

// SetCacheMemory updates the gauge for total cache memory usage in bytes.
func (m *Metrics) SetCacheMemory(bytes int64) {
	metrics.GetOrCreateCounter(keyword.MapMemoryUsageMetricName).Set(uint64(bytes))
}

// SetCacheLength updates the gauge for total number of items in the cache.
func (m *Metrics) SetCacheLength(count int64) {
	metrics.GetOrCreateCounter(keyword.MapLength).Set(uint64(count))
}

// Timer tracks start of an operation for timing metrics.
type Timer struct {
	name  string
	start time.Time
}

// NewResponseTimeTimer creates a Timer for measuring response time of given path and method.
func (m *Metrics) NewResponseTimeTimer(path, method []byte) *Timer {
	buf := make([]byte, 0, 128)
	buf = append(buf, keyword.HttpResponseTimeMsMetricName...)
	buf = append(buf, `{path="`...)
	buf = append(buf, path...)
	buf = append(buf, `",method="`...)
	buf = append(buf, method...)
	buf = append(buf, `"}`...)
	bufStr := *(*string)(unsafe.Pointer(&buf))

	return &Timer{name: bufStr, start: time.Now()}
}

// FlushResponseTimeTimer records the elapsed time since Timer creation into a histogram.
func (m *Metrics) FlushResponseTimeTimer(t *Timer) {
	metrics.GetOrCreateHistogram(t.name).Update(time.Since(t.start).Seconds())
}
