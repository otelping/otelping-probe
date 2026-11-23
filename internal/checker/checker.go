package checker

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/otelping/otelping-probe/internal/config"
)

type Result struct {
	Name          string
	Group         string
	Kind          string
	Target        string
	TraceID       string
	Passed        bool
	StatusCode    int
	TotalMS       float64
	DNSMS         float64
	TCPMS         float64
	TLSMS         float64
	TTFBMS        float64
	CertDaysLeft  int
	ErrorMessage  string
	CheckedAt     time.Time
}

func newW3CTraceParent() (traceID, header string) {
	var raw [24]byte
	_, _ = rand.Read(raw[:])
	// Ensure non-zero trace and span IDs per W3C Trace Context spec
	raw[0] |= 0x01
	raw[16] |= 0x01
	tID := hex.EncodeToString(raw[0:16])
	sID := hex.EncodeToString(raw[16:24])
	return tID, "00-" + tID + "-" + sID + "-01"
}

func buildTLSConfig(c config.CheckConfig, serverName string) (*tls.Config, error) {
	tlsCfg := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		ServerName:         serverName,
		InsecureSkipVerify: c.TLSSkipVerify, // explicit opt-in only for internal self-signed endpoints
	}
	if strings.TrimSpace(c.TLSCAFile) != "" {
		pemBytes, err := os.ReadFile(c.TLSCAFile)
		if err != nil {
			return nil, fmt.Errorf("read tls_ca_file: %w", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pemBytes) {
			return nil, fmt.Errorf("no valid PEM certificates in %s", c.TLSCAFile)
		}
		tlsCfg.RootCAs = pool
	}
	return tlsCfg, nil
}

func Run(ctx context.Context, c config.CheckConfig) Result {
	cctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()

	switch c.Kind {
	case "tcp":
		return runTCP(cctx, c)
	case "dns":
		return runDNS(cctx, c)
	default:
		return runHTTP(cctx, c)
	}
}

func runHTTP(ctx context.Context, c config.CheckConfig) Result {
	traceID, traceparent := newW3CTraceParent()
	res := Result{
		Name:         c.Name,
		Group:        c.Group,
		Kind:         "http",
		Target:       c.Target,
		TraceID:      traceID,
		CertDaysLeft: -1,
		CheckedAt:    time.Now().UTC(),
	}

	targetURL := c.Target
	if !strings.Contains(targetURL, "://") {
		targetURL = "https://" + targetURL
	}
	parsed, err := url.Parse(targetURL)
	if err != nil || parsed.Host == "" {
		res.ErrorMessage = "invalid target URL"
		return res
	}

	tlsCfg, err := buildTLSConfig(c, parsed.Hostname())
	if err != nil {
		res.ErrorMessage = err.Error()
		return res
	}

	var dnsStart, dnsDone, tcpStart, tcpDone, tlsStart, tlsDone, gotFirstByte time.Time
	var peerCertExpiry time.Time

	tr := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		TLSClientConfig:     tlsCfg,
		DisableKeepAlives:   true,
		TLSHandshakeTimeout: c.Timeout,
		DialContext: (&net.Dialer{
			Timeout: c.Timeout,
		}).DialContext,
	}
	defer tr.CloseIdleConnections()

	client := &http.Client{
		Transport: tr,
		Timeout:   c.Timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("stopped after 5 redirects")
			}
			return nil
		},
	}

	ht := &httptrace.ClientTrace{
		DNSStart: func(_ httptrace.DNSStartInfo) { dnsStart = time.Now() },
		DNSDone:  func(_ httptrace.DNSDoneInfo) { dnsDone = time.Now() },
		ConnectStart: func(_, _ string) {
			if tcpStart.IsZero() {
				tcpStart = time.Now()
			}
		},
		ConnectDone: func(_, _ string, err error) {
			if err == nil && tcpDone.IsZero() {
				tcpDone = time.Now()
			}
		},
		TLSHandshakeStart: func() { tlsStart = time.Now() },
		TLSHandshakeDone: func(state tls.ConnectionState, err error) {
			tlsDone = time.Now()
			if err == nil && len(state.PeerCertificates) > 0 {
				peerCertExpiry = state.PeerCertificates[0].NotAfter
			}
		},
		GotFirstResponseByte: func() { gotFirstByte = time.Now() },
	}

	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, ht), c.Method, targetURL, nil)
	if err != nil {
		res.ErrorMessage = err.Error()
		return res
	}
	req.Header.Set("User-Agent", "OTelPing-Probe/1.0 (+https://github.com/otelping/otelping-probe)")
	req.Header.Set("traceparent", traceparent)
	for k, v := range c.Headers {
		req.Header.Set(k, v)
	}

	start := time.Now()
	resp, err := client.Do(req)
	totalDur := time.Since(start)
	res.TotalMS = durMS(totalDur)
	if !dnsStart.IsZero() && !dnsDone.IsZero() {
		res.DNSMS = durMS(dnsDone.Sub(dnsStart))
	}
	if !tcpStart.IsZero() && !tcpDone.IsZero() {
		res.TCPMS = durMS(tcpDone.Sub(tcpStart))
	}
	if !tlsStart.IsZero() && !tlsDone.IsZero() {
		res.TLSMS = durMS(tlsDone.Sub(tlsStart))
	}
	if !gotFirstByte.IsZero() {
		res.TTFBMS = durMS(gotFirstByte.Sub(start))
	}
	if !peerCertExpiry.IsZero() {
		days := int(math.Floor(time.Until(peerCertExpiry).Hours() / 24.0))
		if days < 0 {
			days = 0
		}
		res.CertDaysLeft = days
	}

	if err != nil {
		res.ErrorMessage = compactErr(err)
		return res
	}
	defer resp.Body.Close()

	res.StatusCode = resp.StatusCode
	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	totalDur = time.Since(start)
	res.TotalMS = durMS(totalDur)

	statusOK := resp.StatusCode >= 200 && resp.StatusCode < 400
	if c.ExpectStatus > 0 {
		statusOK = resp.StatusCode == c.ExpectStatus
	}
	if !statusOK {
		res.ErrorMessage = fmt.Sprintf("HTTP %d", resp.StatusCode)
		return res
	}
	if c.Keyword != "" && !strings.Contains(string(bodyBytes), c.Keyword) {
		res.ErrorMessage = fmt.Sprintf("missing expected keyword %q", c.Keyword)
		return res
	}

	res.Passed = true
	return res
}

