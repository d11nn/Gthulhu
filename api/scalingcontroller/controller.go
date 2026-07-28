package scalingcontroller

import (
	"context"
	"fmt"
	"time"
)

type State string

const (
	StateNormal    State = "Normal"
	StateCongested State = "Congested"
	StateRecovery  State = "Recovery"
)

type Profile struct {
	Priority      int
	ExecutionTime int64
}

type TargetConfig struct {
	Name                        string            `json:"name"`
	Namespace                   string            `json:"namespace"`
	Selector                    map[string]string `json:"selector"`
	ServiceQuery                string            `json:"serviceQuery"`
	SchedulingQuery             string            `json:"schedulingQuery"`
	ServiceThreshold            float64           `json:"serviceThreshold"`
	SchedulingThreshold         float64           `json:"schedulingThreshold"`
	RecoveryServiceThreshold    float64           `json:"recoveryServiceThreshold"`
	RecoverySchedulingThreshold float64           `json:"recoverySchedulingThreshold"`
	MinReplicas                 int32             `json:"minReplicas"`
	MaxReplicas                 int32             `json:"maxReplicas"`
	ScaledObjectName            string            `json:"scaledObjectName"`
}

type Config struct {
	PollInterval    time.Duration
	Cooldown        time.Duration
	Hysteresis      int
	BaselineProfile Profile
	BoostedProfile  Profile
	Targets         []TargetConfig
}

type PrometheusClient interface {
	Query(ctx context.Context, query string) (float64, error)
}

type StrategyClient interface {
	ApplyProfile(ctx context.Context, target TargetConfig, profile Profile) error
}

type Scaler interface {
	SetCongested(ctx context.Context, target TargetConfig, congested bool) error
}

type ActionResult struct {
	Target string
	Action string
	Err    error
}

type TargetStatus struct {
	State              State
	LastTransitionTime time.Time
	ServiceValue       float64
	SchedulingValue    float64
	ServiceBad         bool
	SchedulingBad      bool
}

type targetRuntime struct {
	config          TargetConfig
	state           State
	congestedCount  int
	recoveryCount   int
	lastTransition  time.Time
	serviceValue    float64
	schedulingValue float64
	serviceBad      bool
	schedulingBad   bool
	scaleApplied    bool
	profileApplied  bool
}

type Controller struct {
	cfg      Config
	prom     PrometheusClient
	strategy StrategyClient
	scaler   Scaler
	targets  map[string]*targetRuntime
	clock    func() time.Time
}

func NewController(cfg Config, prom PrometheusClient, strategy StrategyClient, scaler Scaler) (*Controller, error) {
	if cfg.Hysteresis <= 0 {
		cfg.Hysteresis = 1
	}
	if cfg.Cooldown <= 0 {
		cfg.Cooldown = 5 * time.Minute
	}
	if prom == nil {
		return nil, fmt.Errorf("prometheus client is required")
	}
	c := &Controller{
		cfg:      cfg,
		prom:     prom,
		strategy: strategy,
		scaler:   scaler,
		targets:  make(map[string]*targetRuntime, len(cfg.Targets)),
		clock:    time.Now,
	}
	for _, target := range cfg.Targets {
		if target.Name == "" {
			return nil, fmt.Errorf("target name is required")
		}
		if target.Namespace == "" {
			return nil, fmt.Errorf("target %s namespace is required", target.Name)
		}
		if target.ServiceQuery == "" || target.SchedulingQuery == "" {
			return nil, fmt.Errorf("target %s requires serviceQuery and schedulingQuery", target.Name)
		}
		if target.RecoveryServiceThreshold == 0 {
			target.RecoveryServiceThreshold = target.ServiceThreshold * 0.8
		}
		if target.RecoverySchedulingThreshold == 0 {
			target.RecoverySchedulingThreshold = target.SchedulingThreshold * 0.8
		}
		if target.ScaledObjectName == "" {
			target.ScaledObjectName = target.Name + "-gthulhu-scaler"
		}
		c.targets[target.Name] = &targetRuntime{config: target, state: StateNormal}
	}
	return c, nil
}

func (c *Controller) SetClock(clock func() time.Time) {
	if clock != nil {
		c.clock = clock
	}
}

func (c *Controller) StepAll(ctx context.Context) []ActionResult {
	var results []ActionResult
	for name := range c.targets {
		results = append(results, c.Step(ctx, name)...)
	}
	return results
}

