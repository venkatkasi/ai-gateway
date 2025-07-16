// Copyright Envoy AI Gateway Authors
// SPDX-License-Identifier: Apache-2.0
// The full text of the Apache license is available in the LICENSE file at
// the root of the repo.

package controller

import (
	"context"
	"fmt"
	"strconv"
	"testing"

	egv1a1 "github.com/envoyproxy/gateway/api/v1alpha1"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zapcore"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	fake2 "k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"
	gwapiv1a2 "sigs.k8s.io/gateway-api/apis/v1alpha2"
	"sigs.k8s.io/yaml"

	aigv1a1 "github.com/envoyproxy/ai-gateway/api/v1alpha1"
	"github.com/envoyproxy/ai-gateway/filterapi"
	"github.com/envoyproxy/ai-gateway/internal/controller/rotators"
)

func TestGatewayController_Reconcile(t *testing.T) {
	fakeClient := requireNewFakeClientWithIndexes(t)
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zap.Options{Development: true, Level: zapcore.DebugLevel})))
	c := NewGatewayController(fakeClient, fake2.NewClientset(), ctrl.Log,
		"envoy-gateway-system", "/foo/bar/uds.sock", "docker.io/envoyproxy/ai-gateway-extproc:latest")

	const namespace = "ns"
	t.Run("not found must be non error", func(t *testing.T) {
		res, err := c.Reconcile(t.Context(), ctrl.Request{})
		require.NoError(t, err)
		require.Equal(t, ctrl.Result{}, res)
	})
	t.Run("gw found but no attached aigw route", func(t *testing.T) {
		err := fakeClient.Create(t.Context(), &gwapiv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: namespace},
			Spec:       gwapiv1.GatewaySpec{},
		})
		require.NoError(t, err)

		res, err := c.Reconcile(t.Context(), ctrl.Request{
			NamespacedName: client.ObjectKey{Name: "gw", Namespace: namespace},
		})
		require.NoError(t, err)
		require.Equal(t, ctrl.Result{}, res)
	})
	// Create a Gateway with attached AIGatewayRoutes.
	const okGwName = "ok-gw"
	err := fakeClient.Create(t.Context(), &gwapiv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: okGwName, Namespace: namespace},
		Spec:       gwapiv1.GatewaySpec{},
	})
	require.NoError(t, err)
	targets := []gwapiv1a2.LocalPolicyTargetReferenceWithSectionName{
		{
			LocalPolicyTargetReference: gwapiv1a2.LocalPolicyTargetReference{
				Name: okGwName, Kind: "Gateway", Group: "gateway.networking.k8s.io",
			},
		},
	}
	for _, aigwRoute := range []*aigv1a1.AIGatewayRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "route1", Namespace: namespace},
			Spec: aigv1a1.AIGatewayRouteSpec{
				TargetRefs: targets,
				Rules: []aigv1a1.AIGatewayRouteRule{
					{BackendRefs: []aigv1a1.AIGatewayRouteRuleBackendRef{{Name: "apple"}}},
				},
				APISchema: aigv1a1.VersionedAPISchema{Name: aigv1a1.APISchemaOpenAI, Version: ptr.To("v1")},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "route2", Namespace: namespace},
			Spec: aigv1a1.AIGatewayRouteSpec{
				TargetRefs: targets,
				Rules: []aigv1a1.AIGatewayRouteRule{
					{BackendRefs: []aigv1a1.AIGatewayRouteRuleBackendRef{{Name: "orange"}}},
				},
				APISchema: aigv1a1.VersionedAPISchema{Name: aigv1a1.APISchemaOpenAI},
			},
		},
	} {
		err = fakeClient.Create(t.Context(), aigwRoute)
		require.NoError(t, err)
	}
	// We also need to create corresponding AIServiceBackends.
	for _, aigwRoute := range []*aigv1a1.AIServiceBackend{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "apple", Namespace: namespace},
			Spec: aigv1a1.AIServiceBackendSpec{
				BackendRef: gwapiv1.BackendObjectReference{Name: "some-backend1", Namespace: ptr.To[gwapiv1.Namespace](namespace)},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "orange", Namespace: namespace},
			Spec: aigv1a1.AIServiceBackendSpec{
				BackendRef: gwapiv1.BackendObjectReference{Name: "some-backend1", Namespace: ptr.To[gwapiv1.Namespace](namespace)},
			},
		},
	} {
		err = fakeClient.Create(t.Context(), aigwRoute)
		require.NoError(t, err)
	}

	res, err := c.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKey{Name: okGwName, Namespace: namespace}})
	require.NoError(t, err)
	require.Equal(t, ctrl.Result{}, res)

	// Verify that side car extproc backend.
	var backend egv1a1.Backend
	err = fakeClient.Get(t.Context(), client.ObjectKey{Name: sideCarExtProcBackendName, Namespace: namespace}, &backend)
	require.NoError(t, err)
	require.Len(t, backend.Spec.Endpoints, 1)
	require.Equal(t, "/foo/bar/uds.sock", backend.Spec.Endpoints[0].Unix.Path)

	// Also make sure that EnvoyExtensionPolicy is created for the Gateway.
	var extPolicy egv1a1.EnvoyExtensionPolicy
	err = fakeClient.Get(t.Context(), client.ObjectKey{Name: fmt.Sprintf("ai-eg-eep-%s", okGwName), Namespace: namespace}, &extPolicy)
	require.NoError(t, err)
	require.Len(t, extPolicy.Spec.ExtProc, 1)
	require.Len(t, extPolicy.Spec.ExtProc[0].BackendRefs, 1)
	require.Equal(t, sideCarExtProcBackendName, string(extPolicy.Spec.ExtProc[0].BackendRefs[0].Name))
}

