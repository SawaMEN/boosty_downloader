package boosty

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestPublicDialSharedDeadlineAndFiltering(t *testing.T) {
	var deadline time.Time
	var calls int
	failure := errors.New("unreachable")
	resolve := func(ctx context.Context, host string) ([]net.IPAddr, error) {
		var ok bool
		deadline, ok = ctx.Deadline()
		if !ok || host != "cdn.example" || time.Until(deadline) > publicDialTimeout {
			t.Fatal("DNS lacks bounded deadline")
		}
		return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}, {IP: net.ParseIP("8.8.8.8")}, {IP: net.ParseIP("1.1.1.1")}}, nil
	}
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		calls++
		got, ok := ctx.Deadline()
		if !ok || !got.Equal(deadline) || network != "tcp" {
			t.Fatal("fallback reset deadline")
		}
		if calls == 1 && address != "8.8.8.8:443" || calls == 2 && address != "1.1.1.1:443" {
			t.Fatalf("unexpected address: %s", address)
		}
		return nil, failure
	}
	_, err := dialPublicHost(context.Background(), "tcp", "cdn.example:443", resolve, dial)
	if calls != 2 || !errors.Is(err, failure) {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

func TestPublicDialCancellationStopsFallback(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resolve := func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}, {IP: net.ParseIP("1.1.1.1")}}, nil
	}
	calls := 0
	dial := func(context.Context, string, string) (net.Conn, error) {
		calls++
		cancel()
		return nil, errors.New("connection failed")
	}
	if _, err := dialPublicHost(ctx, "tcp", "cdn.example:443", resolve, dial); !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("cancellation ignored: %d %v", calls, err)
	}
}

func TestOwnerHTTPPoolsRemainSeparate(t *testing.T) {
	a, b := New(Tokens{}, nil), New(Tokens{}, nil)
	defer a.CloseIdleConnections()
	defer b.CloseIdleConnections()
	seen := map[*http.Transport]bool{}
	for _, c := range []*http.Client{a.api, a.media, b.api, b.media} {
		tr := c.Transport.(*http.Transport)
		if seen[tr] || tr.Proxy != nil || tr.MaxIdleConns != 16 || tr.MaxIdleConnsPerHost != 4 ||
			tr.ResponseHeaderTimeout != 25*time.Second || tr.TLSClientConfig == nil ||
			tr.TLSClientConfig.InsecureSkipVerify || tr.TLSClientConfig.ClientSessionCache == nil {
			t.Fatal("unsafe or shared transport")
		}
		seen[tr] = true
		req, err := http.NewRequest("GET", "http://cdn.example/media", nil)
		if err != nil || c.CheckRedirect(req, nil) == nil {
			t.Fatal("unsafe redirect accepted")
		}
	}
}