func (c *Controller) Step(ctx context.Context, targetName string) []ActionResult {
	rt, ok := c.targets[targetName]
	if !ok {
		return []ActionResult{{Target: targetName, Action: "evaluate", Err: fmt.Errorf("unknown target")}}
	}
	serviceValue, serviceErr := c.prom.Query(ctx, rt.config.ServiceQuery)
	schedulingValue, schedulingErr := c.prom.Query(ctx, rt.config.SchedulingQuery)
	if serviceErr != nil || schedulingErr != nil {
		if serviceErr != nil {
			return []ActionResult{{Target: targetName, Action: "query_service", Err: serviceErr}}
		}
		return []ActionResult{{Target: targetName, Action: "query_scheduling", Err: schedulingErr}}
	}

	rt.serviceValue = serviceValue
	rt.schedulingValue = schedulingValue
	rt.serviceBad = serviceValue >= rt.config.ServiceThreshold
	rt.schedulingBad = schedulingValue >= rt.config.SchedulingThreshold
	serviceRecovered := serviceValue <= rt.config.RecoveryServiceThreshold
	schedulingRecovered := schedulingValue <= rt.config.RecoverySchedulingThreshold

	now := c.clock()
	var results []ActionResult
	switch rt.state {
	case StateNormal:
		if !rt.lastTransition.IsZero() {
			results = append(results, c.reconcileActions(ctx, rt, false, c.cfg.BaselineProfile)...)
		}
		if rt.serviceBad && rt.schedulingBad {
			rt.congestedCount++
		} else {
			rt.congestedCount = 0
		}
		if rt.congestedCount >= c.cfg.Hysteresis {
			return c.transitionToCongested(ctx, rt, now)
		}
	case StateCongested:
		results = append(results, c.reconcileActions(ctx, rt, true, c.cfg.BoostedProfile)...)
		if serviceRecovered && schedulingRecovered {
			rt.recoveryCount++
		} else {
			rt.recoveryCount = 0
		}
		if rt.recoveryCount >= c.cfg.Hysteresis {
			rt.state = StateRecovery
			rt.lastTransition = now
			return append(results, ActionResult{Target: rt.config.Name, Action: "enter_recovery"})
		}
	case StateRecovery:
		results = append(results, c.reconcileActions(ctx, rt, true, c.cfg.BoostedProfile)...)
		if rt.serviceBad && rt.schedulingBad {
			return c.transitionToCongested(ctx, rt, now)
		}
		if now.Sub(rt.lastTransition) >= c.cfg.Cooldown && serviceRecovered && schedulingRecovered {
			return c.transitionToNormal(ctx, rt, now)
		}
	}
	return results
}

func (c *Controller) Status() map[string]TargetStatus {
	out := make(map[string]TargetStatus, len(c.targets))
	for name, rt := range c.targets {
		out[name] = TargetStatus{
			State:              rt.state,
			LastTransitionTime: rt.lastTransition,
			ServiceValue:       rt.serviceValue,
			SchedulingValue:    rt.schedulingValue,
			ServiceBad:         rt.serviceBad,
			SchedulingBad:      rt.schedulingBad,
		}
	}
	return out
}

func (c *Controller) transitionToCongested(ctx context.Context, rt *targetRuntime, now time.Time) []ActionResult {
	rt.state = StateCongested
	rt.lastTransition = now
	rt.congestedCount = 0
	rt.recoveryCount = 0
	rt.scaleApplied = false
	rt.profileApplied = false
	return c.reconcileActions(ctx, rt, true, c.cfg.BoostedProfile)
}

func (c *Controller) transitionToNormal(ctx context.Context, rt *targetRuntime, now time.Time) []ActionResult {
	rt.state = StateNormal
	rt.lastTransition = now
	rt.congestedCount = 0
	rt.recoveryCount = 0
	rt.scaleApplied = false
	rt.profileApplied = false
	return c.reconcileActions(ctx, rt, false, c.cfg.BaselineProfile)
}

func (c *Controller) reconcileActions(ctx context.Context, rt *targetRuntime, congested bool, profile Profile) []ActionResult {
	var results []ActionResult
	if c.scaler != nil && !rt.scaleApplied {
		action := "scale_in"
		if congested {
			action = "scale_out"
		}
		err := c.scaler.SetCongested(ctx, rt.config, congested)
		rt.scaleApplied = err == nil
		results = append(results, ActionResult{Target: rt.config.Name, Action: action, Err: err})
	}
	if c.strategy != nil && !rt.profileApplied {
		action := "restore_strategy"
		if congested {
			action = "boost_strategy"
		}
		err := c.strategy.ApplyProfile(ctx, rt.config, profile)
		rt.profileApplied = err == nil
		results = append(results, ActionResult{Target: rt.config.Name, Action: action, Err: err})
	}
	return results
}