func TestGatewayController_reconcileFilterConfigSecret(t *testing.T) {
	fakeClient := requireNewFakeClientWithIndexes(t)
	kube := fake2.NewClientset()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zap.Options{Development: true, Level: zapcore.DebugLevel})))
	c := NewGatewayController(fakeClient, kube, ctrl.Log,
		"envoy-gateway-system", "/foo/bar/uds.sock",
		"docker.io/envoyproxy/ai-gateway-extproc:latest")

	const namespace = "ns"
	routes := []aigv1a1.AIGatewayRoute{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "route1", Namespace: namespace},
			Spec: aigv1a1.AIGatewayRouteSpec{
				Rules: []aigv1a1.AIGatewayRouteRule{
					{
						BackendRefs: []aigv1a1.AIGatewayRouteRuleBackendRef{{Name: "apple"}},
						Matches: []aigv1a1.AIGatewayRouteRuleMatch{
							{
								Headers: []gwapiv1.HTTPHeaderMatch{
									{
										Name:  aigv1a1.AIModelHeaderKey,
										Value: "mymodel",
									},
								},
							},
						},
					},
				},
				APISchema: aigv1a1.VersionedAPISchema{Name: aigv1a1.APISchemaOpenAI, Version: ptr.To("v1")},
				LLMRequestCosts: []aigv1a1.LLMRequestCost{
					{MetadataKey: "foo", Type: aigv1a1.LLMRequestCostTypeInputToken},
					{MetadataKey: "bar", Type: aigv1a1.LLMRequestCostTypeOutputToken},
					{MetadataKey: "baz", Type: aigv1a1.LLMRequestCostTypeTotalToken},
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "route2", Namespace: namespace},
			Spec: aigv1a1.AIGatewayRouteSpec{
				Rules: []aigv1a1.AIGatewayRouteRule{
					{BackendRefs: []aigv1a1.AIGatewayRouteRuleBackendRef{{Name: "orange"}}},
				},
				APISchema: aigv1a1.VersionedAPISchema{Name: aigv1a1.APISchemaOpenAI},
				LLMRequestCosts: []aigv1a1.LLMRequestCost{
					{MetadataKey: "foo", Type: aigv1a1.LLMRequestCostTypeInputToken}, // This should be ignored as it has the duplicate key.
					{MetadataKey: "cat", Type: aigv1a1.LLMRequestCostTypeCEL, CEL: ptr.To(`backend == 'foo.default' ?  input_tokens + output_tokens : total_tokens`)},
				},
			},
		},
	}
	// We also need to create corresponding AIServiceBackends.
	for _, aigwRoute := range []*aigv1a1.AIServiceBackend{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "apple", Namespace: namespace},
			Spec: aigv1a1.AIServiceBackendSpec{
				BackendRef: gwapiv1.BackendObjectReference{Name: "some-backend1", Namespace: ptr.To[gwapiv1.Namespace](namespace)},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "orange", Namespace: namespace},
			Spec: aigv1a1.AIServiceBackendSpec{
				BackendRef: gwapiv1.BackendObjectReference{Name: "some-backend1", Namespace: ptr.To[gwapiv1.Namespace](namespace)},
			},
		},
	} {
		err := fakeClient.Create(t.Context(), aigwRoute)
		require.NoError(t, err)
	}

	for range 2 { // Reconcile twice to make sure the secret update path is working.
		err := c.reconcileFilterConfigSecret(t.Context(), &gwapiv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: namespace},
		}, routes, "foouuid")
		require.NoError(t, err)

		secret, err := kube.CoreV1().Secrets("envoy-gateway-system").
			Get(t.Context(), FilterConfigSecretPerGatewayName("gw", namespace), metav1.GetOptions{})
		require.NoError(t, err)
		configStr, ok := secret.StringData[FilterConfigKeyInSecret]
		require.True(t, ok)
		var fc filterapi.Config
		require.NoError(t, yaml.Unmarshal([]byte(configStr), &fc))
		require.Len(t, fc.LLMRequestCosts, 4)
		require.Equal(t, filterapi.LLMRequestCostTypeInputToken, fc.LLMRequestCosts[0].Type)
		require.Equal(t, filterapi.LLMRequestCostTypeOutputToken, fc.LLMRequestCosts[1].Type)
		require.Equal(t, filterapi.LLMRequestCostTypeTotalToken, fc.LLMRequestCosts[2].Type)
		require.Equal(t, filterapi.LLMRequestCostTypeCEL, fc.LLMRequestCosts[3].Type)
		require.Equal(t, `backend == 'foo.default' ?  input_tokens + output_tokens : total_tokens`, fc.LLMRequestCosts[3].CEL)
		require.Len(t, fc.Models, 1)
		require.Equal(t, "mymodel", fc.Models[0].Name)
	}
}

