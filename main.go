package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	probing "github.com/prometheus-community/pro-bing"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type Target struct {
	Name string `json:"name"`
	Host string `json:"host"`
}

type Config struct {
	Targets []Target `json:"targets"`
}

var (
	pingLatencyMs = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "ping_latency_ms",
			Help: "Ping latency in milliseconds",
		},
		[]string{"target_name", "target_host"},
	)

	pingPacketLoss = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "ping_packet_loss_percent",
			Help: "Packet loss percentage",
		},
		[]string{"target_name", "target_host"},
	)

	pingOnline = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "ping_online",
			Help: "Target online status (1=online, 0=offline)",
		},
		[]string{"target_name", "target_host"},
	)

	pingPacketsSent = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "ping_packets_sent_total",
			Help: "Total number of ping packets sent",
		},
		[]string{"target_name", "target_host"},
	)

	pingPacketsReceived = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "ping_packets_received_total",
			Help: "Total number of ping packets received",
		},
		[]string{"target_name", "target_host"},
	)

	pingErrors = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "ping_errors_total",
			Help: "Total number of ping errors",
		},
		[]string{"target_name", "target_host"},
	)
)

type PingCollector struct {
	targets          []Target
	pingInterval     time.Duration
	pingTimeout      time.Duration
	pingCount        int
	offlineThreshold int
	mutex            sync.RWMutex
}

func NewPingCollector(targets []Target, pingInterval, pingTimeout time.Duration, pingCount, offlineThreshold int) *PingCollector {
	return &PingCollector{
		targets:          targets,
		pingInterval:     pingInterval,
		pingTimeout:      pingTimeout,
		pingCount:        pingCount,
		offlineThreshold: offlineThreshold,
	}
}

func (pc *PingCollector) Start(ctx context.Context) {
	ticker := time.NewTicker(pc.pingInterval)
	defer ticker.Stop()

	pc.pingAllTargets()

	for {
		select {
		case <-ctx.Done():
			log.Println("Stopping ping collector...")
			return
		case <-ticker.C:
			pc.pingAllTargets()
		}
	}
}

func (pc *PingCollector) pingAllTargets() {
	var wg sync.WaitGroup

	for _, target := range pc.targets {
		wg.Add(1)
		go func(t Target) {
			defer wg.Done()
			pc.pingTarget(t)
		}(target)
	}

	wg.Wait()
}

func (pc *PingCollector) pingTarget(target Target) {
	pc.mutex.Lock()
	defer pc.mutex.Unlock()

	pinger, err := probing.NewPinger(target.Host)
	if err != nil {
		log.Printf("Failed to create pinger for %s (%s): %v", target.Name, target.Host, err)
		pingErrors.WithLabelValues(target.Name, target.Host).Inc()
		pingOnline.WithLabelValues(target.Name, target.Host).Set(0)
		return
	}

	pinger.SetPrivileged(true)
	pinger.Count = pc.pingCount
	pinger.Timeout = pc.pingTimeout
	pinger.Interval = 200 * time.Millisecond

	err = pinger.Run()
	if err != nil {
		log.Printf("Failed to ping %s (%s): %v", target.Name, target.Host, err)
		pingErrors.WithLabelValues(target.Name, target.Host).Inc()
		pingOnline.WithLabelValues(target.Name, target.Host).Set(0)
		return
	}

	stats := pinger.Statistics()

	pingPacketsSent.WithLabelValues(target.Name, target.Host).Add(float64(stats.PacketsSent))
	pingPacketsReceived.WithLabelValues(target.Name, target.Host).Add(float64(stats.PacketsRecv))

	packetLoss := stats.PacketLoss

	pingPacketLoss.WithLabelValues(target.Name, target.Host).Set(packetLoss)

	if stats.PacketsRecv == 0 || packetLoss >= float64(pc.offlineThreshold) {
		pingOnline.WithLabelValues(target.Name, target.Host).Set(0)
		pingLatencyMs.WithLabelValues(target.Name, target.Host).Set(0)
		log.Printf("Target %s (%s) is OFFLINE - packet loss: %.2f%%", target.Name, target.Host, packetLoss)
	} else {
		pingOnline.WithLabelValues(target.Name, target.Host).Set(1)
		latencyMs := float64(stats.AvgRtt.Microseconds()) / 1000.0
		pingLatencyMs.WithLabelValues(target.Name, target.Host).Set(latencyMs)
		log.Printf("Target %s (%s) - latency: %.2fms, packet loss: %.2f%%",
			target.Name, target.Host, latencyMs, packetLoss)
	}
}

func loadConfig(filename string) (*Config, error) {
	file, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var config Config
	if err := json.Unmarshal(file, &config); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	return &config, nil
}

func main() {
	configFile := "targets.json"
	listenAddr := ":9090"
	pingInterval := 30 * time.Second
	pingTimeout := 10 * time.Second
	pingCount := 4
	offlineThreshold := 75

	if addr := os.Getenv("LISTEN_ADDR"); addr != "" {
		listenAddr = addr
	}
	if file := os.Getenv("CONFIG_FILE"); file != "" {
		configFile = file
	}

	log.Printf("Starting ping-exporter...")
	log.Printf("Config file: %s", configFile)
	log.Printf("Listen address: %s", listenAddr)
	log.Printf("Ping interval: %v", pingInterval)
	log.Printf("Ping timeout: %v", pingTimeout)
	log.Printf("Ping count per check: %d", pingCount)
	log.Printf("Offline threshold: %d%% packet loss", offlineThreshold)

	prometheus.MustRegister(pingLatencyMs)
	prometheus.MustRegister(pingPacketLoss)
	prometheus.MustRegister(pingOnline)
	prometheus.MustRegister(pingPacketsSent)
	prometheus.MustRegister(pingPacketsReceived)
	prometheus.MustRegister(pingErrors)

	config, err := loadConfig(configFile)
	if err != nil {
		log.Fatalf("Failed to load config file '%s': %v", configFile, err)
	}

	if len(config.Targets) == 0 {
		log.Fatalf("No targets configured in '%s'", configFile)
	}

	log.Printf("Loaded %d targets from config", len(config.Targets))
	for _, target := range config.Targets {
		log.Printf("  - %s: %s", target.Name, target.Host)
	}

	collector := NewPingCollector(config.Targets, pingInterval, pingTimeout, pingCount, offlineThreshold)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go collector.Start(ctx)

	http.Handle("/metrics", promhttp.Handler())
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, "OK\n")
	})
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = fmt.Fprintf(w, `<html>
<head><title>Ping Exporter</title></head>
<body>
<h1>Ping Exporter</h1>
<p><a href="/metrics">Metrics</a></p>
<p><a href="/health">Health Check</a></p>
<h2>Configured Targets</h2>
<ul>
`)
		for _, target := range config.Targets {
			_, _ = fmt.Fprintf(w, "<li>%s - %s</li>\n", target.Name, target.Host)
		}
		_, _ = fmt.Fprintf(w, `</ul>
</body>
</html>`)
	})

	server := &http.Server{
		Addr:         listenAddr,
		Handler:      http.DefaultServeMux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-sigChan
		log.Println("Received shutdown signal, gracefully shutting down...")
		cancel()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("Server shutdown error: %v", err)
		}
	}()

	log.Printf("Server starting on %s", listenAddr)
	log.Println("Ready to accept requests")
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("Server error: %v", err)
	}

	log.Println("Server stopped")
}
