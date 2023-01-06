// Package WebsocketReverseProxy
//  websocket reverse proxy
package WebsocketReverseProxy

import (
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var (
	// DefaultUpgradeHelper specifies the parameters for upgrading an HTTP
	// connection to a WebSocket connection.
	DefaultUpgradeHelper = &websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
	}

	// DefaultDialer is a dialer with all fields set to the default zero values.
	DefaultDialer = websocket.DefaultDialer
)

// Proxy is an HTTP Handler that takes an incoming WebSocket
// connection and proxies it to another server.
type Proxy struct {
	// Director, if non-nil, is a function that may copy additional request
	// headers from the incoming WebSocket connection into the output headers
	// which will be forwarded to another server.
	Director func(incoming *http.Request, out http.Header)

	// UpgradeHelper specifies the parameters for upgrading a incoming HTTP
	// connection to a WebSocket connection. If nil, DefaultUpgradeHelper is used.
	UpgradeHelper *websocket.Upgrader

	//  Dialer contains options for connecting to the backend WebSocket server.
	//  If nil, DefaultDialer is used.
	Dialer *websocket.Dialer

	// HostName force rewrite host into this
	HostName string
	// Hidden not forward client ip
	Hidden bool

	// backend returns the backend URL which the proxy uses to reverse proxy
	// the incoming WebSocket connection. Request is the initial incoming and
	// unmodified request.
	backend func(*http.Request) (error, *url.URL)
	// logger for inner log
	logger *zap.SugaredLogger

	// metrics metrics
	OnDownload OnDataFlow
	OnUpload   OnDataFlow
}

// NewProxy returns a new Websocket reverse proxy
func NewProxy() *Proxy {
	Logger, err := zap.NewProduction(zap.AddStacktrace(zapcore.WarnLevel))
	if err != nil {
		log.Fatal(err)
	}

	return &Proxy{
		logger: Logger.Sugar().With("component", "github.com/HaoweiCh/reverse-proxy-websocket"),
		Dialer: &websocket.Dialer{
			Proxy:            nil,
			HandshakeTimeout: 32 * time.Second,
		},
	}
}

