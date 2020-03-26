package humiocluster

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	humioapi "github.com/humio/cli/api"
	corev1alpha1 "github.com/humio/humio-operator/pkg/apis/core/v1alpha1"
	"github.com/humio/humio-operator/pkg/humio"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	logf "sigs.k8s.io/controller-runtime/pkg/runtime/log"
)

func TestReconcileHumioCluster_Reconcile(t *testing.T) {
	// Set the logger to development mode for verbose logs.
	logf.SetLogger(logf.ZapLogger(true))

	tests := []struct {
		name         string
		humioCluster *corev1alpha1.HumioCluster
		humioClient  *humio.MockClientConfig
	}{
		{
			"test simple cluster reconciliation",
			&corev1alpha1.HumioCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "humiocluster",
					Namespace: "logging",
				},
				Spec: corev1alpha1.HumioClusterSpec{
					Image:                   "humio/humio-core:1.9.1",
					TargetReplicationFactor: 2,
					StoragePartitionsCount:  3,
					DigestPartitionsCount:   3,
					NodeCount:               3,
				},
			},
			humio.NewMocklient(
				humioapi.Cluster{
					Nodes:             buildClusterNodesList(3),
					StoragePartitions: buildStoragePartitionsList(3, 1),
					IngestPartitions:  buildIngestPartitionsList(3, 1),
				}, nil, nil, nil, ""),
		},
		{
			"test large cluster reconciliation",
			&corev1alpha1.HumioCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "humiocluster",
					Namespace: "logging",
				},
				Spec: corev1alpha1.HumioClusterSpec{
					Image:                   "humio/humio-core:1.9.1",
					TargetReplicationFactor: 3,
					StoragePartitionsCount:  72,
					DigestPartitionsCount:   72,
					NodeCount:               18,
				},
			},
			humio.NewMocklient(
				humioapi.Cluster{
					Nodes:             buildClusterNodesList(18),
					StoragePartitions: buildStoragePartitionsList(72, 2),
					IngestPartitions:  buildIngestPartitionsList(72, 2),
				}, nil, nil, nil, ""),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {

			// Objects to track in the fake client.
			objs := []runtime.Object{
				tt.humioCluster,
			}

			// Register operator types with the runtime scheme.
			s := scheme.Scheme
			s.AddKnownTypes(corev1alpha1.SchemeGroupVersion, tt.humioCluster)

			// Create a fake client to mock API calls.
			cl := fake.NewFakeClient(objs...)

			// Start up http server that can send the mock jwt token
			server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
				rw.Write([]byte(`{"token": "sometempjwttoken"}`))
			}))
			defer server.Close()

			// Point the mock client to the fake server
			tt.humioClient.Url = fmt.Sprintf("%s/", server.URL)

			// Create a ReconcileHumioCluster object with the scheme and fake client.
			r := &ReconcileHumioCluster{
				client:      cl,
				humioClient: tt.humioClient,
				scheme:      s,
			}

			// Mock request to simulate Reconcile() being called on an event for a
			// watched resource .
			req := reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      tt.humioCluster.Name,
					Namespace: tt.humioCluster.Namespace,
				},
			}
			res, err := r.Reconcile(req)
			if err != nil {
				t.Errorf("reconcile: (%v)", err)
			}

			updatedHumioCluster := &corev1alpha1.HumioCluster{}
			err = r.client.Get(context.TODO(), req.NamespacedName, updatedHumioCluster)
			if err != nil {
				t.Errorf("get HumioCluster: (%v)", err)
			}
			if updatedHumioCluster.Status.ClusterState != corev1alpha1.HumioClusterStateBoostrapping {
				t.Errorf("expected cluster state to be %s but got %s", corev1alpha1.HumioClusterStateBoostrapping, updatedHumioCluster.Status.ClusterState)
			}

			// Check that the developer password exists as a k8s secret
			secret, err := r.GetSecret(context.TODO(), updatedHumioCluster, serviceAccountSecretName)
			if err != nil {
				t.Errorf("get secret with password: (%v). %+v", err, secret)
			}
			if string(secret.Data["password"]) == "" {
				t.Errorf("password secret %s expected content to not be empty, but it was", serviceAccountSecretName)
			}

			for nodeCount := 1; nodeCount <= tt.humioCluster.Spec.NodeCount; nodeCount++ {
				foundPodList, err := ListPods(cl, updatedHumioCluster)
				if len(foundPodList) != nodeCount {
					t.Errorf("expected list pods to return equal to %d, got %d", nodeCount, len(foundPodList))
				}

				// We must update the IP address because when we attempt to add labels to the pod we validate that they have IP addresses first
				// We also must update the ready condition as the reconciler will wait until all pods are ready before continuing
				err = markPodsAsRunning(cl, foundPodList)
				if err != nil {
					t.Errorf("failed to update pods to prepare for testing the labels: %s", err)
				}

				// Reconcile again so Reconcile() checks pods and updates the HumioCluster resources' Status.
				res, err = r.Reconcile(req)
				if err != nil {
					t.Errorf("reconcile: (%v)", err)
				}
			}

			// Check that we do not create more than expected number of humio pods
			res, err = r.Reconcile(req)
			if err != nil {
				t.Errorf("reconcile: (%v)", err)
			}
			foundPodList, err := ListPods(cl, updatedHumioCluster)
			if len(foundPodList) != tt.humioCluster.Spec.NodeCount {
				t.Errorf("expected list pods to return equal to %d, got %d", tt.humioCluster.Spec.NodeCount, len(foundPodList))
			}

			updatedHumioCluster = &corev1alpha1.HumioCluster{}
			err = r.client.Get(context.TODO(), req.NamespacedName, updatedHumioCluster)
			if err != nil {
				t.Errorf("get HumioCluster: (%v)", err)
			}
			if updatedHumioCluster.Status.ClusterState != corev1alpha1.HumioClusterStateRunning {
				t.Errorf("expected cluster state to be %s but got %s", corev1alpha1.HumioClusterStateRunning, updatedHumioCluster.Status.ClusterState)
			}

			// Check that the service exists
			service, err := r.GetService(context.TODO(), updatedHumioCluster)
			if err != nil {
				t.Errorf("get service: (%v). %+v", err, service)
			}

			// Check that the persistent token exists as a k8s secret

			token, err := r.GetSecret(context.TODO(), updatedHumioCluster, serviceTokenSecretName)
			if err != nil {
				t.Errorf("get secret with api token: (%v). %+v", err, token)
			}
			if string(token.Data["token"]) != "mocktoken" {
				t.Errorf("api token secret %s expected content \"%+v\", but got \"%+v\"", serviceTokenSecretName, "mocktoken", string(token.Data["token"]))
			}

			// Reconcile again so Reconcile() checks pods and updates the HumioCluster resources' Status.
			res, err = r.Reconcile(req)
			if err != nil {
				t.Errorf("reconcile: (%v)", err)
			}
			if res != (reconcile.Result{Requeue: true, RequeueAfter: time.Second * 30}) {
				t.Error("reconcile finished, requeueing the resource after 30 seconds")
			}

			// Check that the persistent token
			tokenInUse, err := r.humioClient.ApiToken()
			if tokenInUse != "mocktoken" {
				t.Errorf("expected api token in use to be \"%+v\", but got \"%+v\"", "mocktoken", tokenInUse)
			}

			// Get the updated HumioCluster to update it with the partitions
			err = r.client.Get(context.TODO(), req.NamespacedName, updatedHumioCluster)
			if err != nil {
				t.Errorf("get HumioCluster: (%v)", err)
			}

			// Check that the partitions are balanced
			clusterController := humio.NewClusterController(tt.humioClient)
			if b, err := clusterController.AreStoragePartitionsBalanced(updatedHumioCluster); !b || err != nil {
				t.Errorf("expected storage partitions to be balanced. got %v, err %s", b, err)
			}
			if b, err := clusterController.AreIngestPartitionsBalanced(updatedHumioCluster); !b || err != nil {
				t.Errorf("expected ingest partitions to be balanced. got %v, err %s", b, err)
			}

			foundPodList, err = ListPods(cl, updatedHumioCluster)
			if err != nil {
				t.Errorf("could not list pods to validate their content: %v", err)
			}

			if len(foundPodList) != tt.humioCluster.Spec.NodeCount {
				t.Errorf("expected list pods to return equal to %d, got %d", tt.humioCluster.Spec.NodeCount, len(foundPodList))
			}

			// Ensure that we add node_id label to all pods
			for _, pod := range foundPodList {
				if !labelListContainsLabel(pod.GetLabels(), "node_id") {
					t.Errorf("expected pod %s to have label node_id", pod.Name)
				}
			}
		})
	}
}