func TestGatewayController_bspToFilterAPIBackendAuth(t *testing.T) {
	fakeClient := requireNewFakeClientWithIndexes(t)
	kube := fake2.NewClientset()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zap.Options{Development: true, Level: zapcore.DebugLevel})))
	c := NewGatewayController(fakeClient, kube, ctrl.Log,
		"envoy-gateway-system", "/foo/bar/uds.sock",
		"docker.io/envoyproxy/ai-gateway-extproc:latest")

	const namespace = "ns"
	for _, bsp := range []*aigv1a1.BackendSecurityPolicy{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "bsp-apikey", Namespace: namespace},
			Spec: aigv1a1.BackendSecurityPolicySpec{
				Type: aigv1a1.BackendSecurityPolicyTypeAPIKey,
				APIKey: &aigv1a1.BackendSecurityPolicyAPIKey{
					SecretRef: &gwapiv1.SecretObjectReference{Name: "api-key-secret"},
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "aws-credentials-file", Namespace: namespace},
			Spec: aigv1a1.BackendSecurityPolicySpec{
				Type: aigv1a1.BackendSecurityPolicyTypeAWSCredentials,
				AWSCredentials: &aigv1a1.BackendSecurityPolicyAWSCredentials{
					CredentialsFile: &aigv1a1.AWSCredentialsFile{
						SecretRef: &gwapiv1.SecretObjectReference{Name: "aws-credentials-file-secret"},
					},
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "aws-oidc", Namespace: namespace},
			Spec: aigv1a1.BackendSecurityPolicySpec{
				Type: aigv1a1.BackendSecurityPolicyTypeAWSCredentials,
				AWSCredentials: &aigv1a1.BackendSecurityPolicyAWSCredentials{
					OIDCExchangeToken: &aigv1a1.AWSOIDCExchangeToken{},
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "azure-oidc", Namespace: namespace},
			Spec: aigv1a1.BackendSecurityPolicySpec{
				Type:             aigv1a1.BackendSecurityPolicyTypeAzureCredentials,
				AzureCredentials: &aigv1a1.BackendSecurityPolicyAzureCredentials{},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "gcp", Namespace: namespace},
			Spec: aigv1a1.BackendSecurityPolicySpec{
				Type:           aigv1a1.BackendSecurityPolicyTypeGCPCredentials,
				GCPCredentials: &aigv1a1.BackendSecurityPolicyGCPCredentials{},
			},
		},
	} {
		require.NoError(t, fakeClient.Create(t.Context(), bsp))
	}
	for _, s := range []*corev1.Secret{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "api-key-secret", Namespace: namespace},
			StringData: map[string]string{apiKeyInSecret: "thisisapikey"},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "aws-credentials-file-secret", Namespace: namespace},
			StringData: map[string]string{rotators.AwsCredentialsKey: "thisisawscredentials"},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: rotators.GetBSPSecretName("aws-oidc"), Namespace: namespace},
			StringData: map[string]string{rotators.AwsCredentialsKey: "thisisawscredentials"},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: rotators.GetBSPSecretName("azure-oidc"), Namespace: namespace},
			StringData: map[string]string{rotators.AzureAccessTokenKey: "thisisazurecredentials"},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: rotators.GetBSPSecretName("gcp"), Namespace: namespace},
			StringData: map[string]string{rotators.GCPAccessTokenKey: "thisisgcpcredentials"},
		},
	} {
		_, err := kube.CoreV1().Secrets(namespace).Create(t.Context(), s, metav1.CreateOptions{})
		require.NoError(t, err)
	}

	for _, tc := range []struct {
		bspName string
		exp     *filterapi.BackendAuth
	}{
		{
			bspName: "bsp-apikey",
			exp:     &filterapi.BackendAuth{APIKey: &filterapi.APIKeyAuth{Key: "thisisapikey"}},
		},
		{
			bspName: "aws-credentials-file",
			exp: &filterapi.BackendAuth{
				AWSAuth: &filterapi.AWSAuth{
					CredentialFileLiteral: "thisisawscredentials",
				},
			},
		},
		{
			bspName: "aws-oidc",
			exp: &filterapi.BackendAuth{
				AWSAuth: &filterapi.AWSAuth{CredentialFileLiteral: "thisisawscredentials"},
			},
		},
		{
			bspName: "azure-oidc",
			exp: &filterapi.BackendAuth{
				AzureAuth: &filterapi.AzureAuth{AccessToken: "thisisazurecredentials"},
			},
		},
		{
			bspName: "gcp",
			exp: &filterapi.BackendAuth{
				GCPAuth: &filterapi.GCPAuth{AccessToken: "thisisgcpcredentials"},
			},
		},
	} {
		t.Run(tc.bspName, func(t *testing.T) {
			auth, err := c.bspToFilterAPIBackendAuth(t.Context(), namespace, tc.bspName)
			require.NoError(t, err)
			require.Equal(t, tc.exp, auth)
		})
	}
}

