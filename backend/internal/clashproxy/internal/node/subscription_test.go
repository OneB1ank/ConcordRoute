package node

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func TestDownloadSubscriptionRejectsBlockedHosts(t *testing.T) {
	t.Parallel()
	cases := []string{
		"http://localhost/sub.yaml",
		"http://127.0.0.1/sub.yaml",
		"http://10.0.0.1/sub.yaml",
		"http://100.64.0.1/sub.yaml",
		"http://169.254.169.254/latest/meta-data",
		"http://metadata.google.internal/computeMetadata/v1/",
		"http://[::1]/sub.yaml",
		"http://[fc00::1]/sub.yaml",
		"http://[fe80::1]/sub.yaml",
	}
	for _, rawURL := range cases {
		rawURL := rawURL
		t.Run(rawURL, func(t *testing.T) {
			t.Parallel()
			_, err := DownloadSubscription(context.Background(), rawURL, SubscriptionDownloadOptions{
				AllowInsecureHTTP: true,
				ResolveIP: func(context.Context, string) ([]net.IP, error) {
					return []net.IP{net.ParseIP("203.0.113.10")}, nil
				},
			})
			if err == nil {
				t.Fatalf("expected blocked host error")
			}
		})
	}
}

func TestDownloadSubscriptionRejectsPrivateResolvedIP(t *testing.T) {
	t.Parallel()
	_, err := DownloadSubscription(context.Background(), "https://example.com/sub.yaml", SubscriptionDownloadOptions{
		ResolveIP: func(context.Context, string) ([]net.IP, error) {
			return []net.IP{net.ParseIP("192.168.1.10")}, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "resolved ip") {
		t.Fatalf("expected private resolved ip error, got %v", err)
	}
}

func TestDownloadSubscriptionRejectsOversizedBody(t *testing.T) {
	t.Parallel()
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("123456")),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})}
	_, err := DownloadSubscription(context.Background(), "https://example.com/sub.yaml", SubscriptionDownloadOptions{
		HTTPClient: client,
		MaxBytes:   5,
		ResolveIP: func(context.Context, string) ([]net.IP, error) {
			return []net.IP{net.ParseIP("203.0.113.10")}, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("expected oversized body error, got %v", err)
	}
}

func TestDownloadSubscriptionReturnsBody(t *testing.T) {
	t.Parallel()
	want := "proxies:\n  - name: edge\n    type: http\n    server: example.com\n    port: 8080\n"
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("User-Agent") == "" {
			return nil, errors.New("missing user agent")
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(want)),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})}
	got, err := DownloadSubscription(context.Background(), "https://example.com/sub.yaml", SubscriptionDownloadOptions{
		HTTPClient: client,
		ResolveIP: func(context.Context, string) ([]net.IP, error) {
			return []net.IP{net.ParseIP("203.0.113.10")}, nil
		},
	})
	if err != nil {
		t.Fatalf("DownloadSubscription() error = %v", err)
	}
	if string(got) != want {
		t.Fatalf("DownloadSubscription() = %q, want %q", got, want)
	}
}

func TestDownloadSubscriptionAllowsPrivateHostOnlyWhenExplicit(t *testing.T) {
	want := "proxies: []\n"
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(want)),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})}
	got, err := DownloadSubscription(context.Background(), "https://proxy.internal/sub.yaml", SubscriptionDownloadOptions{
		HTTPClient:   client,
		AllowPrivate: true,
		ResolveIP: func(context.Context, string) ([]net.IP, error) {
			return []net.IP{net.ParseIP("192.168.1.10")}, nil
		},
	})
	if err != nil {
		t.Fatalf("DownloadSubscription() error = %v", err)
	}
	if string(got) != want {
		t.Fatalf("DownloadSubscription() = %q, want %q", got, want)
	}
}

func TestDownloadSubscriptionAlwaysRejectsMetadataEndpoint(t *testing.T) {
	for _, rawURL := range []string{
		"http://169.254.169.254/latest/meta-data",
		"http://169.254.170.2/v2/credentials",
		"http://100.100.100.200/latest/meta-data",
		"http://192.0.0.192/latest/meta-data",
		"http://[fd00:ec2::254]/latest/meta-data",
	} {
		_, err := DownloadSubscription(context.Background(), rawURL, SubscriptionDownloadOptions{
			AllowInsecureHTTP: true,
			AllowPrivate:      true,
		})
		if err == nil {
			t.Fatalf("expected metadata endpoint %s to remain blocked", rawURL)
		}
	}
}

func TestDownloadSubscriptionRejectsRedirectToPrivateHost(t *testing.T) {
	calls := 0
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{
			StatusCode: http.StatusFound,
			Status:     "302 Found",
			Body:       io.NopCloser(strings.NewReader("")),
			Header:     http.Header{"Location": []string{"https://127.0.0.1/private"}},
			Request:    req,
		}, nil
	})}
	_, err := DownloadSubscription(context.Background(), "https://example.com/sub.yaml", SubscriptionDownloadOptions{
		HTTPClient: client,
		ResolveIP: func(context.Context, string) ([]net.IP, error) {
			return []net.IP{net.ParseIP("203.0.113.10")}, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("expected private redirect rejection, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("redirect target should not be requested, got %d transport calls", calls)
	}
}