func TestReconcileHumioCluster_Reconcile_update_humio_image(t *testing.T) {
	// Set the logger to development mode for verbose logs.
	logf.SetLogger(logf.ZapLogger(true))

	tests := []struct {
		name          string
		humioCluster  *corev1alpha1.HumioCluster
		humioClient   *humio.MockClientConfig
		imageToUpdate string
	}{
		{
			"test simple cluster humio image update",
			&corev1alpha1.HumioCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "humiocluster",
					Namespace: "logging",
				},
				Spec: corev1alpha1.HumioClusterSpec{
					Image:                   "humio/humio-core:1.9.1",
					TargetReplicationFactor: 2,
					StoragePartitionsCount:  3,
					DigestPartitionsCount:   3,
					NodeCount:               3,
				},
			},
			humio.NewMocklient(
				humioapi.Cluster{
					Nodes:             buildClusterNodesList(3),
					StoragePartitions: buildStoragePartitionsList(3, 1),
					IngestPartitions:  buildIngestPartitionsList(3, 1),
				}, nil, nil, nil, ""),
			"humio/humio-core:1.9.2",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {

			// Objects to track in the fake client.
			objs := []runtime.Object{
				tt.humioCluster,
			}

			// Register operator types with the runtime scheme.
			s := scheme.Scheme
			s.AddKnownTypes(corev1alpha1.SchemeGroupVersion, tt.humioCluster)

			// Create a fake client to mock API calls.
			cl := fake.NewFakeClient(objs...)

			// Start up http server that can send the mock jwt token
			server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
				rw.Write([]byte(`{"token": "sometempjwttoken"}`))
			}))
			defer server.Close()

			// Point the mock client to the fake server
			tt.humioClient.Url = fmt.Sprintf("%s/", server.URL)

			// Create a ReconcileHumioCluster object with the scheme and fake client.
			r := &ReconcileHumioCluster{
				client:      cl,
				humioClient: tt.humioClient,
				scheme:      s,
			}

			// Mock request to simulate Reconcile() being called on an event for a
			// watched resource .
			req := reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      tt.humioCluster.Name,
					Namespace: tt.humioCluster.Namespace,
				},
			}
			_, err := r.Reconcile(req)
			if err != nil {
				t.Errorf("reconcile: (%v)", err)
			}

			updatedHumioCluster := &corev1alpha1.HumioCluster{}
			err = r.client.Get(context.TODO(), req.NamespacedName, updatedHumioCluster)
			if err != nil {
				t.Errorf("get HumioCluster: (%v)", err)
			}
			if updatedHumioCluster.Status.ClusterState != corev1alpha1.HumioClusterStateBoostrapping {
				t.Errorf("expected cluster state to be %s but got %s", corev1alpha1.HumioClusterStateBoostrapping, updatedHumioCluster.Status.ClusterState)
			}
			tt.humioCluster = updatedHumioCluster

			for nodeCount := 0; nodeCount < tt.humioCluster.Spec.NodeCount; nodeCount++ {
				foundPodList, err := ListPods(cl, tt.humioCluster)
				if len(foundPodList) != nodeCount+1 {
					t.Errorf("expected list pods to return equal to %d, got %d", nodeCount+1, len(foundPodList))
				}

				// We must update the IP address because when we attempt to add labels to the pod we validate that they have IP addresses first
				// We also must update the ready condition as the reconciler will wait until all pods are ready before continuing
				err = markPodsAsRunning(cl, foundPodList)
				if err != nil {
					t.Errorf("failed to update pods to prepare for testing the labels: %s", err)
				}

				// Reconcile again so Reconcile() checks pods and updates the HumioCluster resources' Status.
				_, err = r.Reconcile(req)
				if err != nil {
					t.Errorf("reconcile: (%v)", err)
				}
			}

			// Test that we're in a Running state
			updatedHumioCluster = &corev1alpha1.HumioCluster{}
			err = r.client.Get(context.TODO(), req.NamespacedName, updatedHumioCluster)
			if err != nil {
				t.Errorf("get HumioCluster: (%v)", err)
			}
			if updatedHumioCluster.Status.ClusterState != corev1alpha1.HumioClusterStateRunning {
				t.Errorf("expected cluster state to be %s but got %s", corev1alpha1.HumioClusterStateRunning, updatedHumioCluster.Status.ClusterState)
			}

			// Update humio image
			updatedHumioCluster.Spec.Image = tt.imageToUpdate
			cl.Update(context.TODO(), updatedHumioCluster)

			for nodeCount := 0; nodeCount < tt.humioCluster.Spec.NodeCount; nodeCount++ {
				res, err := r.Reconcile(req)
				if err != nil {
					t.Errorf("reconcile: (%v)", err)
				}
				if res != (reconcile.Result{Requeue: true}) {
					t.Errorf("reconcile did not match expected %v", res)
				}
			}

			// Ensure all the pods are shut down to prep for the image update
			foundPodList, err := ListPods(cl, tt.humioCluster)
			if err != nil {
				t.Errorf("failed to list pods: %s", err)
			}
			if len(foundPodList) != 0 {
				t.Errorf("expected list pods to return equal to %d, got %d", 0, len(foundPodList))
			}

			// Simulate the reconcile being run again for each node so they all are started
			for nodeCount := 0; nodeCount < tt.humioCluster.Spec.NodeCount; nodeCount++ {
				res, err := r.Reconcile(req)
				if err != nil {
					t.Errorf("reconcile: (%v)", err)
				}
				if res != (reconcile.Result{Requeue: true}) {
					t.Errorf("reconcile did not match expected %v", res)
				}
			}

			foundPodList, err = ListPods(cl, updatedHumioCluster)
			if err != nil {
				t.Errorf("failed to list pods: %s", err)
			}
			if len(foundPodList) != tt.humioCluster.Spec.NodeCount {
				t.Errorf("expected list pods to return equal to %d, got %d", tt.humioCluster.Spec.NodeCount, len(foundPodList))
			}
		})
	}
}

