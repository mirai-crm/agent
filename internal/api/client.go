package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Errors that callers classify to drive retry/skip behaviour.
var (
	// ErrUnauthorized is returned on 401 (invalid token / archived device).
	ErrUnauthorized = errors.New("unauthorized")
	// ErrNotFound is returned on 404 (document/model not found) — permanent.
	ErrNotFound = errors.New("not found")
	// ErrBadRequest is returned on 400 (bad params/body) — permanent config error.
	ErrBadRequest = errors.New("bad request")
)

// APIError is a non-2xx server response.
type APIError struct {
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("api error: status %d: %s", e.StatusCode, e.Body)
}

// Transient reports whether an APIError is worth retrying (5xx / 429).
func (e *APIError) Transient() bool {
	return e.StatusCode >= 500 || e.StatusCode == http.StatusTooManyRequests
}

// Client talks to the CRM device API for a single device token.
type Client struct {
	baseURL      string
	token        string
	httpClient   *http.Client
	longPollHTTP *http.Client
	userAgent    string
}

// Config configures a Client.
type Config struct {
	BaseURL         string
	Token           string
	RequestTimeout  time.Duration
	LongpollTimeout time.Duration
	UserAgent       string
}

// New builds a Client. It uses two http.Clients so that long-poll requests can
// outlive the short per-request timeout.
func New(cfg Config) *Client {
	ua := cfg.UserAgent
	if ua == "" {
		ua = "mirai-agent"
	}
	return &Client{
		baseURL:      strings.TrimRight(cfg.BaseURL, "/"),
		token:        cfg.Token,
		httpClient:   &http.Client{Timeout: cfg.RequestTimeout},
		longPollHTTP: &http.Client{Timeout: cfg.LongpollTimeout},
		userAgent:    ua,
	}
}

func (c *Client) newRequest(ctx context.Context, method, path string, query url.Values, body io.Reader) (*http.Request, error) {
	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("User-Agent", c.userAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// statusError maps a status code to a sentinel/typed error, or nil for 2xx.
func statusError(resp *http.Response, body []byte) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return ErrUnauthorized
	case http.StatusNotFound:
		return ErrNotFound
	case http.StatusBadRequest:
		return ErrBadRequest
	default:
		return &APIError{StatusCode: resp.StatusCode, Body: strings.TrimSpace(string(body))}
	}
}

// Info fetches device self-discovery data. Used at bootstrap.
func (c *Client) Info(ctx context.Context) (DeviceInfo, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/api/v1/devices/info", nil, nil)
	if err != nil {
		return DeviceInfo{}, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return DeviceInfo{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err := statusError(resp, body); err != nil {
		return DeviceInfo{}, err
	}
	var out infoResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return DeviceInfo{}, fmt.Errorf("decode info: %w", err)
	}
	return out.Device, nil
}

// PollTasks performs GET /tasks with long-poll semantics. Uses the long-poll
// http client so timeoutSeconds can approach the server maximum.
func (c *Client) PollTasks(ctx context.Context, timeoutSeconds, batchSize int) ([]Task, error) {
	q := url.Values{}
	q.Set("timeout", strconv.Itoa(timeoutSeconds))
	q.Set("batch_size", strconv.Itoa(batchSize))
	req, err := c.newRequest(ctx, http.MethodGet, "/api/v1/devices/tasks", q, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.longPollHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err := statusError(resp, body); err != nil {
		return nil, err
	}
	var out TasksResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode tasks: %w", err)
	}
	return out.Tasks, nil
}

// Finalize marks tasks completed. Success items omit ErrorMessage.
func (c *Client) Finalize(ctx context.Context, items []FinalizeItem) (FinalizeResponse, error) {
	payload, err := json.Marshal(FinalizeRequest{Tasks: items})
	if err != nil {
		return FinalizeResponse{}, err
	}
	req, err := c.newRequest(ctx, http.MethodPost, "/api/v1/devices/tasks/finalize", nil, bytes.NewReader(payload))
	if err != nil {
		return FinalizeResponse{}, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return FinalizeResponse{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err := statusError(resp, body); err != nil {
		return FinalizeResponse{}, err
	}
	var out FinalizeResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return FinalizeResponse{}, fmt.Errorf("decode finalize: %w", err)
	}
	return out, nil
}

// Ping sends the presence heartbeat.
func (c *Client) Ping(ctx context.Context) (PingResponse, error) {
	req, err := c.newRequest(ctx, http.MethodPost, "/api/v1/devices/ping", nil, nil)
	if err != nil {
		return PingResponse{}, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return PingResponse{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err := statusError(resp, body); err != nil {
		return PingResponse{}, err
	}
	var out PingResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return PingResponse{}, fmt.Errorf("decode ping: %w", err)
	}
	return out, nil
}

// FetchPNG downloads a print-ready PNG for the given document path (e.g.
// "/api/v1/devices/checks/8842/png"), requesting mono raster and optional scale.
// Returns the raw PNG bytes.
func (c *Client) FetchPNG(ctx context.Context, path string, scale int) ([]byte, error) {
	q := url.Values{}
	q.Set("mono", "1")
	if scale > 0 {
		q.Set("scale", strconv.Itoa(scale))
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, q, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "image/png")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	// Read with a generous cap to avoid unbounded memory on a misbehaving server.
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err := statusError(resp, body); err != nil {
		return nil, err
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "image/png") {
		return nil, fmt.Errorf("unexpected content-type %q (want image/png)", ct)
	}
	if len(body) == 0 {
		return nil, fmt.Errorf("empty PNG body")
	}
	return body, nil
}
