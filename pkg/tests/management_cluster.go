// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package tests

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	cabpt "github.com/talos-systems/cluster-api-bootstrap-provider-talos/api/v1alpha3"
	cacpt "github.com/talos-systems/cluster-api-control-plane-provider-talos/api/v1alpha3"
	"github.com/talos-systems/go-retry/retry"
	sidero "github.com/talos-systems/sidero/app/cluster-api-provider-sidero/api/v1alpha3"
	talosclusterapi "github.com/talos-systems/talos/pkg/machinery/api/cluster"
	talosclient "github.com/talos-systems/talos/pkg/machinery/client"
	clientconfig "github.com/talos-systems/talos/pkg/machinery/client/config"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/restmapper"
	"sigs.k8s.io/cluster-api/api/v1alpha3"
	capiclient "sigs.k8s.io/cluster-api/cmd/clusterctl/client"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/talos-systems/sfyra/pkg/loadbalancer"
)

const (
	managementClusterName   = "management-cluster"
	managementClusterLBPort = 10000
)

// TestManagementCluster deploys the management cluster via CAPI.
func (suite *TestSuite) TestManagementCluster(t *testing.T) {
	config, err := suite.clusterAPI.GetRestConfig(suite.ctx)
	require.NoError(t, err)

	metalClient, err := suite.clusterAPI.GetMetalClient(suite.ctx)
	require.NoError(t, err)

	capiClient := suite.clusterAPI.GetClusterAPIClient()

	kubeconfig, err := suite.clusterAPI.GetKubeconfig(suite.ctx)
	require.NoError(t, err)

	nodeCount := int64(1)

	lb, err := loadbalancer.NewControlPlane(metalClient, suite.bootstrapCluster.GatewayIP(), managementClusterLBPort, "default", managementClusterName, suite.bootstrapCluster.Nodes())
	require.NoError(t, err)

	defer lb.Close() //nolint: errcheck

	os.Setenv("CONTROL_PLANE_ENDPOINT", "localhost")        //nolint: errcheck
	os.Setenv("CONTROL_PLANE_SERVERCLASS", serverClassName) //nolint: errcheck
	os.Setenv("WORKER_SERVERCLASS", serverClassName)        //nolint: errcheck
	// TODO: make it configurable
	os.Setenv("KUBERNETES_VERSION", "v1.19.0") //nolint: errcheck

	templateOptions := capiclient.GetClusterTemplateOptions{
		Kubeconfig:               kubeconfig,
		ClusterName:              managementClusterName,
		ControlPlaneMachineCount: &nodeCount,
		WorkerMachineCount:       &nodeCount,
	}

	template, err := capiClient.GetClusterTemplate(templateOptions)
	require.NoError(t, err)

	dc, err := discovery.NewDiscoveryClientForConfig(config)
	require.NoError(t, err)

	mapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(dc))

	dyn, err := dynamic.NewForConfig(config)
	require.NoError(t, err)

	for _, obj := range template.Objs() {
		var mapping *meta.RESTMapping

		mapping, err = mapper.RESTMapping(obj.GroupVersionKind().GroupKind(), obj.GroupVersionKind().Version)
		require.NoError(t, err)

		var dr dynamic.ResourceInterface
		if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
			// namespaced resources should specify the namespace
			dr = dyn.Resource(mapping.Resource).Namespace(obj.GetNamespace())
		} else {
			// for cluster-wide resources
			dr = dyn.Resource(mapping.Resource)
		}

		if obj.GroupVersionKind().Kind == "MetalCluster" {
			host, portStr, _ := net.SplitHostPort(lb.GetEndpoint()) //nolint: errcheck
			port, _ := strconv.Atoi(portStr)                        //nolint: errcheck

			require.NoError(t, unstructured.SetNestedMap(obj.Object, map[string]interface{}{
				"host": host,
				"port": float64(port),
			}, "spec", "controlPlaneEndpoint"))
		}

		var data []byte

		data, err = obj.MarshalJSON()
		require.NoError(t, err)

		t.Logf("applying %s", string(data))

		obj := obj

		_, err = dr.Create(suite.ctx, &obj, metav1.CreateOptions{})
		if err != nil {
			if apierrors.IsAlreadyExists(err) {
				_, err = dr.Patch(suite.ctx, obj.GetName(), types.ApplyPatchType, data, metav1.PatchOptions{
					FieldManager: "sfyra",
				})
			}
		}

		require.NoError(t, err)
	}

	t.Log("waiting for the cluster to be provisioned")

	require.NoError(t, retry.Constant(10*time.Minute, retry.WithUnits(10*time.Second)).Retry(func() error {
		var cluster v1alpha3.Cluster

		if err = metalClient.Get(suite.ctx, types.NamespacedName{Namespace: "default", Name: managementClusterName}, &cluster); err != nil {
			return retry.UnexpectedError(err)
		}

		ready := false

		for _, cond := range cluster.Status.Conditions {
			if cond.Type == v1alpha3.ReadyCondition && cond.Status == corev1.ConditionTrue {
				ready = true

				break
			}
		}

		if !ready {
			return retry.ExpectedError(fmt.Errorf("cluster is not ready"))
		}

		return nil
	}))

	t.Log("verifying cluster health")

	var (
		cluster      v1alpha3.Cluster
		controlPlane cacpt.TalosControlPlane
		machines     v1alpha3.MachineList
		talosConfig  cabpt.TalosConfig
	)

	require.NoError(t, metalClient.Get(suite.ctx, types.NamespacedName{Namespace: "default", Name: managementClusterName}, &cluster))
	require.NoError(t, metalClient.Get(suite.ctx, types.NamespacedName{Namespace: cluster.Spec.ControlPlaneRef.Namespace, Name: cluster.Spec.ControlPlaneRef.Name}, &controlPlane))

	labelSelector, err := labels.Parse(controlPlane.Status.Selector)
	require.NoError(t, err)

	require.NoError(t, metalClient.List(suite.ctx, &machines, client.MatchingLabelsSelector{Selector: labelSelector}))

	require.GreaterOrEqual(t, len(machines.Items), 1)

	configRef := machines.Items[0].Spec.Bootstrap.ConfigRef

	require.NoError(t, metalClient.Get(suite.ctx, types.NamespacedName{Namespace: configRef.Namespace, Name: configRef.Name}, &talosConfig))

	var clientConfig *clientconfig.Config
	clientConfig, err = clientconfig.FromString(talosConfig.Status.TalosConfig)

	require.NoError(t, err)

	// TODO: endpoints in talosconfig should be filled by Sidero
	var metalMachine sidero.MetalMachine

	require.NoError(t, metalClient.Get(suite.ctx,
		types.NamespacedName{Namespace: machines.Items[0].Spec.InfrastructureRef.Namespace, Name: machines.Items[0].Spec.InfrastructureRef.Name},
		&metalMachine))

	nodeUUID := metalMachine.Spec.ServerRef.Name

	for _, node := range suite.bootstrapCluster.Nodes() {
		if node.UUID.String() == nodeUUID {
			clientConfig.Contexts[clientConfig.Context].Endpoints = append(clientConfig.Contexts[clientConfig.Context].Endpoints, node.PrivateIP.String())
		}
	}

	var talosClient *talosclient.Client

	talosClient, err = talosclient.New(suite.ctx, talosclient.WithConfig(clientConfig))
	require.NoError(t, err)

	require.NoError(t, talosHealth(suite.ctx, talosClient, clientConfig.Contexts[clientConfig.Context].Endpoints))
}

func talosHealth(ctx context.Context, talosClient *talosclient.Client, nodes []string) error {
	resp, err := talosClient.ClusterHealthCheck(talosclient.WithNodes(ctx, nodes...), 3*time.Minute, &talosclusterapi.ClusterInfo{})
	if err != nil {
		return err
	}

	if err := resp.CloseSend(); err != nil {
		return err
	}

	for {
		msg, err := resp.Recv()
		if err != nil {
			if err == io.EOF || status.Code(err) == codes.Canceled {
				return nil
			}

			return err
		}

		if msg.GetMetadata().GetError() != "" {
			return fmt.Errorf("healthcheck error: %s", msg.GetMetadata().GetError())
		}

		fmt.Fprintln(os.Stderr, msg.GetMessage())
	}
}