func markPodsAsRunning(client client.Client, pods []corev1.Pod) error {
	for nodeID, pod := range pods {
		pod.Status.PodIP = fmt.Sprintf("192.168.0.%d", nodeID)
		pod.Status.Conditions = []corev1.PodCondition{
			corev1.PodCondition{
				Type:   corev1.PodConditionType("Ready"),
				Status: corev1.ConditionTrue,
			},
		}
		err := client.Status().Update(context.TODO(), &pod)
		if err != nil {
			return fmt.Errorf("failed to update pods to prepare for testing the labels: %s", err)
		}
	}
	return nil
}

func buildStoragePartitionsList(numberOfPartitions int, nodesPerPartition int) []humioapi.StoragePartition {
	var storagePartitions []humioapi.StoragePartition

	for p := 1; p <= numberOfPartitions; p++ {
		var nodeIds []int
		for n := 0; n < nodesPerPartition; n++ {
			nodeIds = append(nodeIds, n)
		}
		storagePartition := humioapi.StoragePartition{Id: p, NodeIds: nodeIds}
		storagePartitions = append(storagePartitions, storagePartition)
	}
	return storagePartitions
}

func buildIngestPartitionsList(numberOfPartitions int, nodesPerPartition int) []humioapi.IngestPartition {
	var ingestPartitions []humioapi.IngestPartition

	for p := 1; p <= numberOfPartitions; p++ {
		var nodeIds []int
		for n := 0; n < nodesPerPartition; n++ {
			nodeIds = append(nodeIds, n)
		}
		ingestPartition := humioapi.IngestPartition{Id: p, NodeIds: nodeIds}
		ingestPartitions = append(ingestPartitions, ingestPartition)
	}
	return ingestPartitions
}

func buildClusterNodesList(numberOfNodes int) []humioapi.ClusterNode {
	clusterNodes := []humioapi.ClusterNode{}
	for n := 0; n < numberOfNodes; n++ {
		clusterNode := humioapi.ClusterNode{
			Uri:         fmt.Sprintf("http://192.168.0.%d:8080", n),
			Id:          n,
			IsAvailable: true,
		}
		clusterNodes = append(clusterNodes, clusterNode)
	}
	return clusterNodes
}
