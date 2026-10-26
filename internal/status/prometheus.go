package status

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Sample is one series from an instant query: its labels and current value.
type Sample struct {
	Labels map[string]string
	Value  float64
}

// Prometheus runs instant queries against the Prometheus HTTP API. It only needs
// GET /api/v1/query, so it uses the standard library instead of a client package.
type Prometheus struct {
	base   *url.URL
	client *http.Client
}

// NewPrometheus returns a client for the Prometheus server at baseURL
// (e.g. http://prometheus:9090).
func NewPrometheus(baseURL string, client *http.Client) (*Prometheus, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("parse Prometheus URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("prometheus URL must be http or https, got %q", baseURL)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("prometheus URL has no host: %q", baseURL)
	}
	return &Prometheus{base: u, client: client}, nil
}

// queryResponse is the subset of the API's JSON response this client reads.
type queryResponse struct {
	Status string `json:"status"`
	Error  string `json:"error"`
	Data   struct {
		ResultType string          `json:"resultType"`
		Result     json.RawMessage `json:"result"`
	} `json:"data"`
}

// vectorSample is one element of a "vector" result: labels plus [timestamp, "value"].
type vectorSample struct {
	Metric map[string]string `json:"metric"`
	Value  [2]any            `json:"value"`
}

// Query runs an instant query and returns every series in the result.
func (p *Prometheus) Query(ctx context.Context, expr string) ([]Sample, error) {
	u := p.base.JoinPath("api", "v1", "query")
	u.RawQuery = url.Values{"query": {expr}}.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	// Cap what we read: a misbehaving server can't make us buffer gigabytes
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}

	var qr queryResponse
	if err := json.Unmarshal(body, &qr); err != nil {
		return nil, fmt.Errorf("decode response (HTTP %d): %w", resp.StatusCode, err)
	}
	if qr.Status != "success" {
		return nil, fmt.Errorf("query failed (HTTP %d): %s", resp.StatusCode, qr.Error)
	}

	switch qr.Data.ResultType {
	case "vector":
		var raw []vectorSample
		if err := json.Unmarshal(qr.Data.Result, &raw); err != nil {
			return nil, fmt.Errorf("decode vector: %w", err)
		}
		samples := make([]Sample, 0, len(raw))
		for _, r := range raw {
			v, err := parseValue(r.Value[1])
			if err != nil {
				return nil, err
			}
			samples = append(samples, Sample{Labels: r.Metric, Value: v})
		}
		return samples, nil
	case "scalar":
		var raw [2]any
		if err := json.Unmarshal(qr.Data.Result, &raw); err != nil {
			return nil, fmt.Errorf("decode scalar: %w", err)
		}
		v, err := parseValue(raw[1])
		if err != nil {
			return nil, err
		}
		return []Sample{{Value: v}}, nil
	default:
		return nil, fmt.Errorf("unsupported result type %q", qr.Data.ResultType)
	}
}

// QueryValue runs a query expected to return exactly one number.
func (p *Prometheus) QueryValue(ctx context.Context, expr string) (float64, error) {
	samples, err := p.Query(ctx, expr)
	if err != nil {
		return 0, err
	}
	switch len(samples) {
	case 0:
		return 0, errors.New("query returned no data")
	case 1:
		return samples[0].Value, nil
	default:
		return 0, fmt.Errorf("query returned %d series, want 1", len(samples))
	}
}

// Prometheus encodes sample values as strings ("1", "0.5", "NaN", "+Inf").
func parseValue(v any) (float64, error) {
	s, ok := v.(string)
	if !ok {
		return 0, fmt.Errorf("sample value is %T, want string", v)
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, fmt.Errorf("parse sample value %q: %w", s, err)
	}
	return f, nil
}
