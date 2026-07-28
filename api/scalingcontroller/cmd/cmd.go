package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/Gthulhu/api/scalingcontroller"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spf13/cobra"
)

type fileConfig struct {
	PrometheusAddress     string                           `json:"prometheusAddress"`
	ManagerAddress        string                           `json:"managerAddress"`
	ManagerToken          string                           `json:"managerToken"`
	StrategyMode          string                           `json:"strategyMode"`
	StrategyNamespace     string                           `json:"strategyNamespace"`
	Kubeconfig            string                           `json:"kubeconfig"`
	KedaEnabled           bool                             `json:"kedaEnabled"`
	ListenAddress         string                           `json:"listenAddress"`
	PollInterval          string                           `json:"pollInterval"`
	Cooldown              string                           `json:"cooldown"`
	Hysteresis            int                              `json:"hysteresis"`
	BaselinePriority      int                              `json:"baselinePriority"`
	BoostedPriority       int                              `json:"boostedPriority"`
	BaselineExecutionTime int64                            `json:"baselineExecutionTime"`
	BoostedExecutionTime  int64                            `json:"boostedExecutionTime"`
	Targets               []scalingcontroller.TargetConfig `json:"targets"`
}

var configPath string

func init() {
	ScalingControllerCmd.Flags().StringVar(&configPath, "config", "/etc/gthulhu-scaling-controller/config.json", "Controller JSON config path")
}

var ScalingControllerCmd = &cobra.Command{
	Use:   "scaling-controller",
	Short: "Run the Scheduling-Aware Scaling Controller",
	RunE:  runScalingController,
}

func runScalingController(cmd *cobra.Command, args []string) error {
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	pollInterval, err := time.ParseDuration(defaultString(cfg.PollInterval, "30s"))
	if err != nil {
		return fmt.Errorf("parse pollInterval: %w", err)
	}
	cooldown, err := time.ParseDuration(defaultString(cfg.Cooldown, "300s"))
	if err != nil {
		return fmt.Errorf("parse cooldown: %w", err)
	}

	promClient := &scalingcontroller.HTTPPrometheusClient{Address: cfg.PrometheusAddress}
	var strategyClient scalingcontroller.StrategyClient
	if cfg.StrategyMode == "manager" {
		strategyClient = &scalingcontroller.HTTPStrategyClient{ManagerAddress: cfg.ManagerAddress, Token: cfg.ManagerToken}
	} else {
		strategyClient, err = scalingcontroller.NewCRDStrategyClient(cfg.Kubeconfig, cfg.StrategyNamespace)
		if err != nil {
			return fmt.Errorf("create ScheduleStrategy CRD client: %w", err)
		}
	}
	var scaler scalingcontroller.Scaler
	if cfg.KedaEnabled {
		s, err := scalingcontroller.NewKedaScaler(cfg.Kubeconfig)
		if err != nil {
			return fmt.Errorf("create KEDA scaler: %w", err)
		}
		scaler = s
	}
	controller, err := scalingcontroller.NewController(scalingcontroller.Config{
		PollInterval: pollInterval,
		Cooldown:     cooldown,
		Hysteresis:   cfg.Hysteresis,
		BaselineProfile: scalingcontroller.Profile{
			Priority:      cfg.BaselinePriority,
			ExecutionTime: cfg.BaselineExecutionTime,
		},
		BoostedProfile: scalingcontroller.Profile{
			Priority:      cfg.BoostedPriority,
			ExecutionTime: cfg.BoostedExecutionTime,
		},
		Targets: cfg.Targets,
	}, promClient, strategyClient, scaler)
	if err != nil {
		return err
	}

	metrics := newControllerMetrics()
	prometheus.MustRegister(metrics.state, metrics.transitions, metrics.signals, metrics.actions, metrics.queryErrors, metrics.kedaAvailable, metrics.managerAvailable)
	metrics.kedaAvailable.Set(boolFloat(cfg.KedaEnabled))
	metrics.managerAvailable.Set(0)

	listenAddress := defaultString(cfg.ListenAddress, ":9090")
	go func() {
		http.Handle("/metrics", promhttp.Handler())
		http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })
		if err := http.ListenAndServe(listenAddress, nil); err != nil {
			log.Printf("controller metrics server stopped: %v", err)
		}
	}()

	ctx := cmd.Context()
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		if managerClient, ok := strategyClient.(*scalingcontroller.HTTPStrategyClient); ok {
			metrics.managerAvailable.Set(boolFloat(managerClient.Healthy(ctx)))
		}
		results := controller.StepAll(ctx)
		recordResults(metrics, results)
		recordStatus(metrics, controller.Status())
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func loadConfig(path string) (*fileConfig, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg fileConfig
	if err := json.Unmarshal(body, &cfg); err != nil {
		return nil, err
	}
	if cfg.PrometheusAddress == "" {
		return nil, fmt.Errorf("prometheusAddress is required")
	}
	if cfg.StrategyMode == "" {
		cfg.StrategyMode = "manager"
	}
	if token := os.Getenv("GTHULHU_MANAGER_TOKEN"); token != "" {
		cfg.ManagerToken = token
	}
	if cfg.StrategyMode == "manager" && cfg.ManagerAddress == "" {
		return nil, fmt.Errorf("managerAddress is required when strategyMode=manager")
	}
	if cfg.StrategyMode == "manager" && cfg.ManagerToken == "" {
		return nil, fmt.Errorf("GTHULHU_MANAGER_TOKEN is required when strategyMode=manager")
	}
	return &cfg, nil
}