func TestGatewayController_bspToFilterAPIBackendAuth_ErrorCases(t *testing.T) {
	fakeClient := requireNewFakeClientWithIndexes(t)
	c := NewGatewayController(fakeClient, fake2.NewClientset(), ctrl.Log,
		"envoy-gateway-system", "/foo/bar/uds.sock", "docker.io/envoyproxy/ai-gateway-extproc:latest")

	ctx := context.Background()
	namespace := "test-namespace"

	tests := []struct {
		name          string
		bspName       string
		setupBSP      *aigv1a1.BackendSecurityPolicy
		setupSecret   *corev1.Secret
		expectedError string
	}{
		{
			name:          "missing backend security policy",
			bspName:       "missing-bsp",
			expectedError: "failed to get BackendSecurityPolicy missing-bsp",
		},
		{
			name:    "api key type with missing secret",
			bspName: "api-key-bsp",
			setupBSP: &aigv1a1.BackendSecurityPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: "api-key-bsp", Namespace: namespace},
				Spec: aigv1a1.BackendSecurityPolicySpec{
					Type: aigv1a1.BackendSecurityPolicyTypeAPIKey,
					APIKey: &aigv1a1.BackendSecurityPolicyAPIKey{
						SecretRef: &gwapiv1.SecretObjectReference{
							Name: "missing-secret",
						},
					},
				},
			},
			expectedError: "failed to get secret missing-secret",
		},
		{
			name:    "aws credentials with credentials file missing secret",
			bspName: "aws-creds-file-bsp",
			setupBSP: &aigv1a1.BackendSecurityPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: "aws-creds-file-bsp", Namespace: namespace},
				Spec: aigv1a1.BackendSecurityPolicySpec{
					Type: aigv1a1.BackendSecurityPolicyTypeAWSCredentials,
					AWSCredentials: &aigv1a1.BackendSecurityPolicyAWSCredentials{
						Region: "us-west-2",
						CredentialsFile: &aigv1a1.AWSCredentialsFile{
							SecretRef: &gwapiv1.SecretObjectReference{
								Name: "missing-aws-secret",
							},
						},
					},
				},
			},
			expectedError: "failed to get secret missing-aws-secret",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.setupBSP != nil {
				err := fakeClient.Create(ctx, tt.setupBSP)
				require.NoError(t, err)
			}

			if tt.setupSecret != nil {
				err := fakeClient.Create(ctx, tt.setupSecret)
				require.NoError(t, err)
			}

			result, err := c.bspToFilterAPIBackendAuth(ctx, namespace, tt.bspName)
			require.Error(t, err)
			require.Contains(t, err.Error(), tt.expectedError)
			require.Nil(t, result)
		})
	}
}

