package router

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"

	"github.com/convox/praxis/helpers"
	"github.com/convox/praxis/sdk/rack"
	"github.com/convox/praxis/types"
	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"

	mrand "math/rand"
)

type Proxy struct {
	Listen *url.URL
	Target *url.URL

	endpoint *Endpoint
}

func (e *Endpoint) NewProxy(host string, listen, target *url.URL) (*Proxy, error) {
	p := &Proxy{
		Listen:   listen,
		Target:   target,
		endpoint: e,
	}

	pi, err := strconv.Atoi(listen.Port())
	if err != nil {
		return nil, err
	}

	if _, ok := e.Proxies[pi]; ok {
		return nil, fmt.Errorf("proxy already exists for port: %d", pi)
	}

	e.Proxies[pi] = *p

	return p, nil
}

func (p Proxy) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]string{
		"listen": p.Listen.String(),
		"target": p.Target.String(),
	})
}

func (p *Proxy) Serve() error {
	ln, err := net.Listen("tcp", p.Listen.Host)
	if err != nil {
		return err
	}

	defer ln.Close()

	switch p.Listen.Scheme {
	case "https", "tls":
		cert, err := p.endpoint.router.generateCertificate(p.endpoint.Host)
		if err != nil {
			return err
		}

		cfg := &tls.Config{
			Certificates: []tls.Certificate{cert},
		}

		// TODO: check for h2
		cfg.NextProtos = []string{"h2"}

		ln = tls.NewListener(ln, cfg)
	}

	switch p.Listen.Scheme {
	case "http", "https":
		h, err := p.proxyHTTP(p.Listen, p.Target)
		if err != nil {
			return err
		}

		if err := http.Serve(ln, h); err != nil {
			return err
		}
	case "tcp":
		if err := proxyTCP(ln, p.Target); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown listener scheme: %s", p.Listen.Scheme)
	}

	return nil
}

func (p *Proxy) proxyHTTP(listen, target *url.URL) (http.Handler, error) {
	if target.Hostname() == "rack" {
		h, err := p.proxyRackHTTP()
		if err != nil {
			return nil, err
		}

		return h, nil
	}

	px := httputil.NewSingleHostReverseProxy(target)

	px.Transport = logTransport{RoundTripper: defaultTransport()}

	return px, nil
}

func proxyTCP(listener net.Listener, target *url.URL) error {
	for {
		cn, err := listener.Accept()
		if err != nil {
			return err
		}

		go proxyRackTCP(cn, target)
	}
}

func proxyTCPConnection(cn net.Conn, target *url.URL) error {
	if target.Hostname() == "rack" {
		return proxyRackTCP(cn, target)
	}

	defer cn.Close()

	oc, err := net.Dial("tcp", target.Host)
	if err != nil {
		return err
	}

	defer oc.Close()

	return helpers.Pipe(cn, oc)
}

func proxyRackTCP(cn net.Conn, target *url.URL) error {
	defer cn.Close()

	parts := strings.Split(target.Path, "/")

	if len(parts) < 4 {
		return fmt.Errorf("invalid rack endpoint: %s", target)
	}

	app := parts[1]
	kind := parts[2]
	rp := strings.Split(parts[3], ":")

	if len(rp) < 2 {
		return fmt.Errorf("invalid %s endpoint: %s", kind, parts[2])
	}

	resource := rp[0]

	var pr io.ReadCloser

	r, err := rack.NewFromEnv()
	if err != nil {
		return err
	}

	switch kind {
	case "resource":
		rc, err := r.ResourceProxy(app, resource, cn)
		if err != nil {
			return err
		}
		pr = rc
	default:
		return fmt.Errorf("unknown proxy type: %s", kind)
	}

	defer pr.Close()

	if err := helpers.Stream(cn, pr); err != nil {
		return err
	}

	return nil
}

