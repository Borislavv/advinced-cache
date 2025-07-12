package middleware

import (
	"context"
	"github.com/Borislavv/advanced-cache/pkg/prometheus/metrics"
	"github.com/valyala/fasthttp"
	"strconv"
)

var emptyStr = []byte("")

type PrometheusMetrics struct {
	ctx   context.Context
	meter metrics.Meter
	codes [599][]byte
}

func NewPrometheusMetrics(ctx context.Context, meter metrics.Meter) *PrometheusMetrics {
	codes := [599][]byte{}
	for code := 0; code < 599; code++ {
		codes[code] = []byte(strconv.Itoa(code))
	}
	return &PrometheusMetrics{
		ctx:   ctx,
		meter: meter,
		codes: codes,
	}
}

func (m *PrometheusMetrics) Middleware(next fasthttp.RequestHandler) fasthttp.RequestHandler {
	return func(ctx *fasthttp.RequestCtx) {
		pth := ctx.Path()
		method := ctx.Method()

		timer := m.meter.NewResponseTimeTimer(pth, method)
		m.meter.IncTotal(pth, method, emptyStr) // total requests (no status)

		next(ctx)

		status := ctx.Response.StatusCode()
		m.meter.IncStatus(pth, method, m.codes[status])
		m.meter.IncTotal(pth, method, m.codes[status])
		m.meter.FlushResponseTimeTimer(timer)
	}
}
