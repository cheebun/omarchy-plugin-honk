package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// errConfig marks errors that retrying cannot fix; streams exit with code 2 on them.
var errConfig = errors.New("config error")

// apiError is a non-2xx answer from the native API. Bodies are
// {"error":{"code","message"},"request_id"} (docs/reference/openapi).
type apiError struct {
	Status  int
	Code    string
	Message string
}

func (e *apiError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("%s (%s)", e.Message, e.Code)
	}
	return fmt.Sprintf("HTTP %d", e.Status)
}

// client talks to honk's native API (/api/v1) with a bearer secret.
type client struct {
	base   string
	secret string
	http   *http.Client
}

// newClient validates the base URL. It refuses to send a secret over
// cleartext HTTP to a non-loopback host.
func newClient(raw, secret string) (*client, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = defaultURL
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errConfig, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("%w: unsupported scheme %q", errConfig, u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("%w: missing host in %q", errConfig, raw)
	}
	if u.Scheme == "http" && secret != "" && !isLoopback(u.Hostname()) {
		return nil, fmt.Errorf("%w: refusing to send the API secret over cleartext to non-loopback host %q", errConfig, u.Hostname())
	}
	return &client{
		base:   strings.TrimRight(u.String(), "/") + "/api/v1",
		secret: secret,
		http:   &http.Client{},
	}, nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (c *client) newRequest(ctx context.Context, method, path string, body any) (*http.Request, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.secret != "" {
		req.Header.Set("Authorization", "Bearer "+c.secret)
	}
	return req, nil
}

// do sends one request and decodes a JSON answer into out (when non-nil).
func (c *client) do(ctx context.Context, method, path string, body, out any) error {
	req, err := c.newRequest(ctx, method, path, body)
	if err != nil {
		return err
	}
	return c.send(req, out)
}

// send performs a prepared request (for extra headers such as If-Match).
func (c *client) send(req *http.Request, out any) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return parseAPIError(resp.StatusCode, data)
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

func parseAPIError(status int, body []byte) error {
	e := &apiError{Status: status}
	var wrapped struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &wrapped) == nil {
		e.Code, e.Message = wrapped.Error.Code, wrapped.Error.Message
	}
	return e
}

// statusCode maps errors onto the HTTP-like codes the QML side checks:
// 401 for credential problems, 0 when honk is unreachable.
func statusCode(err error) int {
	var ae *apiError
	if errors.As(err, &ae) {
		if ae.Status == 403 {
			return 401
		}
		return ae.Status
	}
	if errors.Is(err, errConfig) {
		return 500
	}
	return 0
}

func errorMessage(err error) string {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.Error()
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err.Error()
	}
	return err.Error()
}

func offline(err error) map[string]any {
	return map[string]any{"type": "offline", "online": false, "status": statusCode(err), "error": errorMessage(err)}
}

// u64 decodes the native API's UInt64, which is a decimal string on the wire
// (schema UInt64: type string), plus null and plain numbers.
type u64 struct {
	V     int64
	Valid bool
}

func (u *u64) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "null" || s == "" {
		*u = u64{}
		return nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return err
	}
	*u = u64{V: int64(f), Valid: true}
	return nil
}

// lineWriter serializes JSON lines so a line is never interleaved.
type lineWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lineWriter) emit(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, err := l.w.Write(append(b, '\n')); err != nil {
		// stdout is gone (reader exited); nothing left to serve.
		os.Exit(0)
	}
}

// stream runs session until ctx ends, reconnecting with exponential backoff
// and reporting each failure as an offline line. The backoff resets once a
// session delivered data. Config errors end the stream with exit code 2.
func stream(ctx context.Context, out *lineWriter, c *client, session func(context.Context, *client) (bool, error)) {
	watchParent()
	backoff := time.Second
	for ctx.Err() == nil {
		delivered, err := session(ctx, c)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			err = errors.New("stream closed by honk")
		}
		out.emit(offline(err))
		if errors.Is(err, errConfig) {
			os.Exit(2)
		}
		if delivered {
			backoff = time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

// watchParent ends the agent when the process that started it goes away, so a
// crashed shell does not leave it running.
func watchParent() {
	ppid := os.Getppid()
	_, _, _ = syscall.RawSyscall(syscall.SYS_PRCTL, syscall.PR_SET_PDEATHSIG, uintptr(syscall.SIGTERM), 0)
	// PDEATHSIG fires on the parent thread's exit, not the process's; poll as a backstop.
	go func() {
		for range time.Tick(2 * time.Second) {
			if os.Getppid() != ppid {
				os.Exit(0)
			}
		}
	}()
}

func formatBytes(b int64) string {
	if b <= 0 {
		return "0 B"
	}
	units := []string{"B", "KB", "MB", "GB", "TB"}
	val := float64(b)
	i := 0
	for val >= 1024 && i < len(units)-1 {
		val /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%.0f B", val)
	}
	return fmt.Sprintf("%.1f %s", val, units[i])
}

func formatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Truncate(time.Second)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh%dm%ds", h, m, s)
	case m > 0:
		return fmt.Sprintf("%dm%ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}