type controllerMetrics struct {
	state            *prometheus.GaugeVec
	transitions      *prometheus.GaugeVec
	signals          *prometheus.GaugeVec
	actions          *prometheus.CounterVec
	queryErrors      *prometheus.CounterVec
	kedaAvailable    prometheus.Gauge
	managerAvailable prometheus.Gauge
}

func newControllerMetrics() controllerMetrics {
	return controllerMetrics{
		state:            prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "gthulhu_scaling_controller_state", Help: "Current target state as a labeled one-hot gauge."}, []string{"target", "state"}),
		transitions:      prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "gthulhu_scaling_controller_last_transition_timestamp_seconds", Help: "Last state transition timestamp."}, []string{"target"}),
		signals:          prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "gthulhu_scaling_controller_congestion_signals", Help: "Current service and scheduling signal values."}, []string{"target", "signal"}),
		actions:          prometheus.NewCounterVec(prometheus.CounterOpts{Name: "gthulhu_scaling_controller_actions_total", Help: "Controller actions by result."}, []string{"target", "action", "result"}),
		queryErrors:      prometheus.NewCounterVec(prometheus.CounterOpts{Name: "gthulhu_scaling_controller_prometheus_query_errors_total", Help: "Prometheus query errors by target."}, []string{"target"}),
		kedaAvailable:    prometheus.NewGauge(prometheus.GaugeOpts{Name: "gthulhu_scaling_controller_keda_available", Help: "Whether KEDA integration is enabled."}),
		managerAvailable: prometheus.NewGauge(prometheus.GaugeOpts{Name: "gthulhu_scaling_controller_manager_available", Help: "Whether Manager integration is configured."}),
	}
}

func recordResults(metrics controllerMetrics, results []scalingcontroller.ActionResult) {
	for _, result := range results {
		label := "success"
		if result.Err != nil {
			label = "error"
			if result.Action == "query_service" || result.Action == "query_scheduling" {
				metrics.queryErrors.WithLabelValues(result.Target).Inc()
			}
			log.Printf("controller action failed target=%s action=%s err=%v", result.Target, result.Action, result.Err)
		}
		metrics.actions.WithLabelValues(result.Target, result.Action, label).Inc()
	}
}

func recordStatus(metrics controllerMetrics, status map[string]scalingcontroller.TargetStatus) {
	states := []scalingcontroller.State{scalingcontroller.StateNormal, scalingcontroller.StateCongested, scalingcontroller.StateRecovery}
	for target, st := range status {
		for _, state := range states {
			value := 0.0
			if st.State == state {
				value = 1
			}
			metrics.state.WithLabelValues(target, string(state)).Set(value)
		}
		if !st.LastTransitionTime.IsZero() {
			metrics.transitions.WithLabelValues(target).Set(float64(st.LastTransitionTime.Unix()))
		}
		metrics.signals.WithLabelValues(target, "service").Set(st.ServiceValue)
		metrics.signals.WithLabelValues(target, "scheduling").Set(st.SchedulingValue)
	}
}

func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func boolFloat(value bool) float64 {
	if value {
		return 1
	}
	return 0
}

func WithContext(ctx context.Context) *cobra.Command {
	cmd := *ScalingControllerCmd
	cmd.SetContext(ctx)
	return &cmd
}