func TestGatewayController_GetSecretData_ErrorCases(t *testing.T) {
	fakeClient := requireNewFakeClientWithIndexes(t)
	c := NewGatewayController(fakeClient, fake2.NewClientset(), ctrl.Log,
		"envoy-gateway-system", "/foo/bar/uds.sock", "docker.io/envoyproxy/ai-gateway-extproc:latest")

	ctx := context.Background()
	namespace := "test-namespace"

	// Test missing secret.
	result, err := c.getSecretData(ctx, namespace, "missing-secret", "test-key")
	require.Error(t, err)
	require.Contains(t, err.Error(), "secrets \"missing-secret\" not found")
	require.Empty(t, result)
}

func TestGatewayController_annotateGatewayPods(t *testing.T) {
	egNamespace := "envoy-gateway-system"
	gwName, gwNamepsace := "gw", "ns"
	labels := map[string]string{
		egOwningGatewayNameLabel:      gwName,
		egOwningGatewayNamespaceLabel: gwNamepsace,
	}

	fakeClient := requireNewFakeClientWithIndexes(t)
	kube := fake2.NewClientset()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zap.Options{Development: true, Level: zapcore.DebugLevel})))
	const v2Container = "ai-gateway-extproc:v2"
	c := NewGatewayController(fakeClient, kube, ctrl.Log,
		egNamespace, "/foo/bar/uds.sock", v2Container)
	t.Run("pod with extproc", func(t *testing.T) {
		_, err := kube.CoreV1().Pods(egNamespace).Create(t.Context(), &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod1",
				Namespace: egNamespace,
				Labels:    labels,
			},
			Spec: corev1.PodSpec{
				// This indicates that the pod has extproc.
				Containers: []corev1.Container{{Name: mutationNamePrefix + "foo"}},
			},
		}, metav1.CreateOptions{})
		require.NoError(t, err)
		err = c.annotateGatewayPods(t.Context(), &gwapiv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Name: gwName, Namespace: gwNamepsace},
		}, "some-uuid")
		require.NoError(t, err)

		annotated, err := kube.CoreV1().Pods(egNamespace).Get(t.Context(), "pod1", metav1.GetOptions{})
		require.NoError(t, err)
		require.Equal(t, "some-uuid", annotated.Annotations[aigatewayUUIDAnnotationKey])
	})

	t.Run("pod without extproc", func(t *testing.T) {
		_, err := kube.CoreV1().Pods(egNamespace).Create(t.Context(), &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod2",
				Namespace: egNamespace,
				Labels:    labels,
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "foo"}}},
		}, metav1.CreateOptions{})
		require.NoError(t, err)

		// We also need to create a parent deployment for the pod.
		_, err = kube.AppsV1().Deployments(egNamespace).Create(t.Context(), &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "deployment1",
				Namespace: egNamespace,
				Labels:    labels,
			},
			Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{}}},
		}, metav1.CreateOptions{})
		require.NoError(t, err)

		err = c.annotateGatewayPods(t.Context(), &gwapiv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Name: gwName, Namespace: gwNamepsace},
		}, "some-uuid")
		require.NoError(t, err)

		// Check the deployment's pod template has the annotation.
		deployment, err := kube.AppsV1().Deployments(egNamespace).Get(t.Context(), "deployment1", metav1.GetOptions{})
		require.NoError(t, err)
		require.Equal(t, "some-uuid", deployment.Spec.Template.Annotations[aigatewayUUIDAnnotationKey])
	})

	t.Run("pod with extproc but old version", func(t *testing.T) {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod3",
				Namespace: egNamespace,
				Labels:    labels,
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{
				// The old v1 container image is used here to simulate the pod without extproc.
				{Name: extProcContainerName, Image: "ai-gateway-extproc:v1"},
			}},
		}
		_, err := kube.CoreV1().Pods(egNamespace).Create(t.Context(), pod, metav1.CreateOptions{})
		require.NoError(t, err)

		// We also need to create a parent deployment for the pod.
		_, err = kube.AppsV1().Deployments(egNamespace).Create(t.Context(), &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "deployment2",
				Namespace: egNamespace,
				Labels:    labels,
			},
			Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{}}},
		}, metav1.CreateOptions{})
		require.NoError(t, err)

		err = c.annotateGatewayPods(t.Context(), &gwapiv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Name: gwName, Namespace: gwNamepsace},
		}, "some-uuid")
		require.NoError(t, err)

		// Check the deployment's pod template has the annotation.
		deployment, err := kube.AppsV1().Deployments(egNamespace).Get(t.Context(), "deployment1", metav1.GetOptions{})
		require.NoError(t, err)
		require.Equal(t, "some-uuid", deployment.Spec.Template.Annotations[aigatewayUUIDAnnotationKey])

		// Simulate the pod's container image is updated to the new version.
		pod.Spec.Containers[0].Image = v2Container
		_, err = kube.CoreV1().Pods(egNamespace).Update(t.Context(), pod, metav1.UpdateOptions{})
		require.NoError(t, err)

		// Call annotateGatewayPods again but the deployment's pod template should not be updated again.
		err = c.annotateGatewayPods(t.Context(), &gwapiv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Name: gwName, Namespace: gwNamepsace},
		}, "another-uuid")
		require.NoError(t, err)

		deployment, err = kube.AppsV1().Deployments(egNamespace).Get(t.Context(), "deployment1", metav1.GetOptions{})
		require.NoError(t, err)
		require.Equal(t, "some-uuid", deployment.Spec.Template.Annotations[aigatewayUUIDAnnotationKey])
	})
}