// ServeHTTP implements the http.Handler that proxies WebSocket connections.
func (w *Proxy) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	if w.backend == nil {
		err := errors.New("backend function isn't defined")

		w.logger.Errorw("reverse fail", "err", err.Error())
		http.Error(rw, fmt.Errorf("internal server error: %w", err).Error(), http.StatusInternalServerError)
		return
	}

	err, backendURL := w.backend(req)
	if err != nil {
		w.logger.Warnw("reverse fail", "err", err.Error())
		http.Error(rw, fmt.Errorf("internal server error: %w", err).Error(), http.StatusInternalServerError)
		return
	}

	dialer := w.Dialer
	if dialer == nil {
		dialer = DefaultDialer
	}

	uid := req.Header.Get("UID")

	// Pass headers from the incoming request to the dialer to forward them to
	// the final destinations.
	requestHeader := http.Header{}
	if origin := req.Header.Get("Origin"); origin != "" {
		requestHeader.Add("Origin", origin)
	}
	for _, prot := range req.Header[http.CanonicalHeaderKey("Sec-WebSocket-Protocol")] {
		requestHeader.Add("Sec-WebSocket-Protocol", prot)
	}
	for _, cookie := range req.Header[http.CanonicalHeaderKey("Cookie")] {
		requestHeader.Add("Cookie", cookie)
	}

	// about host
	if w.HostName != "" {
		port := backendURL.Port()
		if port != "" {
			port = ":" + port
		}
		backendURL.Host = w.HostName + port
		requestHeader.Set("Host", w.HostName)
	} else if req.Host != "" {
		requestHeader.Set("Host", req.Host)
	}

	if !w.Hidden {
		// Pass X-Forwarded-For headers too, code below is a part of
		// httputil.ReverseProxy. See http://en.wikipedia.org/wiki/X-Forwarded-For
		// for more information
		// TODO: use RFC7239 http://tools.ietf.org/html/rfc7239
		if clientIP, _, err := net.SplitHostPort(req.RemoteAddr); err == nil {
			// If we aren't the first proxy retain prior
			// X-Forwarded-For information as a comma+space
			// separated list and fold multiple headers into one.
			if prior, ok := req.Header["X-Forwarded-For"]; ok {
				clientIP = strings.Join(prior, ", ") + ", " + clientIP
			}
			requestHeader.Set("X-Forwarded-For", clientIP)
		}
	}

	// Set the originating protocol of the incoming HTTP request. The SSL might
	// be terminated on our site and because we doing proxy adding this would
	// be helpful for applications on the backend.
	requestHeader.Set("X-Forwarded-Proto", "http")
	if req.TLS != nil {
		requestHeader.Set("X-Forwarded-Proto", "https")
	}

	// Enable the director to copy any additional headers it desires for
	// forwarding to the remote server.
	if w.Director != nil {
		w.Director(req, requestHeader)
	}

	// Connect to the backend URL, also pass the headers we get from the requst
	// together with the Forwarded headers we prepared above.
	// TODO: support multiplexing on the same backend connection instead of
	// opening a new TCP connection time for each request. This should be
	// optional:
	// http://tools.ietf.org/html/draft-ietf-hybi-websocket-multiplexing-01
	connBackend, resp, err := dialer.Dial(backendURL.String(), requestHeader)
	if err != nil {
		w.logger.Errorw("reverse failed, couldn't dial to remote backend",
			"backend_url", backendURL.String(),
			"err", err.Error(),
		)
		if resp != nil {
			// If the WebSocket handshake fails, ErrBadHandshake is returned
			// along with a non-nil *http.Response so that callers can handle
			// redirects, authentication, etcetera.
			if err := copyResponse(rw, resp); err != nil {
				w.logger.Errorw(
					"reverse failed, couldn't write response after failed remote backend handshake",
					"err", err.Error())
			}
		} else {
			http.Error(rw, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
		}
		return
	}
	defer connBackend.Close()

	upgrader := w.UpgradeHelper
	if w.UpgradeHelper == nil {
		upgrader = DefaultUpgradeHelper
	}

	// Only pass those headers to the upgrader.
	upgradeHeader := http.Header{}
	if hdr := resp.Header.Get("Sec-Websocket-Protocol"); hdr != "" {
		upgradeHeader.Set("Sec-Websocket-Protocol", hdr)
	}
	if hdr := resp.Header.Get("Set-Cookie"); hdr != "" {
		upgradeHeader.Set("Set-Cookie", hdr)
	}

	// Now upgrade the existing incoming request to a WebSocket connection.
	// Also pass the header that we gathered from the Dial handshake.
	connPub, err := upgrader.Upgrade(rw, req, upgradeHeader)
	if err != nil {
		w.logger.Errorw("reverse failed, couldn't upgrade", "err", err.Error())
		return
	}
	defer connPub.Close()

	errClient := make(chan error, 1)
	errBackend := make(chan error, 1)
	//TODO 可能需要 errors group 重写这里
	replicateWebsocketConn := func(dst, src *websocket.Conn, errC chan error, trigger OnDataFlow) {
		for {
			msgType, msg, err := src.ReadMessage()
			if err != nil {
				m := websocket.FormatCloseMessage(websocket.CloseNormalClosure, fmt.Sprintf("%v", err))
				if e, ok := err.(*websocket.CloseError); ok {
					if e.Code != websocket.CloseNoStatusReceived {
						m = websocket.FormatCloseMessage(e.Code, e.Text)
					}
				}
				errC <- err
				_ = dst.WriteMessage(websocket.CloseMessage, m)
				break
			}
			err = dst.WriteMessage(msgType, msg)

			if trigger != nil {
				trigger(uid, int64(len(msg)))
			}

			if err != nil {
				errC <- err
				break
			}
		}
	}

	go replicateWebsocketConn(connPub, connBackend, errClient, w.OnDownload) // downlink
	go replicateWebsocketConn(connBackend, connPub, errBackend, w.OnUpload)  // uplink

	var message string
	select {
	case err = <-errClient:
		message = "reverse failed, fail copy from backend to client"
	case err = <-errBackend:
		message = "reverse failed, fail copy from client to backend"

	}
	if e, ok := err.(*websocket.CloseError); !ok || e.Code == websocket.CloseAbnormalClosure {
		if strings.Contains(err.Error(), "close 1006 (abnormal closure): unexpected EOF") {

		} else {
			w.logger.Errorw(message, "err", err.Error())
		}
	}
}

func (w *Proxy) MustRewrite(remote string) {
	uri, err := url.Parse(remote)
	if err != nil {
		log.Fatal(err)
	}
	if uri.Scheme != "wss" && uri.Scheme != "ws" {
		log.Fatal("only support websocket scheme")
	}
	if hostName := uri.Hostname(); net.ParseIP(hostName) == nil {
		w.HostName = strings.ToLower(hostName)
	}
	w.backend = MustRewrite(uri.String())
}
