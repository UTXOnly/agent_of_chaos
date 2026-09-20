// Package ddapi is the small slice of the Datadog HTTP API the harness needs:
// metric submission (v2 series), events, notebooks and key validation.
package ddapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/klauspost/compress/gzip"
)

// Client talks to one Datadog site with an API key (and optionally an
// application key, needed for notebooks).
type Client struct {
	Site   string
	APIKey string
	AppKey string
	HTTP   *http.Client
	// BaseURL overrides https://api.<site> (tests).
	BaseURL string
}

// FromEnv builds a client from DD_API_KEY / DD_APP_KEY / DD_SITE.
func FromEnv() *Client {
	return New(os.Getenv("DD_SITE"), os.Getenv("DD_API_KEY"), os.Getenv("DD_APP_KEY"))
}

func New(site, apiKey, appKey string) *Client {
	if site == "" {
		site = "datadoghq.com"
	}
	return &Client{Site: site, APIKey: apiKey, AppKey: appKey, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

// Configured reports whether an API key is present.
func (c *Client) Configured() bool { return c != nil && c.APIKey != "" }

func (c *Client) apiURL(path string) string {
	if c.BaseURL != "" {
		return strings.TrimRight(c.BaseURL, "/") + path
	}
	return "https://api." + c.Site + path
}

// AppURL is the browser-facing base URL for the site: https://app.<site>
// for the main sites, https://<site> for the regional ones.
func AppURL(site string) string {
	return AppURLFor(site, "", "")
}

// AppURLFor resolves the browser URL with an org's custom subdomain
// (DD_SUBDOMAIN=bhartford → https://bhartford.datadoghq.com) or a full
// override (DD_APP_URL), which wins when set.
func AppURLFor(site, subdomain, override string) string {
	if override != "" {
		return strings.TrimRight(override, "/")
	}
	if site == "" {
		site = "datadoghq.com"
	}
	if subdomain != "" {
		return "https://" + subdomain + "." + site
	}
	switch site {
	case "datadoghq.com", "datadoghq.eu", "ddog-gov.com":
		return "https://app." + site
	}
	return "https://" + site
}

// AppURLFromEnv is AppURLFor with DD_SITE, DD_SUBDOMAIN and DD_APP_URL.
func AppURLFromEnv() string {
	return AppURLFor(os.Getenv("DD_SITE"), os.Getenv("DD_SUBDOMAIN"), os.Getenv("DD_APP_URL"))
}

// Error is a non-2xx response.
type Error struct {
	Status int
	Body   string
}

func (e *Error) Error() string { return fmt.Sprintf("datadog API: HTTP %d: %s", e.Status, e.Body) }

func (c *Client) do(ctx context.Context, method, path string, body []byte, gz bool, needAppKey bool) ([]byte, error) {
	if c.APIKey == "" {
		return nil, errors.New("DD_API_KEY is not set")
	}
	if needAppKey && c.AppKey == "" {
		return nil, errors.New("DD_APP_KEY is not set (an application key is required for this call)")
	}
	var rdr io.Reader
	if body != nil {
		if gz {
			var buf bytes.Buffer
			zw := gzip.NewWriter(&buf)
			zw.Write(body)
			zw.Close()
			rdr = &buf
		} else {
			rdr = bytes.NewReader(body)
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.apiURL(path), rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("DD-API-KEY", c.APIKey)
	if c.AppKey != "" {
		req.Header.Set("DD-APPLICATION-KEY", c.AppKey)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
		if gz {
			req.Header.Set("Content-Encoding", "gzip")
		}
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(out))
		if len(msg) > 500 {
			msg = msg[:500] + "…"
		}
		return out, &Error{Status: resp.StatusCode, Body: msg}
	}
	return out, nil
}

// Validate checks the API key against /api/v1/validate.
func (c *Client) Validate(ctx context.Context) error {
	out, err := c.do(ctx, "GET", "/api/v1/validate", nil, false, false)
	if err != nil {
		return err
	}
	var v struct {
		Valid bool `json:"valid"`
	}
	if json.Unmarshal(out, &v) == nil && !v.Valid {
		return errors.New("API key rejected")
	}
	return nil
}

// Metric types for v2 series.
const (
	TypeUnspecified = 0
	TypeCount       = 1
	TypeRate        = 2
	TypeGauge       = 3
)

type Point struct {
	Timestamp int64   `json:"timestamp"`
	Value     float64 `json:"value"`
}

type Series struct {
	Metric   string   `json:"metric"`
	Type     int      `json:"type"`
	Points   []Point  `json:"points"`
	Tags     []string `json:"tags,omitempty"`
	Unit     string   `json:"unit,omitempty"`
	Interval int64    `json:"interval,omitempty"`
}

// SubmitSeries posts to /api/v2/series in batches of 1000, gzip-compressed.
func (c *Client) SubmitSeries(ctx context.Context, series []Series) error {
	for len(series) > 0 {
		n := min(len(series), 1000)
		body, err := json.Marshal(map[string]any{"series": series[:n]})
		if err != nil {
			return err
		}
		if _, err := c.do(ctx, "POST", "/api/v2/series", body, true, false); err != nil {
			return err
		}
		series = series[n:]
	}
	return nil
}

// Event is a v1 event.
type Event struct {
	Title          string   `json:"title"`
	Text           string   `json:"text"`
	Tags           []string `json:"tags,omitempty"`
	AlertType      string   `json:"alert_type,omitempty"` // error, warning, info, success
	Priority       string   `json:"priority,omitempty"`   // normal, low
	SourceTypeName string   `json:"source_type_name,omitempty"`
	AggregationKey string   `json:"aggregation_key,omitempty"`
	DateHappened   int64    `json:"date_happened,omitempty"`
	Host           string   `json:"host,omitempty"`
}

// PostEvent posts to /api/v1/events.
func (c *Client) PostEvent(ctx context.Context, ev Event) error {
	body, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	_, err = c.do(ctx, "POST", "/api/v1/events", body, false, false)
	return err
}

// CreateNotebook posts a raw notebook body ({"data":{...}}) and returns the
// new notebook's id and URL.
func (c *Client) CreateNotebook(ctx context.Context, body []byte) (int64, string, error) {
	out, err := c.do(ctx, "POST", "/api/v1/notebooks", body, false, true)
	if err != nil {
		return 0, "", err
	}
	var resp struct {
		Data struct {
			ID int64 `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return 0, "", fmt.Errorf("decode notebook response: %w", err)
	}
	return resp.Data.ID, fmt.Sprintf("%s/notebook/%d", AppURLFor(c.Site, os.Getenv("DD_SUBDOMAIN"), os.Getenv("DD_APP_URL")), resp.Data.ID), nil
}

// NotebookID extracts the numeric id from a notebook URL or id string.
func NotebookID(urlOrID string) string {
	u := strings.TrimRight(urlOrID, "/")
	if i := strings.LastIndex(u, "/"); i >= 0 {
		u = u[i+1:]
	}
	return u
}

// PrependNotebookCell adds a markdown cell at the top of an existing
// notebook (GET, then PUT with the existing cells referenced by id).
func (c *Client) PrependNotebookCell(ctx context.Context, id string, text string) error {
	out, err := c.do(ctx, "GET", "/api/v1/notebooks/"+id, nil, false, true)
	if err != nil {
		return err
	}
	var nb struct {
		Data struct {
			Attributes map[string]json.RawMessage `json:"attributes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out, &nb); err != nil {
		return fmt.Errorf("decode notebook: %w", err)
	}
	var cells []json.RawMessage
	if raw, ok := nb.Data.Attributes["cells"]; ok {
		if err := json.Unmarshal(raw, &cells); err != nil {
			return fmt.Errorf("decode cells: %w", err)
		}
	}
	first, _ := json.Marshal(map[string]any{"type": "notebook_cells", "attributes": map[string]any{
		"definition": map[string]any{"type": "markdown", "text": text},
	}})
	attrs := map[string]any{"cells": append([]json.RawMessage{first}, cells...)}
	for _, k := range []string{"name", "time", "status", "metadata"} {
		if v, ok := nb.Data.Attributes[k]; ok {
			attrs[k] = v
		}
	}
	body, err := json.Marshal(map[string]any{"data": map[string]any{"type": "notebooks", "attributes": attrs}})
	if err != nil {
		return err
	}
	_, err = c.do(ctx, "PUT", "/api/v1/notebooks/"+id, body, false, true)
	return err
}