func TestGatewayController_annotateDaemonSetGatewayPods(t *testing.T) {
	egNamespace := "envoy-gateway-system"
	gwName, gwNamepsace := "gw", "ns"
	labels := map[string]string{
		egOwningGatewayNameLabel:      gwName,
		egOwningGatewayNamespaceLabel: gwNamepsace,
	}

	fakeClient := requireNewFakeClientWithIndexes(t)
	kube := fake2.NewClientset()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zap.Options{Development: true, Level: zapcore.DebugLevel})))
	const v2Container = "ai-gateway-extproc:v2"
	c := NewGatewayController(fakeClient, kube, ctrl.Log,
		egNamespace, "/foo/bar/uds.sock", v2Container)

	t.Run("pod without extproc", func(t *testing.T) {
		_, err := kube.CoreV1().Pods(egNamespace).Create(t.Context(), &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod2",
				Namespace: egNamespace,
				Labels:    labels,
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "foo"}}},
		}, metav1.CreateOptions{})
		require.NoError(t, err)

		// We also need to create a parent deployment for the pod.
		_, err = kube.AppsV1().DaemonSets(egNamespace).Create(t.Context(), &appsv1.DaemonSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "deployment1",
				Namespace: egNamespace,
				Labels:    labels,
			},
			Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{}}},
		}, metav1.CreateOptions{})
		require.NoError(t, err)

		err = c.annotateGatewayPods(t.Context(), &gwapiv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Name: gwName, Namespace: gwNamepsace},
		}, "some-uuid")
		require.NoError(t, err)

		// Check the deployment's pod template has the annotation.
		deployment, err := kube.AppsV1().DaemonSets(egNamespace).Get(t.Context(), "deployment1", metav1.GetOptions{})
		require.NoError(t, err)
		require.Equal(t, "some-uuid", deployment.Spec.Template.Annotations[aigatewayUUIDAnnotationKey])
	})

	t.Run("pod with extproc but old version", func(t *testing.T) {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod3",
				Namespace: egNamespace,
				Labels:    labels,
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{
				// The old v1 container image is used here to simulate the pod without extproc.
				{Name: extProcContainerName, Image: "ai-gateway-extproc:v1"},
			}},
		}
		_, err := kube.CoreV1().Pods(egNamespace).Create(t.Context(), pod, metav1.CreateOptions{})
		require.NoError(t, err)

		// We also need to create a parent DaemonSet for the pod.
		_, err = kube.AppsV1().DaemonSets(egNamespace).Create(t.Context(), &appsv1.DaemonSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "deployment2",
				Namespace: egNamespace,
				Labels:    labels,
			},
			Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{}}},
		}, metav1.CreateOptions{})
		require.NoError(t, err)

		err = c.annotateGatewayPods(t.Context(), &gwapiv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Name: gwName, Namespace: gwNamepsace},
		}, "some-uuid")
		require.NoError(t, err)

		// Check the deployment's pod template has the annotation.
		deployment, err := kube.AppsV1().DaemonSets(egNamespace).Get(t.Context(), "deployment1", metav1.GetOptions{})
		require.NoError(t, err)
		require.Equal(t, "some-uuid", deployment.Spec.Template.Annotations[aigatewayUUIDAnnotationKey])

		// Simulate the pod's container image is updated to the new version.
		pod.Spec.Containers[0].Image = v2Container
		_, err = kube.CoreV1().Pods(egNamespace).Update(t.Context(), pod, metav1.UpdateOptions{})
		require.NoError(t, err)

		// Call annotateGatewayPods again, but the deployment's pod template should not be updated again.
		err = c.annotateGatewayPods(t.Context(), &gwapiv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Name: gwName, Namespace: gwNamepsace},
		}, "another-uuid")
		require.NoError(t, err)

		deployment, err = kube.AppsV1().DaemonSets(egNamespace).Get(t.Context(), "deployment1", metav1.GetOptions{})
		require.NoError(t, err)
		require.Equal(t, "some-uuid", deployment.Spec.Template.Annotations[aigatewayUUIDAnnotationKey])
	})
}

