package proxy

import (
	"net/http"
	"net/http/httputil"
	"net/url"

	"github.com/pkg/errors"
)

func NewNodeProviderProxy(config NodeProviderConfig) (*httputil.ReverseProxy, error) {
	target, err := url.Parse(config.Connection.HTTP.URL)
	if err != nil {
		return nil, errors.Wrap(err, "cannot parse url")
	}

	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Director = func(r *http.Request) {
		r.Host = target.Host
		r.URL.Scheme = target.Scheme
		r.URL.Host = target.Host
		r.URL.Path = target.Path
		r.URL.RawQuery = target.RawQuery //bug:不然这种格式会报授权失败?api-key
	}

	return proxy, nil
}
