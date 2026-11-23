package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/otelping/otelping-probe/internal/checker"
	"github.com/otelping/otelping-probe/internal/config"
	"github.com/otelping/otelping-probe/internal/otlp"
)

var version = "1.0.0"

func main() {
	var (
		configPath  = flag.String("config", defaultConfigPath(), "Path to otelping.yaml or otelping.json (optional if OTELPING_TARGETS is set)")
		runOnce     = flag.Bool("once", false, "Run all configured checks once, push OTLP metrics, and exit")
		dryRun      = flag.Bool("dry-run", false, "Run checks and print results without pushing to the OTLP endpoint")
		showVersion = flag.Bool("version", false, "Print version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Printf("otelping-probe v%s (https://github.com/otelping/otelping-probe)\n", version)
		return
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("[otelping-probe] configuration error: %v", err)
	}
	if !*dryRun && cfg.APIKey == "" {
		log.Printf("[otelping-probe] warning: OTELPING_API_KEY is empty (set api_key or OTELPING_API_KEY for https://app.otelping.eu)")
	}

	exporter := otlp.NewExporter(cfg.Endpoint, cfg.APIKey, cfg.Region, cfg.Headers)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *runOnce {
		os.Exit(executeOnce(ctx, cfg, exporter, *dryRun))
	}

	log.Printf("[otelping-probe] v%s started (region=%s, checks=%d, endpoint=%s)", version, cfg.Region, len(cfg.Checks), cfg.Endpoint)

	resultsCh := make(chan checker.Result, len(cfg.Checks)*4+16)
	var wg sync.WaitGroup

	for idx, chk := range cfg.Checks {
		c := chk
		stagger := time.Duration(idx*150) * time.Millisecond
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case <-ctx.Done():
				return
			case <-time.After(stagger):
			}
			runAndQueue := func() {
				res := checker.Run(ctx, c)
				if res.Passed {
					log.Printf("[OK]   %-28s %d  %6.1fms", res.Name, res.StatusCode, res.TotalMS)
				} else {
					log.Printf("[FAIL] %-28s %d  %6.1fms  (%s)", res.Name, res.StatusCode, res.TotalMS, res.ErrorMessage)
				}
				select {
				case resultsCh <- res:
				default:
				}
			}
			runAndQueue()
			ticker := time.NewTicker(c.Interval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					runAndQueue()
				}
			}
		}()
	}

	// Flush batched OTLP metrics every 2s or whenever 25 results accumulate so 10s checks arrive promptly with minimal HTTP overhead
	flushTicker := time.NewTicker(2 * time.Second)
	defer flushTicker.Stop()
	var batch []checker.Result

	flush := func() {
		if len(batch) == 0 {
			return
		}
		toSend := make([]checker.Result, len(batch))
		copy(toSend, batch)
		batch = batch[:0]
		if *dryRun {
			return
		}
		fctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()
		if exErr := exporter.Export(fctx, toSend); exErr != nil {
			log.Printf("[OTLP] push warning (%d check(s)): %v", len(toSend), exErr)
		}
	}

	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			for {
				select {
				case r := <-resultsCh:
					batch = append(batch, r)
				default:
					flush()
					log.Printf("[otelping-probe] stopped cleanly")
					return
				}
			}
		case r := <-resultsCh:
			batch = append(batch, r)
			if len(batch) >= 25 {
				flush()
			}
		case <-flushTicker.C:
			flush()
		}
	}
}

func defaultConfigPath() string {
	if env := os.Getenv("OTELPING_CONFIG"); env != "" {
		return env
	}
	if _, err := os.Stat("otelping.yaml"); err == nil {
		return "otelping.yaml"
	}
	return ""
}

func executeOnce(ctx context.Context, cfg *config.Config, exporter *otlp.Exporter, dryRun bool) int {
	results := make([]checker.Result, len(cfg.Checks))
	var wg sync.WaitGroup
	for i, chk := range cfg.Checks {
		i, chk := i, chk
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = checker.Run(ctx, chk)
		}()
	}
	wg.Wait()

	failed := 0
	for _, r := range results {
		state := "OK"
		if !r.Passed {
			state = "FAIL"
			failed++
		}
		extra := r.ErrorMessage
		if r.Passed && r.CertDaysLeft >= 0 {
			extra = fmt.Sprintf("TLS valid (%dd left)", r.CertDaysLeft)
		}
		fmt.Printf("%-5s %-30s kind=%-4s status=%-3d total=%6.1fms  %s\n", state, r.Name, r.Kind, r.StatusCode, r.TotalMS, extra)
	}

	if !dryRun {
		if err := exporter.Export(ctx, results); err != nil {
			log.Printf("[OTLP] export error: %v", err)
			return 1
		}
		fmt.Printf("Pushed %d check result(s) via OTLP/HTTP to %s\n", len(results), cfg.Endpoint)
	}
	if failed > 0 {
		return 1
	}
	return 0
}