func Test_schemaToFilterAPI(t *testing.T) {
	for i, tc := range []struct {
		in       aigv1a1.VersionedAPISchema
		expected filterapi.VersionedAPISchema
	}{
		{
			in:       aigv1a1.VersionedAPISchema{Name: aigv1a1.APISchemaOpenAI, Version: ptr.To("v123")},
			expected: filterapi.VersionedAPISchema{Name: filterapi.APISchemaOpenAI, Version: "v123"},
		},
		{
			in:       aigv1a1.VersionedAPISchema{Name: aigv1a1.APISchemaOpenAI},
			expected: filterapi.VersionedAPISchema{Name: filterapi.APISchemaOpenAI, Version: "v1"},
		},
		{
			in:       aigv1a1.VersionedAPISchema{Name: aigv1a1.APISchemaOpenAI, Version: ptr.To("")},
			expected: filterapi.VersionedAPISchema{Name: filterapi.APISchemaOpenAI, Version: "v1"},
		},
		{
			in:       aigv1a1.VersionedAPISchema{Name: aigv1a1.APISchemaAWSBedrock},
			expected: filterapi.VersionedAPISchema{Name: filterapi.APISchemaAWSBedrock},
		},
	} {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			require.Equal(t, tc.expected, schemaToFilterAPI(tc.in))
		})
	}
}
