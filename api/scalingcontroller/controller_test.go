package scalingcontroller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type fakePrometheus struct {
	values map[string]float64
}

func (f *fakePrometheus) Query(ctx context.Context, query string) (float64, error) {
	return f.values[query], nil
}

type fakeStrategy struct {
	profiles     []Profile
	failuresLeft int
}

func (f *fakeStrategy) ApplyProfile(ctx context.Context, target TargetConfig, profile Profile) error {
	f.profiles = append(f.profiles, profile)
	if f.failuresLeft > 0 {
		f.failuresLeft--
		return errors.New("strategy unavailable")
	}
	return nil
}

type fakeScaler struct {
	states []bool
}

func (f *fakeScaler) SetCongested(ctx context.Context, target TargetConfig, congested bool) error {
	f.states = append(f.states, congested)
	return nil
}

func testController(t *testing.T, prom *fakePrometheus, strategy *fakeStrategy, scaler *fakeScaler) *Controller {
	t.Helper()
	controller, err := NewController(Config{
		Cooldown:   time.Minute,
		Hysteresis: 2,
		BaselineProfile: Profile{
			Priority:      10,
			ExecutionTime: 20,
		},
		BoostedProfile: Profile{
			Priority:      2,
			ExecutionTime: 50,
		},
		Targets: []TargetConfig{{
			Name:                        "smf",
			Namespace:                   "aether-5gc",
			Selector:                    map[string]string{"app": "smf"},
			ServiceQuery:                "service",
			SchedulingQuery:             "scheduling",
			ServiceThreshold:            100,
			SchedulingThreshold:         10,
			RecoveryServiceThreshold:    70,
			RecoverySchedulingThreshold: 6,
			MinReplicas:                 1,
			MaxReplicas:                 5,
		}},
	}, prom, strategy, scaler)
	require.NoError(t, err)
	return controller
}

func TestControllerRequiresBothSignalsBeforeCongestion(t *testing.T) {
	prom := &fakePrometheus{values: map[string]float64{"service": 120, "scheduling": 5}}
	strategy := &fakeStrategy{}
	scaler := &fakeScaler{}
	controller := testController(t, prom, strategy, scaler)

	controller.Step(context.Background(), "smf")
	controller.Step(context.Background(), "smf")

	status := controller.Status()["smf"]
	require.Equal(t, StateNormal, status.State)
	require.Empty(t, strategy.profiles)
	require.Empty(t, scaler.states)
}

func TestControllerTransitionsToCongestedAfterHysteresis(t *testing.T) {
	prom := &fakePrometheus{values: map[string]float64{"service": 120, "scheduling": 12}}
	strategy := &fakeStrategy{}
	scaler := &fakeScaler{}
	controller := testController(t, prom, strategy, scaler)

	controller.Step(context.Background(), "smf")
	results := controller.Step(context.Background(), "smf")

	require.Len(t, results, 2)
	require.Equal(t, StateCongested, controller.Status()["smf"].State)
	require.Equal(t, []bool{true}, scaler.states)
	require.Equal(t, Profile{Priority: 2, ExecutionTime: 50}, strategy.profiles[0])
}

func TestControllerRecoversAfterCooldown(t *testing.T) {
	prom := &fakePrometheus{values: map[string]float64{"service": 120, "scheduling": 12}}
	strategy := &fakeStrategy{}
	scaler := &fakeScaler{}
	controller := testController(t, prom, strategy, scaler)
	now := time.Unix(100, 0)
	controller.SetClock(func() time.Time { return now })

	controller.Step(context.Background(), "smf")
	controller.Step(context.Background(), "smf")
	require.Equal(t, StateCongested, controller.Status()["smf"].State)

	prom.values["service"] = 50
	prom.values["scheduling"] = 3
	controller.Step(context.Background(), "smf")
	controller.Step(context.Background(), "smf")
	require.Equal(t, StateRecovery, controller.Status()["smf"].State)

	now = now.Add(time.Minute)
	controller.Step(context.Background(), "smf")
	require.Equal(t, StateNormal, controller.Status()["smf"].State)
	require.Equal(t, []bool{true, false}, scaler.states)
	require.Equal(t, Profile{Priority: 10, ExecutionTime: 20}, strategy.profiles[1])
}

func TestControllerRetriesFailedActionsWhileCongested(t *testing.T) {
	prom := &fakePrometheus{values: map[string]float64{"service": 120, "scheduling": 12}}
	strategy := &fakeStrategy{failuresLeft: 1}
	scaler := &fakeScaler{}
	controller := testController(t, prom, strategy, scaler)

	controller.Step(context.Background(), "smf")
	first := controller.Step(context.Background(), "smf")
	require.Len(t, first, 2)
	require.Error(t, first[1].Err)

	retry := controller.Step(context.Background(), "smf")
	require.Len(t, retry, 1)
	require.Equal(t, "boost_strategy", retry[0].Action)
	require.NoError(t, retry[0].Err)
	require.Len(t, strategy.profiles, 2)
	require.Equal(t, []bool{true}, scaler.states)
}
