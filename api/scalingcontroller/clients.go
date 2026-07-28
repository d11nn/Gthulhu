package scalingcontroller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

type HTTPPrometheusClient struct {
	Address string
	Client  *http.Client
}

func (c *HTTPPrometheusClient) Query(ctx context.Context, query string) (float64, error) {
	if c.Client == nil {
		c.Client = &http.Client{Timeout: 10 * time.Second}
	}
	base := strings.TrimRight(c.Address, "/") + "/api/v1/query"
	reqURL, err := url.Parse(base)
	if err != nil {
		return 0, err
	}
	q := reqURL.Query()
	q.Set("query", query)
	reqURL.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL.String(), nil)
	if err != nil {
		return 0, err
	}
	resp, err := c.Client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return 0, fmt.Errorf("prometheus query returned HTTP %d", resp.StatusCode)
	}
	var body struct {
		Status string `json:"status"`
		Data   struct {
			Result []struct {
				Value []interface{} `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return 0, err
	}
	if body.Status != "success" {
		return 0, fmt.Errorf("prometheus query status %q", body.Status)
	}
	if len(body.Data.Result) == 0 || len(body.Data.Result[0].Value) < 2 {
		return 0, nil
	}
	value, ok := body.Data.Result[0].Value[1].(string)
	if !ok {
		return 0, fmt.Errorf("unexpected prometheus value format")
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, err
	}
	return parsed, nil
}

type HTTPStrategyClient struct {
	ManagerAddress string
	Token          string
	Client         *http.Client
}

type labelSelectorPayload struct {
	Key   string `json:"key,omitempty"`
	Value string `json:"value,omitempty"`
}

type strategyPayload struct {
	StrategyNamespace string                 `json:"strategyNamespace,omitempty"`
	LabelSelectors    []labelSelectorPayload `json:"labelSelectors,omitempty"`
	K8sNamespace      []string               `json:"k8sNamespace,omitempty"`
	Priority          int                    `json:"priority,omitempty"`
	ExecutionTime     int64                  `json:"executionTime,omitempty"`
}

func (c *HTTPStrategyClient) Healthy(ctx context.Context) bool {
	if c.Client == nil {
		c.Client = &http.Client{Timeout: 10 * time.Second}
	}
	endpoint := strings.TrimRight(c.ManagerAddress, "/") + "/health"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false
	}
	resp, err := c.Client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode < http.StatusBadRequest
}

func (c *HTTPStrategyClient) ApplyProfile(ctx context.Context, target TargetConfig, profile Profile) error {
	if c.Client == nil {
		c.Client = &http.Client{Timeout: 10 * time.Second}
	}
	selectors := make([]labelSelectorPayload, 0, len(target.Selector))
	for key, value := range target.Selector {
		selectors = append(selectors, labelSelectorPayload{Key: key, Value: value})
	}
	payload := strategyPayload{
		StrategyNamespace: "aether-" + target.Name + "-autoscaling",
		LabelSelectors:    selectors,
		K8sNamespace:      []string{target.Namespace},
		Priority:          profile.Priority,
		ExecutionTime:     profile.ExecutionTime,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	endpoint := strings.TrimRight(c.ManagerAddress, "/") + "/api/v1/internal/strategies"
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("manager strategy update returned HTTP %d", resp.StatusCode)
	}
	return nil
}

type KedaScaler struct {
	client dynamic.Interface
}

func NewKedaScaler(kubeconfig string) (*KedaScaler, error) {
	var cfg *rest.Config
	var err error
	if kubeconfig != "" {
		cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else {
		cfg, err = rest.InClusterConfig()
	}
	if err != nil {
		return nil, err
	}
	client, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &KedaScaler{client: client}, nil
}

func (s *KedaScaler) SetCongested(ctx context.Context, target TargetConfig, congested bool) error {
	if s == nil || s.client == nil {
		return nil
	}
	minReplicas := target.MinReplicas
	if congested && target.MaxReplicas > target.MinReplicas {
		minReplicas = target.MinReplicas + 1
	}
	patchBody, err := json.Marshal(map[string]interface{}{
		"spec": map[string]interface{}{
			"minReplicaCount": minReplicas,
			"maxReplicaCount": target.MaxReplicas,
		},
	})
	if err != nil {
		return err
	}
	gvr := schema.GroupVersionResource{Group: "keda.sh", Version: "v1alpha1", Resource: "scaledobjects"}
	_, err = s.client.Resource(gvr).Namespace(target.Namespace).Patch(ctx, target.ScaledObjectName, types.MergePatchType, patchBody, metav1.PatchOptions{})
	return err
}

type CRDStrategyClient struct {
	client    dynamic.Interface
	namespace string
}

func NewCRDStrategyClient(kubeconfig, namespace string) (*CRDStrategyClient, error) {
	var cfg *rest.Config
	var err error
	if kubeconfig != "" {
		cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else {
		cfg, err = rest.InClusterConfig()
	}
	if err != nil {
		return nil, err
	}
	client, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	if namespace == "" {
		namespace = "gthulhu-system"
	}
	return &CRDStrategyClient{client: client, namespace: namespace}, nil
}

func (c *CRDStrategyClient) ApplyProfile(ctx context.Context, target TargetConfig, profile Profile) error {
	if c == nil || c.client == nil {
		return nil
	}
	name := "aether-" + target.Name + "-autoscaling"
	selectors := make([]interface{}, 0, len(target.Selector))
	for key, value := range target.Selector {
		selectors = append(selectors, map[string]interface{}{"key": key, "value": value})
	}
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "gthulhu.io/v1alpha1",
		"kind":       "SchedulingStrategy",
		"metadata": map[string]interface{}{
			"name": name,
		},
		"spec": map[string]interface{}{
			"strategyNamespace": name,
			"labelSelectors":    selectors,
			"k8sNamespaces":     []interface{}{target.Namespace},
			"priority":          int64(profile.Priority),
			"executionTime":     profile.ExecutionTime,
			"updaterID":         "scheduling-aware-scaling-controller",
			"updatedTime":       time.Now().Unix(),
		},
	}}
	gvr := schema.GroupVersionResource{Group: "gthulhu.io", Version: "v1alpha1", Resource: "schedulingstrategies"}
	_, err := c.client.Resource(gvr).Namespace(c.namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, createErr := c.client.Resource(gvr).Namespace(c.namespace).Create(ctx, obj, metav1.CreateOptions{})
		return createErr
	}
	if err != nil {
		return err
	}
	patchBody, err := json.Marshal(map[string]interface{}{"spec": obj.Object["spec"]})
	if err != nil {
		return err
	}
	_, err = c.client.Resource(gvr).Namespace(c.namespace).Patch(ctx, name, types.MergePatchType, patchBody, metav1.PatchOptions{})
	return err
}
