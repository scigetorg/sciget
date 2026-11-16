package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// download is what a downloaded file must match. A zero field is not
// checked.
type download struct {
	size   int64
	sha256 string // lowercase hex
}

// downloadFile downloads url into a new file beside dest, named
// "<dest's name>.*.incomplete", and returns that file's path. The file
// holds exactly the size HEAD reports and matches want. On any error the
// file is deleted. dest's directory must exist. progress, if not nil, is
// called at most twice a second and once at the end.
func downloadFile(client *http.Client, url, dest string, want download, progress func(done, total int64)) (string, error) {
	size, err := headSize(client, url)
	if err != nil {
		return "", err
	}
	if want.size != 0 && size != want.size {
		return "", fmt.Errorf("%s is %d bytes, but the catalogue says %d", url, size, want.size)
	}
	dir := filepath.Dir(dest)
	free, err := freeSpace(dir)
	if err != nil {
		return "", err
	}
	if err := checkSpace(free, size, dir); err != nil {
		return "", err
	}

	resp, err := client.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("downloading %s: %s", url, resp.Status)
	}

	f, err := os.CreateTemp(dir, filepath.Base(dest)+".*.incomplete")
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	var w io.Writer = f
	if want.sha256 != "" {
		w = io.MultiWriter(f, hash)
	}
	// One byte past the size is enough to know the body is too long, so a
	// server that sends more cannot fill the disk.
	body := &progressReader{r: io.LimitReader(resp.Body, size+1), total: size, progress: progress}
	n, err := io.Copy(w, body)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	switch {
	case err != nil:
		err = fmt.Errorf("downloading %s: %w", url, err)
	case n != size:
		err = fmt.Errorf("downloading %s: got %d bytes, expected %d", url, n, size)
	case want.sha256 != "" && hex.EncodeToString(hash.Sum(nil)) != want.sha256:
		err = fmt.Errorf("downloading %s: the SHA-256 does not match the catalogue", url)
	}
	if err != nil {
		os.Remove(f.Name())
		return "", err
	}
	if progress != nil {
		progress(n, size)
	}
	return f.Name(), nil
}

// headSize returns the size a HEAD request for url reports.
func headSize(client *http.Client, url string) (int64, error) {
	resp, err := client.Head(url)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("checking %s: %s", url, resp.Status)
	}
	size, err := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64)
	if err != nil || size < 0 {
		return 0, fmt.Errorf("checking %s: no size given", url)
	}
	return size, nil
}

// checkSpace returns an error if free bytes cannot hold need bytes in dir.
// A negative free means unknown, and passes.
func checkSpace(free, need int64, dir string) error {
	if free >= 0 && free < need {
		return fmt.Errorf("not enough space in %s: needs %s, %s free", dir, humanSize(need), humanSize(free))
	}
	return nil
}

// humanSize formats n bytes in decimal units: "8.8 GB", "512 B".
func humanSize(n int64) string {
	switch {
	case n >= 1e9:
		return fmt.Sprintf("%.1f GB", float64(n)/1e9)
	case n >= 1e6:
		return fmt.Sprintf("%.1f MB", float64(n)/1e6)
	case n >= 1e3:
		return fmt.Sprintf("%.1f kB", float64(n)/1e3)
	}
	return fmt.Sprintf("%d B", n)
}

// progressReader calls progress as bytes are read from r, at most twice a
// second.
type progressReader struct {
	r        io.Reader
	total    int64
	done     int64
	last     time.Time
	progress func(done, total int64)
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.done += int64(n)
	if p.progress != nil && time.Since(p.last) >= 500*time.Millisecond {
		p.last = time.Now()
		p.progress(p.done, p.total)
	}
	return n, err
}

// newDownloadClient returns a client for downloads of any length: it has no
// overall timeout, but a connection that sends or receives nothing for
// network_timeout_seconds fails, as does a server that takes that long to
// answer. Redirects must stay on https.
func newDownloadClient(cfg config) (*http.Client, error) {
	seconds, err := cfg.getNumber("network_timeout_seconds")
	if err != nil {
		return nil, err
	}
	timeout := time.Duration(seconds) * time.Second
	dialer := &net.Dialer{Timeout: timeout}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := dialer.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return idleConn{Conn: conn, timeout: timeout}, nil
	}
	transport.ResponseHeaderTimeout = timeout
	// Asking for gzip would let a server send a body whose length differs
	// from the size HEAD reports.
	transport.DisableCompression = true
	return &http.Client{Transport: transport, CheckRedirect: httpsOnly}, nil
}

// idleConn is a connection whose every read and write must make progress
// within timeout.
type idleConn struct {
	net.Conn
	timeout time.Duration
}

func (c idleConn) Read(b []byte) (int, error) {
	c.Conn.SetReadDeadline(time.Now().Add(c.timeout))
	return c.Conn.Read(b)
}

func (c idleConn) Write(b []byte) (int, error) {
	c.Conn.SetWriteDeadline(time.Now().Add(c.timeout))
	return c.Conn.Write(b)
}
