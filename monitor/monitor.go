// SPDX-FileCopyrightText: 2025 Gthulhu Team
//
// SPDX-License-Identifier: Apache-2.0

// Package monitor provides the pod-level scheduling metrics collector.
//
// It loads an eBPF program (sched_monitor.bpf.o) that hooks into
// tp_btf/sched_switch and tp_btf/sched_process_exit tracepoints,
// reads per-PID scheduling metrics from BPF maps, aggregates them
// by pod, and exposes the results as Prometheus metrics.
//
// This is the BASE feature of Gthulhu — works on Linux 5.2+ (BTF-enabled
// kernels) and does NOT require sched_ext.
//
// Architecture:
//
// PodSchedulingMetrics CRD  →  CRD Watcher  →  eBPF Collector (tp_btf)
//
//	         ↓
//	  Prometheus /metrics
//	         ↓
//	Prometheus Adapter → KEDA → HPA
package monitor

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/Gthulhu/Gthulhu/monitor/collector"
	"github.com/Gthulhu/Gthulhu/monitor/crdwatcher"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Config holds the monitor configuration passed from the main scheduler.
type Config struct {
	BPFObjectPath         string
	CollectionIntervalSec int
	MonitorAll            bool
	StreamEvents          bool
	PrometheusPort        int
	NodeName              string
	EnableCRDWatcher      bool
	KubeConfigPath        string
}

// StartMonitor loads the eBPF monitor, starts the collector poll loop and
// Prometheus HTTP server. It blocks until ctx is cancelled.
func StartMonitor(ctx context.Context, cfg Config, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	logger = logger.With("component", "monitor")

	// Pod mapper: resolves PIDs → Kubernetes pods via /proc/<pid>/cgroup
	podMapper := collector.NewPodMapper(cfg.NodeName, logger)
	done := make(chan struct{})
	defer close(done)
	podMapper.StartPeriodicScan(30*time.Second, done)

	if kubeConfig, err := buildKubeConfig(cfg.KubeConfigPath); err != nil {
		logger.Warn("kubeconfig unavailable, pod index disabled", "error", err)
	} else {
		if err := startPodIndexRefresher(ctx, kubeConfig, podMapper, cfg.NodeName, logger); err != nil {
			logger.Warn("pod index refresher disabled", "error", err)
		}
	}

	// eBPF Collector
	interval := cfg.CollectionIntervalSec
	if interval <= 0 {
		interval = 10
	}
	col := collector.New(collector.Config{
		BPFObjectPath: cfg.BPFObjectPath,
		PollInterval:  time.Duration(interval) * time.Second,
		MonitorAll:    cfg.MonitorAll,
		StreamEvents:  cfg.StreamEvents,
	}, podMapper, logger)

	// CRD Watcher (optional — needs Kubernetes API access)
	if cfg.EnableCRDWatcher {
		kubeConfig, err := buildKubeConfig(cfg.KubeConfigPath)
		if err != nil {
			logger.Warn("kubeconfig unavailable, CRD watcher disabled", "error", err)
		} else {
			w, err := crdwatcher.New(kubeConfig, col, podMapper, cfg.NodeName, logger)
			if err != nil {
				logger.Warn("CRD watcher creation failed", "error", err)
			} else {
				go func() {
					if err := w.Run(ctx); err != nil {
						logger.Error("CRD watcher error", "error", err)
					}
				}()
				logger.Info("CRD watcher started for PodSchedulingMetrics")
			}
		}
	}

	// Register Prometheus collector with a dedicated registry to avoid
	// polluting the default global registry used by other components.
	reg := prometheus.NewRegistry()
	reg.MustRegister(collector.NewPodSchedMetricsCollector(col))
	reg.MustRegister(prometheus.NewGoCollector())
	reg.MustRegister(prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))

	// Prometheus HTTP server
	port := cfg.PrometheusPort
	if port == 0 {
		port = 9090
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "ok")
	})
	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: mux,
	}

	// Start HTTP server in background
	go func() {
		logger.Info("Prometheus metrics server starting", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("metrics server error", "error", err)
		}
	}()

	// Start collector — blocks until ctx is cancelled
	err := col.Start(ctx)

	// Graceful shutdown of HTTP server
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	_ = srv.Shutdown(shutdownCtx)

	logger.Info("monitor stopped")
	return err
}

func startPodIndexRefresher(ctx context.Context, kubeConfig *rest.Config, podMapper *collector.PodMapper, nodeName string, logger *slog.Logger) error {
	if nodeName == "" {
		return fmt.Errorf("NODE_NAME is empty")
	}
	client, err := kubernetes.NewForConfig(kubeConfig)
	if err != nil {
		return fmt.Errorf("kubernetes client: %w", err)
	}

	refresh := func() {
		pods, err := client.CoreV1().Pods("").List(ctx, metav1.ListOptions{
			FieldSelector: "spec.nodeName=" + nodeName,
		})
		if err != nil {
			logger.Warn("failed to refresh pod index", "node", nodeName, "error", err)
			return
		}
		index := make(map[string]*collector.PodRef, len(pods.Items))
		for i := range pods.Items {
			pod := &pods.Items[i]
			index[string(pod.UID)] = &collector.PodRef{
				PodName:   pod.Name,
				PodUID:    string(pod.UID),
				Namespace: pod.Namespace,
				NodeName:  pod.Spec.NodeName,
			}
		}
		podMapper.SetPodIndex(index)
		logger.Info("pod index refreshed", "node", nodeName, "pods", len(index))
	}

	refresh()
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				refresh()
			}
		}
	}()
	return nil
}

// buildKubeConfig returns a Kubernetes rest.Config from a kubeconfig path
// or falls back to in-cluster config when running inside a pod.
func buildKubeConfig(kubeconfigPath string) (*rest.Config, error) {
	if kubeconfigPath != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	}
	return rest.InClusterConfig()
}
