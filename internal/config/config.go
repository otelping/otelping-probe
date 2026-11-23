package config

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Endpoint      string            `json:"endpoint"`
	APIKey        string            `json:"api_key"`
	Region        string            `json:"region"`
	DefaultEvery  time.Duration     `json:"-"`
	IntervalStr   string            `json:"interval,omitempty"`
	PushBatchEvery time.Duration    `json:"-"`
	Headers       map[string]string `json:"headers,omitempty"`
	Checks        []CheckConfig     `json:"checks"`
}

type CheckConfig struct {
	Name          string            `json:"name"`
	Group         string            `json:"group,omitempty"`
	Kind          string            `json:"kind,omitempty"` // "http", "tcp", "dns"
	Target        string            `json:"target"`
	Method        string            `json:"method,omitempty"`
	Interval      time.Duration     `json:"-"`
	IntervalStr   string            `json:"interval,omitempty"`
	Timeout       time.Duration     `json:"-"`
	TimeoutStr    string            `json:"timeout,omitempty"`
	ExpectStatus  int               `json:"expect_status,omitempty"`
	Keyword       string            `json:"keyword,omitempty"`
	DNSServer     string            `json:"dns_server,omitempty"`
	TLSCAFile     string            `json:"tls_ca_file,omitempty"`
	TLSSkipVerify bool              `json:"tls_skip_verify,omitempty"`
	Headers       map[string]string `json:"headers,omitempty"`
}

// Load reads an optional YAML or JSON config file and overlays environment variables.
func Load(path string) (*Config, error) {
	cfg := &Config{
		Endpoint:       "https://app.otelping.eu",
		Region:         defaultHostnameRegion(),
		DefaultEvery:   10 * time.Second,
		PushBatchEvery: 10 * time.Second,
		Headers:        make(map[string]string),
	}

	if strings.TrimSpace(path) != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config %s: %w", path, err)
		}
		trimmed := strings.TrimSpace(string(raw))
		if strings.HasPrefix(trimmed, "{") {
			if jErr := json.Unmarshal(raw, cfg); jErr != nil {
				return nil, fmt.Errorf("parse JSON config: %w", jErr)
			}
		} else {
			if yErr := parseSimpleYAML(string(raw), cfg); yErr != nil {
				return nil, fmt.Errorf("parse YAML config: %w", yErr)
			}
		}
	}

	if v := strings.TrimSpace(os.Getenv("OTELPING_ENDPOINT")); v != "" {
		cfg.Endpoint = v
	}
	if v := strings.TrimSpace(os.Getenv("OTELPING_API_KEY")); v != "" {
		cfg.APIKey = v
	}
	if v := strings.TrimSpace(os.Getenv("OTELPING_REGION")); v != "" {
		cfg.Region = v
	}
	if v := strings.TrimSpace(os.Getenv("OTELPING_INTERVAL")); v != "" {
		cfg.IntervalStr = v
	}
	if cfg.IntervalStr != "" {
		d, err := time.ParseDuration(cfg.IntervalStr)
		if err != nil {
			return nil, fmt.Errorf("invalid interval %q: %w", cfg.IntervalStr, err)
		}
		cfg.DefaultEvery = d
	}
	if cfg.DefaultEvery < 5*time.Second {
		cfg.DefaultEvery = 5 * time.Second
	}

	// Zero-file quickstart via OTELPING_TARGETS="Internal-API=https://10.0.1.15:8443/healthz,Postgres=tcp://10.0.1.20:5432"
	if envTargets := strings.TrimSpace(os.Getenv("OTELPING_TARGETS")); envTargets != "" {
		for _, item := range strings.FieldsFunc(envTargets, func(r rune) bool { return r == ',' || r == ';' || r == '\n' }) {
			part := strings.TrimSpace(item)
			if part == "" {
				continue
			}
			name, target := "", part
			if eq := strings.IndexByte(part, '='); eq > 0 {
				name = strings.TrimSpace(part[:eq])
				target = strings.TrimSpace(part[eq+1:])
			}
			cfg.Checks = append(cfg.Checks, CheckConfig{
				Name:   name,
				Target: target,
			})
		}
	}

	cfg.Endpoint = strings.TrimRight(strings.TrimSpace(cfg.Endpoint), "/")
	if cfg.Endpoint == "" {
		return nil, errors.New("endpoint cannot be empty")
	}
	if len(cfg.Checks) == 0 {
		return nil, errors.New("no checks configured (set checks in config file or OTELPING_TARGETS)")
	}

	for i := range cfg.Checks {
		c := &cfg.Checks[i]
		c.Target = strings.TrimSpace(c.Target)
		if c.Target == "" {
			return nil, fmt.Errorf("check #%d has an empty target", i+1)
		}
		if c.Kind == "" {
			c.Kind = inferKind(c.Target)
		} else {
			c.Kind = strings.ToLower(strings.TrimSpace(c.Kind))
		}
		if c.Kind == "https" {
			c.Kind = "http"
		}
		if c.Kind != "http" && c.Kind != "tcp" && c.Kind != "dns" {
			return nil, fmt.Errorf("check %q has unsupported kind %q (use http, tcp, or dns)", c.Name, c.Kind)
		}
		if strings.TrimSpace(c.Name) == "" {
			c.Name = deriveName(c.Target)
		}
		if c.IntervalStr != "" {
			d, err := time.ParseDuration(c.IntervalStr)
			if err != nil {
				return nil, fmt.Errorf("check %q invalid interval %q: %w", c.Name, c.IntervalStr, err)
			}
			c.Interval = d
		}
		if c.Interval <= 0 {
			c.Interval = cfg.DefaultEvery
		}
		if c.Interval < 5*time.Second {
			c.Interval = 5 * time.Second
		}
		if c.TimeoutStr != "" {
			d, err := time.ParseDuration(c.TimeoutStr)
			if err != nil {
				return nil, fmt.Errorf("check %q invalid timeout %q: %w", c.Name, c.TimeoutStr, err)
			}
			c.Timeout = d
		}
		if c.Timeout <= 0 {
			c.Timeout = 8 * time.Second
		}
		if c.Method == "" {
			c.Method = "GET"
		} else {
			c.Method = strings.ToUpper(strings.TrimSpace(c.Method))
		}
	}
	return cfg, nil
}

