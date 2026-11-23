package otlp_test

import (
	"compress/gzip"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/otelping/otelping-probe/internal/checker"
	"github.com/otelping/otelping-probe/internal/config"
	"github.com/otelping/otelping-probe/internal/otlp"
)

func TestEndToEndCheckersYAMLConfigAndOTLPExport(t *testing.T) {
	ctx := context.Background()

	// 1. Local internal HTTPS server with valid 45-day self-signed cert tested via tls_ca_file
	priv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(42),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-1 * time.Hour),
		NotAfter:     time.Now().Add(45 * 24 * time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	derBytes, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}
	caPath := filepath.Join(t.TempDir(), "internal-ca.pem")
	pemCert := tlsCertPEM(derBytes)
	if err := os.WriteFile(caPath, pemCert, 0o600); err != nil {
		t.Fatalf("write CA pem: %v", err)
	}

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("traceparent") == "" {
			t.Errorf("expected W3C traceparent header on synthetic HTTP request")
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"healthy","db":"connected"}`))
	}))
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{{
			Certificate: [][]byte{derBytes},
			PrivateKey:  priv,
		}},
	}
	srv.StartTLS()
	defer srv.Close()

	// 2. Local TCP listener (simulating internal Postgres/Redis port)
	tcpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	defer tcpLn.Close()
	go func() {
		for {
			c, err := tcpLn.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()

	// 3. Mock OTLP/HTTP collector endpoint verifying gzip + Bearer header + OTLP JSON schema
	var gotAuth, gotEncoding string
	var gotBody []byte
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/metrics" {
			http.NotFound(w, r)
			return
		}
		gotAuth = r.Header.Get("Authorization")
		gotEncoding = r.Header.Get("Content-Encoding")
		gz, err := gzip.NewReader(r.Body)
		if err != nil {
			http.Error(w, "bad gzip", http.StatusBadRequest)
			return
		}
		defer gz.Close()
		gotBody, _ = io.ReadAll(gz)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer collector.Close()

	// 4. Write YAML config with ${ENV} expansion and load it
	t.Setenv("TEST_OTELPING_KEY", "otp_live_test_secret_123")
	yamlFile := filepath.Join(t.TempDir(), "otelping.yaml")
	yamlContent := `endpoint: "` + collector.URL + `"
api_key: "${TEST_OTELPING_KEY}"
region: "k8s-eu-west-internal"
interval: "10s"

checks:
  - name: "Internal Billing API"
    group: "Core-Services"
    kind: "http"
    target: "` + srv.URL + `/healthz"
    keyword: "healthy"
    tls_ca_file: "` + caPath + `"
  - name: "Internal Postgres Primary"
    group: "Core-Services"
    kind: "tcp"
    target: "tcp://` + tcpLn.Addr().String() + `"
`
	if err := os.WriteFile(yamlFile, []byte(yamlContent), 0o600); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	cfg, err := config.Load(yamlFile)
	if err != nil {
		t.Fatalf("config.Load failed: %v", err)
	}
	if cfg.APIKey != "otp_live_test_secret_123" || len(cfg.Checks) != 2 {
		t.Fatalf("unexpected parsed config: %+v", cfg)
	}

	var results []checker.Result
	for _, c := range cfg.Checks {
		r := checker.Run(ctx, c)
		if !r.Passed {
			t.Fatalf("expected check %q to pass, got err=%q status=%d", c.Name, r.ErrorMessage, r.StatusCode)
		}
		results = append(results, r)
	}
	if results[0].CertDaysLeft < 43 || results[0].CertDaysLeft > 45 {
		t.Fatalf("expected ~44 TLS cert days remaining, got %d", results[0].CertDaysLeft)
	}

	exp := otlp.NewExporter(cfg.Endpoint, cfg.APIKey, cfg.Region, cfg.Headers)
	if err := exp.Export(ctx, results); err != nil {
		t.Fatalf("Export failed: %v", err)
	}
	if gotAuth != "Bearer otp_live_test_secret_123" || gotEncoding != "gzip" {
		t.Fatalf("unexpected OTLP headers: auth=%q encoding=%q", gotAuth, gotEncoding)
	}
	var parsed map[string]any
	if err := json.Unmarshal(gotBody, &parsed); err != nil {
		t.Fatalf("invalid OTLP JSON payload: %v", err)
	}
	bodyStr := string(gotBody)
	for _, want := range []string{"Internal Billing API", "Internal Postgres Primary", "Core-Services", "k8s-eu-west-internal", "tls.cert.days_remaining", "check.passed"} {
		if !strings.Contains(bodyStr, want) {
			t.Fatalf("expected %q in exported OTLP JSON: %s", want, bodyStr)
		}
	}
}

func tlsCertPEM(der []byte) []byte {
	const lineLen = 64
	b64 := base64EncodeStd(der)
	var sb strings.Builder
	sb.WriteString("-----BEGIN CERTIFICATE-----\n")
	for i := 0; i < len(b64); i += lineLen {
		end := i + lineLen
		if end > len(b64) {
			end = len(b64)
		}
		sb.WriteString(b64[i:end] + "\n")
	}
	sb.WriteString("-----END CERTIFICATE-----\n")
	return []byte(sb.String())
}

func base64EncodeStd(src []byte) string {
	const encodeStd = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	var out strings.Builder
	for i := 0; i < len(src); i += 3 {
		rem := len(src) - i
		var b0, b1, b2 byte
		b0 = src[i]
		if rem > 1 {
			b1 = src[i+1]
		}
		if rem > 2 {
			b2 = src[i+2]
		}
		out.WriteByte(encodeStd[b0>>2])
		out.WriteByte(encodeStd[((b0&0x03)<<4)|(b1>>4)])
		if rem > 1 {
			out.WriteByte(encodeStd[((b1&0x0f)<<2)|(b2>>6)])
		} else {
			out.WriteByte('=')
		}
		if rem > 2 {
			out.WriteByte(encodeStd[b2&0x3f])
		} else {
			out.WriteByte('=')
		}
	}
	return out.String()
}
