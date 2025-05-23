package rpcgateway

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/0xProject/rpc-gateway/internal/metrics"
	"github.com/0xProject/rpc-gateway/internal/proxy"
	"github.com/carlmjohnson/flowmatic"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/httplog/v2"
	"github.com/pkg/errors"
	"gopkg.in/yaml.v2"
)

type RPCGateway struct {
	config  RPCGatewayConfig
	proxy   *proxy.Proxy
	hcm     *proxy.HealthCheckManager
	server  *http.Server
	metrics *metrics.Server
}

func (r *RPCGateway) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.server.Handler.ServeHTTP(w, req)
}

func (r *RPCGateway) Start(c context.Context) error {
	return flowmatic.Do(
		func() error {
			return errors.Wrap(r.hcm.Start(c), "failed to start health check manager")
		},
		func() error {
			return errors.Wrap(r.server.ListenAndServe(), "failed to start rpc-gateway")
		},
		func() error {
			return errors.Wrap(r.metrics.Start(), "failed to start metrics server")
		},
	)
}

func (r *RPCGateway) Stop(c context.Context) error {
	return flowmatic.Do(
		func() error {
			return errors.Wrap(r.hcm.Stop(c), "failed to stop health check manager")
		},
		func() error {
			return errors.Wrap(r.server.Close(), "failed to stop rpc-gateway")
		},
		func() error {
			return errors.Wrap(r.metrics.Stop(), "failed to stop metrics server")
		},
	)
}

func NewRPCGateway(config RPCGatewayConfig) (*RPCGateway, error) {
	logLevel := slog.LevelWarn
	if os.Getenv("DEBUG") == "true" {
		logLevel = slog.LevelDebug
	}

	logger := httplog.NewLogger("rpc-gateway", httplog.Options{
		JSON:           true,
		RequestHeaders: true,
		LogLevel:       logLevel,
	})

	slogger := slog.New(
		slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
			Level: logLevel,
		}))

	hcm, err := proxy.NewHealthCheckManager(
		proxy.HealthCheckManagerConfig{
			Targets: config.Targets,
			Config:  config.HealthChecks,
			Logger:  slogger,
		})
	if err != nil {
		return nil, errors.Wrap(err, "healthcheckmanager failed")
	}

	proxyObject, err := proxy.NewProxy(
		proxy.Config{
			Proxy:              config.Proxy,
			Targets:            config.Targets,
			HealthChecks:       config.HealthChecks,
			HealthcheckManager: hcm,
			Logger:             slogger,
		},
	)
	if err != nil {
		return nil, errors.Wrap(err, "proxy failed")
	}

	r := chi.NewRouter()
	r.Use(httplog.RequestLogger(logger))

	// Recoverer is a middleware that recovers from panics, logs the panic (and
	// a backtrace), and returns a HTTP 500 (Internal Server Error) status if
	// possible. Recoverer prints a request ID if one is provided.
	//
	r.Use(middleware.Recoverer)

	r.Handle("/", proxyObject)

	return &RPCGateway{
		config: config,
		proxy:  proxyObject,
		hcm:    hcm,
		metrics: metrics.NewServer(
			metrics.Config{
				Port: config.Metrics.Port,
			},
		),
		server: &http.Server{
			Addr:              fmt.Sprintf(":%s", config.Proxy.Port),
			Handler:           r,
			WriteTimeout:      time.Second * 15,
			ReadTimeout:       time.Second * 15,
			ReadHeaderTimeout: time.Second * 5,
		},
	}, nil
}

// NewRPCGatewayFromConfigFile creates an instance of RPCGateway from provided
// configuration file.
func NewRPCGatewayFromConfigFile(s string) (*RPCGateway, error) {
	data, err := os.ReadFile(s)
	if err != nil {
		return nil, err
	}

	var config RPCGatewayConfig

	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, err
	}

	return NewRPCGateway(config)
}