func defaultHostnameRegion() string {
	h, err := os.Hostname()
	if err != nil || strings.TrimSpace(h) == "" {
		return "internal-lan"
	}
	return "internal-" + strings.ToLower(strings.TrimSpace(h))
}

func inferKind(target string) string {
	lower := strings.ToLower(strings.TrimSpace(target))
	switch {
	case strings.HasPrefix(lower, "tcp://") || strings.HasPrefix(lower, "tls://"):
		return "tcp"
	case strings.HasPrefix(lower, "dns://"):
		return "dns"
	default:
		return "http"
	}
}

func deriveName(target string) string {
	s := strings.TrimSpace(target)
	for _, pfx := range []string{"https://", "http://", "tcp://", "tls://", "dns://"} {
		if strings.HasPrefix(strings.ToLower(s), pfx) {
			s = s[len(pfx):]
			break
		}
	}
	s = strings.TrimRight(s, "/")
	if len(s) > 56 {
		s = s[:56]
	}
	if s == "" {
		return "internal-check"
	}
	return s
}

func unquoteYAML(s string) string {
	s = strings.TrimSpace(s)
	if idx := strings.Index(s, " #"); idx >= 0 && !strings.HasPrefix(s, `"`) && !strings.HasPrefix(s, `'`) {
		s = strings.TrimSpace(s[:idx])
	}
	if len(s) >= 2 && ((s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'')) {
		return s[1 : len(s)-1]
	}
	return s
}

