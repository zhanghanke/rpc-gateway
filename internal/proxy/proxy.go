package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

type Proxy struct {
	targets []*NodeProvider
	hcm     *HealthCheckManager
	timeout time.Duration

	metricRequestDuration *prometheus.HistogramVec
	metricRequestErrors   *prometheus.CounterVec

	Logger *slog.Logger
}

func NewProxy(config Config) (*Proxy, error) {
	proxy := &Proxy{
		hcm:     config.HealthcheckManager,
		timeout: config.Proxy.UpstreamTimeout,
		metricRequestDuration: promauto.NewHistogramVec(
			prometheus.HistogramOpts{
				Name: "zeroex_rpc_gateway_request_duration_seconds",
				Help: "Histogram of response time for Gateway in seconds",
				Buckets: []float64{
					.025,
					.05,
					.1,
					.25,
					.5,
					1,
					2.5,
					5,
					10,
					15,
					20,
					25,
					30,
				},
			}, []string{
				"provider",
				"method",
				"status_code",
			}),
		metricRequestErrors: promauto.NewCounterVec(
			prometheus.CounterOpts{
				Name: "zeroex_rpc_gateway_request_errors_handled_total",
				Help: "The total number of request errors handled by gateway",
			}, []string{
				"provider",
				"type",
			}),
		Logger: config.Logger,
	}

	for _, target := range config.Targets {
		p, err := NewNodeProvider(target)
		if err != nil {
			return nil, err
		}

		proxy.targets = append(proxy.targets, p)
	}

	return proxy, nil
}

func (p *Proxy) HasNodeProviderFailed(statusCode int) bool {
	return statusCode != http.StatusOK
}

func (p *Proxy) copyHeaders(dst http.ResponseWriter, src http.ResponseWriter) {
	for k, v := range src.Header() {
		if len(v) == 0 {
			continue
		}

		dst.Header().Set(k, v[0])
	}
}

func (p *Proxy) proxyWithTimeout(target *NodeProvider, req *http.Request) (*ReponseWriter, error) {
	pw := NewResponseWriter()
	ctx, cancel := context.WithTimeout(req.Context(), p.timeout)
	defer cancel()

	done := make(chan struct{})

	go func() {
		target.Proxy.ServeHTTP(pw, req)
		close(done)
	}()

	select {
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, context.DeadlineExceeded
		} else if errors.Is(ctx.Err(), context.Canceled) {
			return nil, context.Canceled
		}
		return nil, ctx.Err()
	case <-done:
		return pw, nil
	}
}

func (p *Proxy) timeoutHandler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		handler := http.TimeoutHandler(next, p.timeout, http.StatusText(http.StatusGatewayTimeout))
		handler.ServeHTTP(w, r)
	}

	return http.HandlerFunc(fn)
}

func (p *Proxy) errServiceUnavailable(w http.ResponseWriter) {
	http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body := &bytes.Buffer{}

	if _, err := io.Copy(body, r.Body); err != nil {
		p.errServiceUnavailable(w)

		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	var once sync.Once
	var wg sync.WaitGroup

	var respondedOK atomic.Bool

	for _, target := range p.targets {
		//if !p.hcm.IsHealthy(target.Name()) { //检测功能屏蔽了，这里不需要再调用
		//	continue
		//}

		wg.Add(1)
		go func(target *NodeProvider) {
			defer wg.Done()

			p.Logger.Debug("proxying request", slog.String("target", target.Name()))
			req := r.Clone(ctx)
			req.Body = io.NopCloser(bytes.NewBuffer(body.Bytes()))

			pw, err := p.proxyWithTimeout(target, req)
			if err != nil {
				p.Logger.Debug("proxyWithTimeout: failed to proxy request",
					slog.String("target", target.Name()), slog.String("error", err.Error()))
				return
			}

			if p.HasNodeProviderFailed(pw.statusCode) {
				p.Logger.Error("fHasNodeProviderFailed: ailed to proxy request",
					slog.String("target", target.Name()), slog.Int("status_code", pw.statusCode))
				return
			}
			once.Do(func() {
				respondedOK.Store(true)
				p.Logger.Debug("select proxying request", slog.String("target", target.Name()))
				p.copyHeaders(w, pw)
				w.WriteHeader(pw.statusCode)
				w.Write(pw.body.Bytes()) // nolint:errcheck

				// 通知其他协程停止
				cancel()
			})

		}(target)
	}

	wg.Wait()
	if !respondedOK.Load() {
		p.errServiceUnavailable(w)
	}
}
