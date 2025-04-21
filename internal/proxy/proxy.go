package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"golang.org/x/sync/errgroup"
	"io"
	"log/slog"
	"net/http"
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
	/*
		method, _, _ := ParseRPCMethodFromRequest(req)
		defer TimeCost(fmt.Sprintf("target:%s,proxyWithTimeout-method:%s", target.Name(), method))()
	*/
	pw := NewResponseWriter()
	ctx, cancel := context.WithTimeout(req.Context(), p.timeout)
	defer cancel()

	done := make(chan struct{})

	go func() {
		defer func() {
			if err := recover(); err != nil {
				p.Logger.Error("ServeHTTP", err)
			}
		}()
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
	defer TimeCost("Proxy.ServeHTTP")()
	body := &bytes.Buffer{}

	if _, err := io.Copy(body, r.Body); err != nil {
		p.errServiceUnavailable(w)
		return
	}

	//ctx, cancel := context.WithCancel(r.Context())
	//defer cancel()

	type ReqResult struct {
		pw   *ReponseWriter
		err  error
		node string
	}

	respCh := make(chan ReqResult, 1)
	g, _ := errgroup.WithContext(r.Context())

	for _, target := range p.targets {
		copyTarget := target
		g.Go(func() error {
			defer TimeCost(fmt.Sprintf("Request By Target:%s", copyTarget.Name()))()
			defer func() {
				if err := recover(); err != nil {
					p.Logger.Error("proxying request panic", err)
				}
			}()

			//p.Logger.Debug("proxying request", slog.String("target", copyTarget.Name()))
			req := r.Clone(r.Context()) //这里不要使用ctx，因为会被cancel
			req.Body = io.NopCloser(bytes.NewBuffer(body.Bytes()))

			pw, err := p.proxyWithTimeout(copyTarget, req)
			if err != nil {
				p.Logger.Debug("proxyWithTimeout: failed to proxy request",
					slog.String("target", copyTarget.Name()), slog.String("error", err.Error()))
				return nil //忽略
			}

			if p.HasNodeProviderFailed(pw.statusCode) {
				p.Logger.Error("HasNodeProviderFailed: failed to proxy request",
					slog.String("target", copyTarget.Name()), slog.Int("status_code", pw.statusCode))
				return nil //忽略
			}
			//非阻塞，如果已经有人放入，则放弃
			select {
			case respCh <- ReqResult{pw: pw, node: copyTarget.Name()}:
			default:
			}
			return nil

		})
	}

	go func() {
		_ = g.Wait()
		close(respCh)
	}()

	for res := range respCh {
		if res.pw != nil {
			p.Logger.Debug("select proxying request", slog.String("target", res.node))
			p.copyHeaders(w, res.pw)
			w.WriteHeader(res.pw.statusCode)
			_, _ = w.Write(res.pw.body.Bytes())
			return
		}
	}

	p.Logger.Error("all node providers failed to proxy request")
	p.errServiceUnavailable(w)
}

func TimeCost(keyName string) func() {
	begin := time.Now()
	return func() {
		cost := time.Since(begin)
		if cost > time.Millisecond*50 {
			fmt.Println("Time cost:", keyName, cost)
		}
	}
}

// 生产环境不应该使用
func ParseRPCMethodFromRequest(req *http.Request) (method string, bodyCopy []byte, err error) {
	if req.Body == nil {
		return "", nil, fmt.Errorf("empty request body")
	}

	bodyBytes, err := io.ReadAll(req.Body)
	if err != nil {
		return "", nil, err
	}

	req.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))

	var rpcPayload struct {
		Method string `json:"method"`
	}
	if err := json.Unmarshal(bodyBytes, &rpcPayload); err != nil {
		return "", bodyBytes, err
	}

	return rpcPayload.Method, bodyBytes, nil
}
