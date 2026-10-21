/*
 * Copyright The Kmesh Authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at:
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	kubefake "k8s.io/client-go/kubernetes/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayapiclient "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned"
	gatewayfake "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned/fake"

	"kmesh.net/kmesh/pkg/kube"
)

// fakeCLIClient is a kube.CLIClient backed by in-memory fakes.
type fakeCLIClient struct {
	kube.CLIClient
	kubeClient kubernetes.Interface
	gwClient   gatewayapiclient.Interface
	daemonAddr string
	forwards   []string
}

func (f *fakeCLIClient) Kube() kubernetes.Interface             { return f.kubeClient }
func (f *fakeCLIClient) GatewayAPI() gatewayapiclient.Interface { return f.gwClient }
func (f *fakeCLIClient) NewPortForwarder(podName, ns, _ string, _, _ int) (kube.PortForwarder, error) {
	f.forwards = append(f.forwards, ns+"/"+podName)
	return &fakeForwarder{addr: f.daemonAddr}, nil
}

type fakeForwarder struct{ addr string }

func (f *fakeForwarder) Start() error    { return nil }
func (f *fakeForwarder) Address() string { return f.addr }
func (f *fakeForwarder) Close()          {}

func daemonPod(name, node string, ready bool) *corev1.Pod {
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "kmesh-system", Labels: map[string]string{"app": "kmesh"}},
		Spec:       corev1.PodSpec{NodeName: node},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			PodIP:      "10.0.0.1",
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: status}},
		},
	}
}

func newToolRequest(args map[string]any) mcp.CallToolRequest {
	req := mcp.CallToolRequest{}
	req.Params.Arguments = args
	return req
}

func resultText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	require.NotNil(t, res)
	require.NotEmpty(t, res.Content)
	text, ok := res.Content[0].(mcp.TextContent)
	require.True(t, ok, "expected text content")
	return text.Text
}

func TestCondenseJsonDump(t *testing.T) {
	assert.Equal(t, "short", condenseJsonDump([]byte("short"), 10))

	out := condenseJsonDump([]byte(strings.Repeat("a", 20)), 10)
	assert.True(t, strings.HasPrefix(out, strings.Repeat("a", 10)))
	assert.Contains(t, out, "TRUNCATED")
}

func TestGetDefaultDaemonPod(t *testing.T) {
	tests := []struct {
		name    string
		pods    []*corev1.Pod
		want    string
		wantErr string
	}{
		{name: "no pods", wantErr: "no kmesh daemon pods found"},
		{
			name:    "none ready",
			pods:    []*corev1.Pod{daemonPod("kmesh-a", "node-a", false)},
			wantErr: "none are in Ready state",
		},
		{
			name: "skips unready pod",
			pods: []*corev1.Pod{daemonPod("kmesh-a", "node-a", false), daemonPod("kmesh-b", "node-b", true)},
			want: "kmesh-b",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cs := kubefake.NewSimpleClientset()
			for _, p := range tt.pods {
				_, err := cs.CoreV1().Pods(p.Namespace).Create(context.Background(), p, metav1.CreateOptions{})
				require.NoError(t, err)
			}
			h := &McpHandler{cliClient: &fakeCLIClient{kubeClient: cs}}

			got, err := h.getDefaultDaemonPod(context.Background(), "")
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestFetchFromDaemon(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/version":
			_, _ = w.Write([]byte(`{"mode":"dual-engine"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cs := kubefake.NewSimpleClientset(daemonPod("kmesh-a", "node-a", true))
	fc := &fakeCLIClient{kubeClient: cs, daemonAddr: strings.TrimPrefix(srv.URL, "http://")}
	h := &McpHandler{cliClient: fc}

	raw, err := h.fetchFromDaemon(context.Background(), "", "", "version")
	require.NoError(t, err)
	assert.JSONEq(t, `{"mode":"dual-engine"}`, string(raw))
	assert.Equal(t, []string{"kmesh-system/kmesh-a"}, fc.forwards, "an empty pod name should be auto-discovered")

	_, err = h.fetchFromDaemon(context.Background(), "kmesh-a", "kmesh-system", "missing")
	require.ErrorContains(t, err, "status 404")
}

func TestHandleBpfDump(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/version" {
			_, _ = w.Write([]byte(`{"mode":"dual-engine"}`))
			return
		}
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	cs := kubefake.NewSimpleClientset(daemonPod("kmesh-a", "node-a", true))
	h := &McpHandler{cliClient: &fakeCLIClient{kubeClient: cs, daemonAddr: strings.TrimPrefix(srv.URL, "http://")}}

	t.Run("mode is auto-detected from the daemon", func(t *testing.T) {
		res, err := h.HandleBpfDump(context.Background(), newToolRequest(nil))
		require.NoError(t, err)
		assert.False(t, res.IsError)
		assert.Equal(t, "/debug/config_dump/bpf/dual-engine", gotPath)
	})

	t.Run("invalid mode is rejected", func(t *testing.T) {
		res, err := h.HandleBpfDump(context.Background(), newToolRequest(map[string]any{"mode": "bogus"}))
		require.NoError(t, err)
		assert.True(t, res.IsError)
		assert.Contains(t, resultText(t, res), "mode must be")
	})
}

func TestHandleListDaemonPods(t *testing.T) {
	cs := kubefake.NewSimpleClientset(
		daemonPod("kmesh-a", "node-a", true),
		daemonPod("kmesh-b", "node-b", false),
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "kmesh-system", Labels: map[string]string{"app": "other"}}},
	)
	h := &McpHandler{cliClient: &fakeCLIClient{kubeClient: cs}}

	res, err := h.HandleListDaemonPods(context.Background(), newToolRequest(nil))
	require.NoError(t, err)
	text := resultText(t, res)
	assert.Contains(t, text, "kmesh-a")
	assert.Contains(t, text, "node-b")
	assert.NotContains(t, text, "other")
}

func TestHandleGetMeshNamespaces(t *testing.T) {
	cs := kubefake.NewSimpleClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "meshed", Labels: map[string]string{"istio.io/dataplane-mode": "Kmesh"}}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "plain"}},
	)
	h := &McpHandler{cliClient: &fakeCLIClient{kubeClient: cs}}

	res, err := h.HandleGetMeshNamespaces(context.Background(), newToolRequest(nil))
	require.NoError(t, err)
	text := resultText(t, res)
	assert.Contains(t, text, "meshed")
	assert.NotContains(t, text, "plain")
}

func TestWaypointHandlers(t *testing.T) {
	waypoint := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "wp", Namespace: "default"},
		Spec:       gatewayv1.GatewaySpec{GatewayClassName: "istio-waypoint"},
		Status: gatewayv1.GatewayStatus{Conditions: []metav1.Condition{
			{Type: "Programmed", Status: metav1.ConditionTrue, Reason: "Programmed"},
		}},
	}
	ingress := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "ingress", Namespace: "default"},
		Spec:       gatewayv1.GatewaySpec{GatewayClassName: "istio"},
	}
	gwClient := gatewayfake.NewSimpleClientset()
	for _, gw := range []*gatewayv1.Gateway{waypoint, ingress} {
		_, err := gwClient.GatewayV1().Gateways(gw.Namespace).Create(context.Background(), gw, metav1.CreateOptions{})
		require.NoError(t, err)
	}
	h := &McpHandler{cliClient: &fakeCLIClient{kubeClient: kubefake.NewSimpleClientset(), gwClient: gwClient}}

	res, err := h.HandleListWaypoints(context.Background(), newToolRequest(nil))
	require.NoError(t, err)
	text := resultText(t, res)
	assert.Contains(t, text, "wp")
	assert.NotContains(t, text, "ingress", "non-waypoint gateways must be filtered out")

	res, err = h.HandleListWaypoints(context.Background(), newToolRequest(map[string]any{"namespace": "empty"}))
	require.NoError(t, err)
	assert.Equal(t, "No Waypoint Gateways found.", resultText(t, res))

	res, err = h.HandleGetWaypointStatus(context.Background(), newToolRequest(map[string]any{"name": "wp"}))
	require.NoError(t, err)
	assert.Contains(t, resultText(t, res), "Type: Programmed, Status: True")

	res, err = h.HandleGetWaypointStatus(context.Background(), newToolRequest(map[string]any{"name": "nope"}))
	require.NoError(t, err)
	assert.True(t, res.IsError)

	res, err = h.HandleGetWaypointStatus(context.Background(), newToolRequest(nil))
	require.NoError(t, err)
	assert.True(t, res.IsError, "name is required")
}
