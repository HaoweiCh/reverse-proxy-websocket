package WebsocketReverseProxy

import (
	"io"
	"net/http"
	"net/url"
)

// ProxyHandler returns a new http.Handler interface that reverse proxies the
// request to the given target.
//func ProxyHandler(target *url.URL) http.Handler { return NewProxy(target) }

func MustRewrite(backend string) func(r *http.Request) (error, *url.URL) {
	backendUrl, err := url.Parse(backend)

	ret := func(r *http.Request) (error, *url.URL) {
		// shallow copy
		u := *backendUrl
		u.Fragment = r.URL.Fragment
		u.Path = r.URL.Path
		u.RawQuery = r.URL.RawQuery
		return err, &u // 这里的 error 来自于 url parse
	}

	return ret
}

func copyHeader(dst, src http.Header) {
	for k, vv := range src {
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

func copyResponse(rw http.ResponseWriter, resp *http.Response) error {
	copyHeader(rw.Header(), resp.Header)
	rw.WriteHeader(resp.StatusCode)
	defer resp.Body.Close()

	_, err := io.Copy(rw, resp.Body)
	return err
}
