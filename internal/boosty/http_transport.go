package boosty

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"time"
)

const publicDialTimeout = 12 * time.Second

type resolvePublicHost func(context.Context, string) ([]net.IPAddr, error)
type dialPublicAddress func(context.Context, string, string) (net.Conn, error)

// DNS and all fallback addresses share one deadline. Dial only the numeric
// public IP which was checked, never the original hostname a second time.
func dialPublicHost(ctx context.Context, network, address string, resolve resolvePublicHost, dial dialPublicAddress) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, publicDialTimeout)
	defer cancel()
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	ips, err := resolve(ctx, host)
	if err != nil {
		return nil, err
	}
	var last error
	for _, ip := range ips {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !permittedIP(ip.IP) {
			continue
		}
		conn, err := dial(ctx, network, net.JoinHostPort(ip.IP.String(), port))
		if err == nil {
			return conn, nil
		}
		last = err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if last != nil {
		return nil, last
	}
	return nil, errors.New("сетевой адрес запрещён")
}

func safeMediaClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	dialer := &net.Dialer{Timeout: publicDialTimeout, KeepAlive: 30 * time.Second}
	tr.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return dialPublicHost(ctx, network, address, net.DefaultResolver.LookupIPAddr, dialer.DialContext)
	}
	tr.ResponseHeaderTimeout = 25 * time.Second
	tr.MaxIdleConns = 16
	tr.MaxIdleConnsPerHost = 4
	// Session resumption is local to this transport. Full certificate
	// validation, TLS server name, and existing HTTP/2 settings remain intact.
	if tr.TLSClientConfig == nil {
		tr.TLSClientConfig = &tls.Config{}
	}
	tr.TLSClientConfig.ClientSessionCache = tls.NewLRUClientSessionCache(16)
	return &http.Client{Transport: tr, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 4 {
			return errors.New("слишком много переадресаций")
		}
		return validateMediaURL(req.URL.String())
	}}
}
