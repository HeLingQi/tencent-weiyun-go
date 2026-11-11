package remote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var ErrUnsafeDownload = errors.New("unsafe download URL")

type DownloadStatusError struct {
	StatusCode int
}

func (e *DownloadStatusError) Error() string {
	return fmt.Sprintf("download HTTP status %d", e.StatusCode)
}

type Downloader struct {
	HTTPClient    *http.Client
	LookupIP      func(context.Context, string) ([]net.IP, error)
	mu            sync.Mutex
	defaultClient *http.Client
}

func (d *Downloader) baseClient(lookup func(context.Context, string) ([]net.IP, error)) *http.Client {
	if d.HTTPClient != nil {
		copy := *d.HTTPClient
		return &copy
	}
	d.mu.Lock()
	if d.defaultClient == nil {
		d.defaultClient = secureHTTPClient(lookup)
	}
	shared := *d.defaultClient
	d.mu.Unlock()
	return &shared
}

func (d *Downloader) doRequest(ctx context.Context, rawURL, cookie, rangeHeader string) (*http.Response, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil {
		return nil, ErrUnsafeDownload
	}
	lookup := d.LookupIP
	if lookup == nil {
		lookup = func(ctx context.Context, host string) ([]net.IP, error) {
			return net.DefaultResolver.LookupIP(ctx, "ip", host)
		}
	}
	if err := validateHost(ctx, parsed.Hostname(), lookup); err != nil {
		return nil, err
	}

	client := d.baseClient(lookup)
	previousRedirect := client.CheckRedirect
	client.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if len(via) >= 5 || request.URL.Scheme != "https" || !strings.EqualFold(request.URL.Host, parsed.Host) {
			return ErrUnsafeDownload
		}
		if err := validateHost(request.Context(), request.URL.Hostname(), lookup); err != nil {
			return err
		}
		if previousRedirect != nil {
			return previousRedirect(request, via)
		}
		return nil
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, ErrUnsafeDownload
	}
	if cookie != "" {
		request.Header.Set("Cookie", cookie)
	}
	if rangeHeader != "" {
		request.Header.Set("Range", rangeHeader)
	}
	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, ErrUnsafeDownload) {
			return nil, ErrUnsafeDownload
		}
		return nil, errors.New("download transport error")
	}
	return response, nil
}

func (d *Downloader) Download(ctx context.Context, rawURL, cookie string, expectedSize int64, target string) error {
	if _, err := os.Lstat(target); err == nil {
		return ErrConflict
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	response, err := d.doRequest(ctx, rawURL, cookie, "")
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return &DownloadStatusError{StatusCode: response.StatusCode}
	}

	dir := filepath.Dir(target)
	temporary, err := os.CreateTemp(dir, "."+filepath.Base(target)+".tmp-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	written, copyErr := io.Copy(temporary, response.Body)
	closeErr := temporary.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if expectedSize >= 0 && written != expectedSize {
		return fmt.Errorf("download size mismatch: got %d, want %d", written, expectedSize)
	}
	if err := os.Link(temporaryName, target); err != nil {
		if errors.Is(err, os.ErrExist) {
			return ErrConflict
		}
		return err
	}
	return nil
}

func (d *Downloader) DownloadRange(ctx context.Context, rawURL, cookie string, offset int64, dest []byte) error {
	if len(dest) == 0 {
		return nil
	}
	if offset < 0 {
		return errors.New("negative range offset")
	}
	end := offset + int64(len(dest)) - 1
	response, err := d.doRequest(ctx, rawURL, cookie, fmt.Sprintf("bytes=%d-%d", offset, end))
	if err != nil {
		return err
	}
	defer response.Body.Close()

	switch response.StatusCode {
	case http.StatusPartialContent:
		wantPrefix := fmt.Sprintf("bytes %d-%d/", offset, end)
		if !strings.HasPrefix(response.Header.Get("Content-Range"), wantPrefix) {
			return errors.New("download Content-Range mismatch")
		}
	case http.StatusOK:
		if offset != 0 {
			return errors.New("upstream ignored Range header")
		}
	default:
		return &DownloadStatusError{StatusCode: response.StatusCode}
	}
	if _, err := io.ReadFull(response.Body, dest); err != nil {
		return err
	}
	return nil
}

func secureHTTPClient(lookup func(context.Context, string) ([]net.IP, error)) *http.Client {
	dialer := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			ips, err := lookup(ctx, host)
			if err != nil || len(ips) == 0 {
				return nil, ErrUnsafeDownload
			}
			for _, ip := range ips {
				if !safeIP(ip) {
					return nil, ErrUnsafeDownload
				}
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].String(), port))
		},
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		IdleConnTimeout:       90 * time.Second,
	}
	return &http.Client{Transport: transport, Timeout: 30 * time.Minute}
}

func validateHost(ctx context.Context, host string, lookup func(context.Context, string) ([]net.IP, error)) error {
	if strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
		return ErrUnsafeDownload
	}
	if ip := net.ParseIP(host); ip != nil {
		if !safeIP(ip) {
			return ErrUnsafeDownload
		}
		return nil
	}
	ips, err := lookup(ctx, host)
	if err != nil || len(ips) == 0 {
		return ErrUnsafeDownload
	}
	for _, ip := range ips {
		if !safeIP(ip) {
			return ErrUnsafeDownload
		}
	}
	return nil
}

func safeIP(ip net.IP) bool {
	return ip != nil && !ip.IsLoopback() && !ip.IsPrivate() && !ip.IsUnspecified() && !ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast() && !ip.IsMulticast()
}