func runTCP(ctx context.Context, c config.CheckConfig) Result {
	traceID, _ := newW3CTraceParent()
	res := Result{
		Name:         c.Name,
		Group:        c.Group,
		Kind:         "tcp",
		Target:       c.Target,
		TraceID:      traceID,
		CertDaysLeft: -1,
		CheckedAt:    time.Now().UTC(),
	}

	raw := strings.TrimSpace(c.Target)
	useTLS := strings.HasPrefix(strings.ToLower(raw), "tls://")
	for _, pfx := range []string{"tcp://", "tls://"} {
		if strings.HasPrefix(strings.ToLower(raw), pfx) {
			raw = raw[len(pfx):]
			break
		}
	}
	host, _, splitErr := net.SplitHostPort(raw)
	if splitErr != nil {
		res.ErrorMessage = fmt.Sprintf("invalid host:port %q", raw)
		return res
	}

	start := time.Now()
	d := net.Dialer{Timeout: c.Timeout}
	conn, err := d.DialContext(ctx, "tcp", raw)
	if err != nil {
		res.TotalMS = durMS(time.Since(start))
		res.StatusCode = 503
		res.ErrorMessage = compactErr(err)
		return res
	}
	defer conn.Close()
	res.TCPMS = durMS(time.Since(start))

	if useTLS {
		tlsCfg, cfgErr := buildTLSConfig(c, host)
		if cfgErr != nil {
			res.ErrorMessage = cfgErr.Error()
			return res
		}
		tlsStart := time.Now()
		tlsConn := tls.Client(conn, tlsCfg)
		_ = tlsConn.SetDeadline(time.Now().Add(c.Timeout))
		if hErr := tlsConn.HandshakeContext(ctx); hErr != nil {
			res.TotalMS = durMS(time.Since(start))
			res.StatusCode = 503
			res.ErrorMessage = compactErr(hErr)
			return res
		}
		res.TLSMS = durMS(time.Since(tlsStart))
		st := tlsConn.ConnectionState()
		if len(st.PeerCertificates) > 0 {
			days := int(math.Floor(time.Until(st.PeerCertificates[0].NotAfter).Hours() / 24.0))
			if days < 0 {
				days = 0
			}
			res.CertDaysLeft = days
		}
	}

	res.TotalMS = durMS(time.Since(start))
	res.StatusCode = 200
	res.Passed = true
	return res
}

func runDNS(ctx context.Context, c config.CheckConfig) Result {
	traceID, _ := newW3CTraceParent()
	res := Result{
		Name:         c.Name,
		Group:        c.Group,
		Kind:         "dns",
		Target:       c.Target,
		TraceID:      traceID,
		CertDaysLeft: -1,
		CheckedAt:    time.Now().UTC(),
	}

	domain := strings.TrimSpace(c.Target)
	if strings.HasPrefix(strings.ToLower(domain), "dns://") {
		domain = strings.TrimSpace(domain[6:])
	}
	domain = strings.TrimRight(domain, "/")

	resolver := net.DefaultResolver
	if srv := strings.TrimSpace(c.DNSServer); srv != "" {
		if !strings.Contains(srv, ":") {
			srv = srv + ":53"
		}
		resolver = &net.Resolver{
			PreferGo: true,
			Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
				d := net.Dialer{Timeout: c.Timeout}
				return d.DialContext(ctx, network, srv)
			},
		}
	}

	start := time.Now()
	addrs, err := resolver.LookupHost(ctx, domain)
	elapsed := durMS(time.Since(start))
	res.DNSMS = elapsed
	res.TotalMS = elapsed

	if err != nil || len(addrs) == 0 {
		res.StatusCode = 503
		if err != nil {
			res.ErrorMessage = compactErr(err)
		} else {
			res.ErrorMessage = "no A or AAAA records returned"
		}
		return res
	}

	res.StatusCode = 200
	res.Passed = true
	return res
}

func durMS(d time.Duration) float64 {
	return math.Round(float64(d)/float64(time.Microsecond)) / 1000.0
}

func compactErr(err error) string {
	if err == nil {
		return ""
	}
	s := strings.TrimSpace(err.Error())
	if len(s) > 160 {
		return s[:160]
	}
	return s
}