func (p *Proxy) proxyRackHTTP() (http.Handler, error) {
	parts := strings.Split(p.Target.Path, "/")

	if len(parts) < 4 {
		return nil, fmt.Errorf("invalid rack endpoint")
	}

	app := parts[1]
	kind := parts[2]
	sp := strings.Split(parts[3], ":")

	if len(sp) < 2 {
		return nil, fmt.Errorf("invalid %s endpoint: %s", kind, parts[2])
	}

	service := sp[0]

	pi, err := strconv.Atoi(sp[1])
	if err != nil {
		return nil, err
	}

	rp := &httputil.ReverseProxy{Director: p.rackDirector}

	switch kind {
	case "service":
		rp.Transport = logTransport{RoundTripper: serviceTransport(app, service, pi)}
	default:
		return nil, fmt.Errorf("unknown proxy type: %s", kind)
	}

	px := mux.NewRouter()
	px.HandleFunc("/{path:.*}", p.ws(app, service, pi)).Methods("GET").Headers("Upgrade", "websocket")
	px.Handle("/{path:.*}", rp)

	return px, nil
}

func (p *Proxy) rackDirector(r *http.Request) {
	r.URL.Host = p.endpoint.Host
	r.URL.Scheme = p.Target.Scheme

	r.Header.Add("X-Forwarded-For", r.RemoteAddr)
	r.Header.Add("X-Forwarded-Port", p.Listen.Port())
	r.Header.Add("X-Forwarded-Proto", p.Listen.Scheme)
}

func serviceTransport(app, service string, port int) http.RoundTripper {
	tr := defaultTransport()

	tr.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		r, err := rack.NewFromEnv()
		if err != nil {
			return nil, err
		}

		pss, err := r.ProcessList(app, types.ProcessListOptions{Service: service})
		if err != nil {
			return nil, err
		}

		if len(pss) < 1 {
			return nil, fmt.Errorf("no processes available for service: %s", service)
		}

		ps := pss[mrand.Intn(len(pss))]

		a, b := net.Pipe()

		go serviceProxy(r, app, ps.Id, port, a)

		return b, nil
	}

	return tr
}

func serviceProxy(rk rack.Rack, app, pid string, port int, rw io.ReadWriter) error {
	pr, err := rk.ProcessProxy(app, pid, port, rw)
	if err != nil {
		return err
	}

	defer pr.Close()

	if _, err := io.Copy(rw, pr); err != nil {
		return err
	}

	return nil
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
}

func (p *Proxy) ws(app, service string, port int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		frontend, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			fmt.Printf("ns=convox.router at=proxy type=ws.upgrader error=%q\n", err)
			return
		}

		dialer := &websocket.Dialer{
			Proxy: http.ProxyFromEnvironment,
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
			},
		}

		dialer.NetDial = func(network, address string) (net.Conn, error) {
			r, err := rack.NewFromEnv()
			if err != nil {
				return nil, err
			}

			pss, err := r.ProcessList(app, types.ProcessListOptions{Service: service})
			if err != nil {
				return nil, err
			}

			if len(pss) < 1 {
				return nil, fmt.Errorf("no processes available for service: %s", service)
			}

			ps := pss[mrand.Intn(len(pss))]

			a, b := net.Pipe()

			go serviceProxy(r, app, ps.Id, port, a)

			return &nopDeadlineConn{b}, nil
		}

		r.URL.Host = p.endpoint.Host
		r.URL.Scheme = "wss"

		headers := http.Header{}
		headers.Add("X-Forwarded-For", r.RemoteAddr)
		headers.Add("X-Forwarded-Port", p.Listen.Port())
		headers.Add("X-Forwarded-Proto", p.Listen.Scheme)

		for k, v := range r.Header {
			// Websocket headers to skip as they are set by the dialer and duplicates aren't allowed
			if k == "Upgrade" || k == "Connection" || k == "Sec-Websocket-Key" ||
				k == "Sec-Websocket-Version" || k == "Sec-Websocket-Extensions" || k == "Sec-Websocket-Protocol" {
				continue
			}
			for _, s := range v {
				headers.Add(k, s)
			}
		}

		backend, _, err := dialer.Dial(r.URL.String(), headers)
		if err != nil {
			fmt.Printf("ns=convox.router at=proxy type=ws.dial error=%q\n", err)
			return
		}

		errc := make(chan error, 2)
		cp := func(dst io.Writer, src io.Reader) {
			_, err := io.Copy(dst, src)
			errc <- err
		}

		go cp(frontend.UnderlyingConn(), backend.UnderlyingConn())
		go cp(backend.UnderlyingConn(), frontend.UnderlyingConn())

		if err := <-errc; err != nil {
			fmt.Printf("ns=convox.router at=proxy type=ws.cp error=%q\n", err)
		}
	}
}
