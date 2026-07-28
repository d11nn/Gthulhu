package scalingcontroller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHTTPStrategyClientUsesInternalManagerUpsert(t *testing.T) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPut, r.Method)
		require.Equal(t, "/api/v1/internal/strategies", r.URL.Path)
		require.Equal(t, "Bearer shared-token", r.Header.Get("Authorization"))
		var payload strategyPayload
		require.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
		require.Equal(t, "aether-smf-autoscaling", payload.StrategyNamespace)
		require.Equal(t, 2, payload.Priority)
		require.Equal(t, int64(50000000), payload.ExecutionTime)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{"strategyId":"000000000000000000000002"}}`))
	}))
	defer server.Close()

	client := &HTTPStrategyClient{ManagerAddress: server.URL, Token: "shared-token"}
	err := client.ApplyProfile(context.Background(), TargetConfig{
		Name:      "smf",
		Namespace: "aether-5gc",
		Selector:  map[string]string{"app": "smf", "release": "sd-core"},
	}, Profile{Priority: 2, ExecutionTime: 50000000})
	require.NoError(t, err)
}
