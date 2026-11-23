package otlp

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/otelping/otelping-probe/internal/checker"
)

type Exporter struct {
	endpoint   string
	apiKey     string
	region     string
	headers    map[string]string
	httpClient *http.Client
}

func NewExporter(endpoint, apiKey, region string, extraHeaders map[string]string) *Exporter {
	ep := strings.TrimRight(strings.TrimSpace(endpoint), "/")
	if !strings.HasSuffix(ep, "/v1/metrics") {
		ep += "/v1/metrics"
	}
	return &Exporter{
		endpoint: ep,
		apiKey:   strings.TrimSpace(apiKey),
		region:   strings.TrimSpace(region),
		headers:  extraHeaders,
		httpClient: &http.Client{
			Timeout: 12 * time.Second,
		},
	}
}

// OTLP/HTTP ProtoJSON wire structs (uint64/fixed64 timestamps serialized as decimal strings per OTLP JSON specification).
type exportMetricsRequest struct {
	ResourceMetrics []resourceMetrics `json:"resourceMetrics"`
}

type resourceMetrics struct {
	Resource     otlpResource   `json:"resource"`
	ScopeMetrics []scopeMetrics `json:"scopeMetrics"`
}

type otlpResource struct {
	Attributes []keyValue `json:"attributes"`
}

type scopeMetrics struct {
	Scope   instrumentationScope `json:"scope"`
	Metrics []metricItem         `json:"metrics"`
}

type instrumentationScope struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type metricItem struct {
	Name  string    `json:"name"`
	Unit  string    `json:"unit,omitempty"`
	Gauge *gaugeSet `json:"gauge,omitempty"`
}

type gaugeSet struct {
	DataPoints []numberDataPoint `json:"dataPoints"`
}

type numberDataPoint struct {
	Attributes   []keyValue `json:"attributes,omitempty"`
	TimeUnixNano string     `json:"timeUnixNano"`
	AsDouble     *float64   `json:"asDouble,omitempty"`
	AsInt        *string    `json:"asInt,omitempty"`
}

type keyValue struct {
	Key   string   `json:"key"`
	Value anyValue `json:"value"`
}

type anyValue struct {
	StringValue string `json:"stringValue,omitempty"`
}

func strAttr(k, v string) keyValue {
	return keyValue{Key: k, Value: anyValue{StringValue: v}}
}

func gaugeDouble(name, unit string, tsNano string, val float64) metricItem {
	v := val
	return metricItem{
		Name: name,
		Unit: unit,
		Gauge: &gaugeSet{
			DataPoints: []numberDataPoint{{
				TimeUnixNano: tsNano,
				AsDouble:     &v,
			}},
		},
	}
}

func gaugeInt(name, unit string, tsNano string, val int) metricItem {
	s := strconv.Itoa(val)
	return metricItem{
		Name: name,
		Unit: unit,
		Gauge: &gaugeSet{
			DataPoints: []numberDataPoint{{
				TimeUnixNano: tsNano,
				AsInt:        &s,
			}},
		},
	}
}

// BuildPayload encodes a slice of check results into a spec-compliant OTLP/HTTP ExportMetricsServiceRequest JSON payload.
func (e *Exporter) BuildPayload(results []checker.Result) ([]byte, error) {
	rms := make([]resourceMetrics, 0, len(results))
	for _, r := range results {
		ts := r.CheckedAt
		if ts.IsZero() {
			ts = time.Now().UTC()
		}
		tsNano := strconv.FormatInt(ts.UnixNano(), 10)

		attrs := []keyValue{
			strAttr("service.name", r.Name),
			strAttr("probe.region", e.region),
			strAttr("check.kind", r.Kind),
			strAttr("url.full", r.Target),
		}
		if r.Group != "" {
			attrs = append(attrs, strAttr("service.group", r.Group))
		}
		if r.TraceID != "" {
			attrs = append(attrs, strAttr("trace.id", r.TraceID))
		}
		if r.ErrorMessage != "" {
			attrs = append(attrs, strAttr("error.message", r.ErrorMessage))
		}

		upVal := 0
		if r.Passed {
			upVal = 1
		}
		metrics := []metricItem{
			gaugeInt("check.passed", "1", tsNano, upVal),
			gaugeDouble("http.client.duration", "ms", tsNano, r.TotalMS),
		}
		if r.StatusCode > 0 {
			metrics = append(metrics, gaugeInt("http.response.status_code", "1", tsNano, r.StatusCode))
		}
		if r.DNSMS > 0 {
			metrics = append(metrics, gaugeDouble("net.dns.duration", "ms", tsNano, r.DNSMS))
		}
		if r.TLSMS > 0 {
			metrics = append(metrics, gaugeDouble("net.tls.duration", "ms", tsNano, r.TLSMS))
		}
		if r.TTFBMS > 0 {
			metrics = append(metrics, gaugeDouble("http.ttfb.duration", "ms", tsNano, r.TTFBMS))
		}
		if r.CertDaysLeft >= 0 {
			metrics = append(metrics, gaugeInt("tls.cert.days_remaining", "d", tsNano, r.CertDaysLeft))
		}

		rms = append(rms, resourceMetrics{
			Resource: otlpResource{Attributes: attrs},
			ScopeMetrics: []scopeMetrics{{
				Scope: instrumentationScope{
					Name:    "github.com/otelping/otelping-probe",
					Version: "1.0.0",
				},
				Metrics: metrics,
			}},
		})
	}
	return json.Marshal(exportMetricsRequest{ResourceMetrics: rms})
}

// Export sends the gzip-compressed OTLP/HTTP JSON metrics batch with retry backoff on 429/5xx.
func (e *Exporter) Export(ctx context.Context, results []checker.Result) error {
	if len(results) == 0 {
		return nil
	}
	rawJSON, err := e.BuildPayload(results)
	if err != nil {
		return err
	}

	var gzBuf bytes.Buffer
	gz := gzip.NewWriter(&gzBuf)
	if _, err := gz.Write(rawJSON); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	compressed := gzBuf.Bytes()

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt*600) * time.Millisecond):
			}
		}

		req, rErr := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint, bytes.NewReader(compressed))
		if rErr != nil {
			return rErr
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Content-Encoding", "gzip")
		req.Header.Set("User-Agent", "OTelPing-Probe/1.0 (+https://github.com/otelping/otelping-probe)")
		if e.apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+e.apiKey)
			req.Header.Set("X-API-Key", e.apiKey)
		}
		for k, v := range e.headers {
			req.Header.Set(k, v)
		}

		resp, doErr := e.httpClient.Do(req)
		if doErr != nil {
			lastErr = doErr
			continue
		}
		bodySnippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil
		}
		lastErr = fmt.Errorf("OTLP export HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(bodySnippet)))
		if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode < 500 {
			return lastErr
		}
	}
	return lastErr
}