// parseSimpleYAML parses the documented otelping.yaml schema using only the Go standard library.
func parseSimpleYAML(raw string, cfg *Config) error {
	scanner := bufio.NewScanner(strings.NewReader(raw))
	section := ""
	subMap := ""
	var cur *CheckConfig
	lineNo := 0

	flushCheck := func() {
		if cur != nil {
			cfg.Checks = append(cfg.Checks, *cur)
			cur = nil
		}
	}

	for scanner.Scan() {
		lineNo++
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))

		if indent == 0 {
			flushCheck()
			subMap = ""
			k, v, ok := splitKV(trimmed)
			if !ok {
				return fmt.Errorf("line %d: expected key: value", lineNo)
			}
			if v == "" {
				section = k
				continue
			}
			section = ""
			switch k {
			case "endpoint":
				cfg.Endpoint = unquoteYAML(v)
			case "api_key":
				cfg.APIKey = expandEnvRef(unquoteYAML(v))
			case "region":
				cfg.Region = unquoteYAML(v)
			case "interval":
				cfg.IntervalStr = unquoteYAML(v)
			}
			continue
		}

		switch section {
		case "headers":
			k, v, ok := splitKV(trimmed)
			if !ok {
				return fmt.Errorf("line %d: invalid header entry", lineNo)
			}
			cfg.Headers[k] = expandEnvRef(unquoteYAML(v))

		case "checks":
			if strings.HasPrefix(trimmed, "- ") {
				flushCheck()
				subMap = ""
				cur = &CheckConfig{Headers: make(map[string]string)}
				rest := strings.TrimSpace(strings.TrimPrefix(trimmed, "- "))
				if rest != "" {
					k, v, ok := splitKV(rest)
					if !ok {
						return fmt.Errorf("line %d: invalid check field", lineNo)
					}
					if err := assignCheckField(cur, k, v, &subMap); err != nil {
						return fmt.Errorf("line %d: %w", lineNo, err)
					}
				}
				continue
			}
			if cur == nil {
				return fmt.Errorf("line %d: check field outside list item", lineNo)
			}
			if subMap == "headers" && indent >= 6 {
				k, v, ok := splitKV(trimmed)
				if !ok {
					return fmt.Errorf("line %d: invalid check header", lineNo)
				}
				cur.Headers[k] = expandEnvRef(unquoteYAML(v))
				continue
			}
			subMap = ""
			k, v, ok := splitKV(trimmed)
			if !ok {
				return fmt.Errorf("line %d: expected key: value", lineNo)
			}
			if err := assignCheckField(cur, k, v, &subMap); err != nil {
				return fmt.Errorf("line %d: %w", lineNo, err)
			}
		}
	}
	flushCheck()
	return scanner.Err()
}

func splitKV(s string) (string, string, bool) {
	idx := strings.IndexByte(s, ':')
	if idx <= 0 {
		return "", "", false
	}
	return strings.TrimSpace(s[:idx]), strings.TrimSpace(s[idx+1:]), true
}

func expandEnvRef(val string) string {
	if strings.HasPrefix(val, "${") && strings.HasSuffix(val, "}") {
		envName := strings.TrimSpace(val[2 : len(val)-1])
		return os.Getenv(envName)
	}
	return val
}

func assignCheckField(c *CheckConfig, k, rawV string, subMap *string) error {
	v := expandEnvRef(unquoteYAML(rawV))
	switch k {
	case "name":
		c.Name = v
	case "group":
		c.Group = v
	case "kind":
		c.Kind = v
	case "target", "url":
		c.Target = v
	case "method":
		c.Method = v
	case "interval":
		c.IntervalStr = v
	case "timeout":
		c.TimeoutStr = v
	case "keyword":
		c.Keyword = v
	case "dns_server":
		c.DNSServer = v
	case "tls_ca_file":
		c.TLSCAFile = v
	case "tls_skip_verify":
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("invalid boolean for tls_skip_verify: %q", v)
		}
		c.TLSSkipVerify = b
	case "expect_status":
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("invalid integer for expect_status: %q", v)
		}
		c.ExpectStatus = n
	case "headers":
		if v == "" {
			*subMap = "headers"
		}
	}
	return nil
}
