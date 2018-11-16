package installer

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	operatorv1alpha1 "github.com/openshift/api/operator/v1alpha1"
	"github.com/openshift/library-go/pkg/operator/staticpod/controller/common"
)

func TestNewNodeStateForInstallInProgress(t *testing.T) {
	kubeClient := fake.NewSimpleClientset()

	var (
		installerPod   *v1.Pod
		createPodCount int
	)

	kubeClient.PrependReactor("create", "pods", func(action ktesting.Action) (handled bool, ret runtime.Object, err error) {
		installerPod = action.(ktesting.CreateAction).GetObject().(*v1.Pod)
		createPodCount += 1
		return false, nil, nil
	})

	kubeInformers := informers.NewSharedInformerFactoryWithOptions(kubeClient, 1*time.Minute, informers.WithNamespace("test"))
	fakeStaticPodOperatorClient := common.NewFakeStaticPodOperatorClient(
		&operatorv1alpha1.OperatorSpec{
			ManagementState: operatorv1alpha1.Managed,
			Version:         "3.11.1",
		},
		&operatorv1alpha1.OperatorStatus{},
		&operatorv1alpha1.StaticPodOperatorStatus{
			LatestAvailableDeploymentGeneration: 1,
			NodeStatuses: []operatorv1alpha1.NodeStatus{
				{
					NodeName:                    "test-node-1",
					CurrentDeploymentGeneration: 0,
					TargetDeploymentGeneration:  0,
				},
			},
		},
		nil,
	)

	c := NewInstallerController(
		"test", "test-pod",
		[]string{"test-config"},
		[]string{"test-secret"},
		[]string{"/bin/true"},
		kubeInformers,
		fakeStaticPodOperatorClient,
		kubeClient,
	)
	c.installerPodImageFn = func() string { return "docker.io/foo/bar" }

	if err := c.sync(); err != nil {
		t.Fatal(err)
	}

	if installerPod == nil {
		t.Fatalf("expected to create installer pod")
	}

	_, currStatus, _, _ := fakeStaticPodOperatorClient.Get()
	currStatus.NodeStatuses[0].TargetDeploymentGeneration = 1
	fakeStaticPodOperatorClient.UpdateStatus("1", currStatus)

	if err := c.sync(); err != nil {
		t.Fatal(err)
	}

	if createPodCount != 1 {
		t.Fatalf("was not expecting to create new installer pod")
	}

	t.Log("installer succeeded")
	installerPod.Status.Phase = v1.PodSucceeded
	staticPod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod-test-node-1",
			Namespace: "test",
			Labels:    map[string]string{"deployment-id": "1"},
		},
		Spec: v1.PodSpec{},
		Status: v1.PodStatus{
			Conditions: []v1.PodCondition{
				{
					Status: v1.ConditionFalse,
					Type:   v1.PodReady,
				},
			},
			Phase: v1.PodRunning,
		},
	}
	kubeClient.PrependReactor("get", "pods", getPodsReactor(installerPod))

	if err := c.sync(); err != nil {
		t.Fatal(err)
	}

	_, currStatus, _, _ = fakeStaticPodOperatorClient.Get()
	if generation := currStatus.NodeStatuses[0].CurrentDeploymentGeneration; generation != 0 {
		t.Errorf("expected current deployment generation for node to be 0, got %d", generation)
	}

	t.Log("static pod launched, but is not ready")
	kubeClient.PrependReactor("get", "pods", getPodsReactor(staticPod))

	if err := c.sync(); err != nil {
		t.Fatal(err)
	}

	_, currStatus, _, _ = fakeStaticPodOperatorClient.Get()
	if generation := currStatus.NodeStatuses[0].CurrentDeploymentGeneration; generation != 0 {
		t.Errorf("expected current deployment generation for node to be 0, got %d", generation)
	}

	t.Log("static pod is ready")
	staticPod.Status.Conditions[0].Status = v1.ConditionTrue

	if err := c.sync(); err != nil {
		t.Fatal(err)
	}

	_, currStatus, _, _ = fakeStaticPodOperatorClient.Get()
	if generation := currStatus.NodeStatuses[0].CurrentDeploymentGeneration; generation != 1 {
		t.Errorf("expected current deployment generation for node to be 1, got %d", generation)
	}

	_, currStatus, _, _ = fakeStaticPodOperatorClient.Get()
	currStatus.LatestAvailableDeploymentGeneration = 2
	currStatus.NodeStatuses[0].TargetDeploymentGeneration = 2
	currStatus.NodeStatuses[0].CurrentDeploymentGeneration = 1
	fakeStaticPodOperatorClient.UpdateStatus("1", currStatus)

	installerPod.Name = "installer-2-test-node-1"
	installerPod.Status.Phase = v1.PodFailed
	installerPod.Status.ContainerStatuses = []v1.ContainerStatus{
		{
			Name: "installer",
			State: v1.ContainerState{
				Terminated: &v1.ContainerStateTerminated{Message: "fake death"},
			},
		},
	}
	if err := c.sync(); err != nil {
		t.Fatal(err)
	}

	_, currStatus, _, _ = fakeStaticPodOperatorClient.Get()
	if generation := currStatus.NodeStatuses[0].LastFailedDeploymentGeneration; generation != 2 {
		t.Errorf("expected last failed deployment generation for node to be 2, got %d", generation)
	}

	if errors := currStatus.NodeStatuses[0].LastFailedDeploymentErrors; len(errors) > 0 {
		if errors[0] != "installer: fake death" {
			t.Errorf("expected the error to be set to 'fake death', got %#v", errors)
		}
	} else {
		t.Errorf("expected errors to be not empty")
	}
}

func getPodsReactor(pods ...*v1.Pod) ktesting.ReactionFunc {
	return func(action ktesting.Action) (handled bool, ret runtime.Object, err error) {
		podName := action.(ktesting.GetAction).GetName()
		for _, p := range pods {
			if p.Namespace == action.GetNamespace() && p.Name == podName {
				return true, p, nil
			}
		}
		return false, nil, nil
	}
}

func TestCreateInstallerPod(t *testing.T) {
	kubeClient := fake.NewSimpleClientset()

	var installerPod *v1.Pod
	kubeClient.PrependReactor("create", "pods", func(action ktesting.Action) (handled bool, ret runtime.Object, err error) {
		installerPod = action.(ktesting.CreateAction).GetObject().(*v1.Pod)
		return false, nil, nil
	})
	kubeInformers := informers.NewSharedInformerFactoryWithOptions(kubeClient, 1*time.Minute, informers.WithNamespace("test"))

	fakeStaticPodOperatorClient := common.NewFakeStaticPodOperatorClient(
		&operatorv1alpha1.OperatorSpec{
			ManagementState: operatorv1alpha1.Managed,
			Version:         "3.11.1",
		},
		&operatorv1alpha1.OperatorStatus{},
		&operatorv1alpha1.StaticPodOperatorStatus{
			LatestAvailableDeploymentGeneration: 1,
			NodeStatuses: []operatorv1alpha1.NodeStatus{
				{
					NodeName:                    "test-node-1",
					CurrentDeploymentGeneration: 0,
					TargetDeploymentGeneration:  0,
				},
			},
		},
		nil,
	)

	c := NewInstallerController(
		"test", "test-pod",
		[]string{"test-config"},
		[]string{"test-secret"},
		[]string{"/bin/true"},
		kubeInformers,
		fakeStaticPodOperatorClient,
		kubeClient,
	)
	c.installerPodImageFn = func() string { return "docker.io/foo/bar" }
	if err := c.sync(); err != nil {
		t.Fatal(err)
	}

	if installerPod == nil {
		t.Fatalf("expected to create installer pod")
	}

	if installerPod.Spec.Containers[0].Image != "docker.io/foo/bar" {
		t.Fatalf("expected docker.io/foo/bar image, got %q", installerPod.Spec.Containers[0].Image)
	}

	if installerPod.Spec.Containers[0].Command[0] != "/bin/true" {
		t.Fatalf("expected /bin/true as a command, got %q", installerPod.Spec.Containers[0].Command[0])
	}

	if installerPod.Name != "installer-1-test-node-1" {
		t.Fatalf("expected name installer-1-test-node-1, got %q", installerPod.Name)
	}

	if installerPod.Namespace != "test" {
		t.Fatalf("expected test namespace, got %q", installerPod.Namespace)
	}

	expectedArgs := []string{
		"-v=0",
		"--deployment-id=1",
		"--namespace=test",
		"--pod=test-config",
		"--resource-dir=/etc/kubernetes/static-pod-resources",
		"--pod-manifest-dir=/etc/kubernetes/manifests",
		"--configmaps=test-config",
		"--secrets=test-secret",
	}

	if len(expectedArgs) != len(installerPod.Spec.Containers[0].Args) {
		t.Fatalf("expected arguments does not match container arguments: %#v != %#v", expectedArgs, installerPod.Spec.Containers[0].Args)
	}

	for i, v := range installerPod.Spec.Containers[0].Args {
		if expectedArgs[i] != v {
			t.Errorf("arg[%d] expected %q, got %q", i, expectedArgs[i], v)
		}
	}
}

func TestCreateInstallerPodMultiNode(t *testing.T) {
	newStaticPod := func(name string, id int, phase v1.PodPhase, ready bool) *v1.Pod {
		condStatus := v1.ConditionTrue
		if !ready {
			condStatus = v1.ConditionFalse
		}
		return &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: "test",
				Labels:    map[string]string{"deployment-id": strconv.Itoa(id)},
			},
			Spec: v1.PodSpec{},
			Status: v1.PodStatus{
				Conditions: []v1.PodCondition{
					{
						Status: condStatus,
						Type:   v1.PodReady,
					},
				},
				Phase: phase,
			},
		}
	}

	tests := []struct {
		name                                string
		nodeStatuses                        []operatorv1alpha1.NodeStatus
		staticPods                          []*v1.Pod
		latestAvailableDeploymentGeneration int32
		expectedUpgradeOrder                []int
	}{
		{
			name: "three fresh nodes",
			latestAvailableDeploymentGeneration: 1,
			nodeStatuses: []operatorv1alpha1.NodeStatus{
				{
					NodeName: "test-node-0",
				},
				{
					NodeName: "test-node-1",
				},
				{
					NodeName: "test-node-2",
				},
			},
			expectedUpgradeOrder: []int{0, 1, 2},
		},
		{
			name: "three nodes with current deployment, all static pods ready",
			latestAvailableDeploymentGeneration: 2,
			nodeStatuses: []operatorv1alpha1.NodeStatus{
				{
					NodeName:                    "test-node-0",
					CurrentDeploymentGeneration: 1,
				},
				{
					NodeName:                    "test-node-1",
					CurrentDeploymentGeneration: 1,
				},
				{
					NodeName:                    "test-node-2",
					CurrentDeploymentGeneration: 1,
				},
			},
			staticPods: []*v1.Pod{
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-1"), 1, v1.PodRunning, true),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-2"), 1, v1.PodRunning, true),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-3"), 1, v1.PodRunning, true),
			},
			expectedUpgradeOrder: []int{0, 1, 2},
		},
		{
			name: "three nodes with current deployment, second static pods unread",
			latestAvailableDeploymentGeneration: 2,
			nodeStatuses: []operatorv1alpha1.NodeStatus{
				{
					NodeName:                    "test-node-1",
					CurrentDeploymentGeneration: 1,
				},
				{
					NodeName:                    "test-node-2",
					CurrentDeploymentGeneration: 1,
				},
				{
					NodeName:                    "test-node-3",
					CurrentDeploymentGeneration: 1,
				},
			},
			staticPods: []*v1.Pod{
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-1"), 1, v1.PodRunning, true),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-2"), 1, v1.PodRunning, false),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-3"), 1, v1.PodRunning, true),
			},
			expectedUpgradeOrder: []int{1, 0, 2},
		},
		{
			name: "three nodes with current deployment, 2nd & 3rd static pods unread",
			latestAvailableDeploymentGeneration: 2,
			nodeStatuses: []operatorv1alpha1.NodeStatus{
				{
					NodeName:                    "test-node-1",
					CurrentDeploymentGeneration: 1,
				},
				{
					NodeName:                    "test-node-2",
					CurrentDeploymentGeneration: 1,
				},
				{
					NodeName:                    "test-node-3",
					CurrentDeploymentGeneration: 1,
				},
			},
			staticPods: []*v1.Pod{
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-1"), 1, v1.PodRunning, true),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-2"), 1, v1.PodRunning, false),
				newStaticPod(mirrorPodNameForNode("test-pod", "test-node-3"), 1, v1.PodRunning, false),
			},
			expectedUpgradeOrder: []int{1, 2, 0},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			createdInstallerPods := []*v1.Pod{}
			updatedStaticPods := map[string]*v1.Pod{}

			installerNodeAndID := func(installerName string) (string, int) {
				ss := strings.SplitN(strings.TrimPrefix(installerName, "installer-"), "-", 2)
				id, err := strconv.Atoi(ss[0])
				if err != nil {
					t.Fatalf("unexpected id derived from install pod name %q: %v", installerName, err)
				}
				return ss[1], id
			}

			kubeClient := fake.NewSimpleClientset()
			kubeClient.PrependReactor("create", "pods", func(action ktesting.Action) (handled bool, ret runtime.Object, err error) {
				createdPod := action.(ktesting.CreateAction).GetObject().(*v1.Pod)
				// Once the installer pod is created, set its status to succeeded.
				// Note that in reality, this will probably take couple sync cycles to happen, however it is useful to do this fast
				// to rule out timing bugs.
				createdPod.Status.Phase = v1.PodSucceeded
				createdInstallerPods = append(createdInstallerPods, createdPod)

				nodeName, id := installerNodeAndID(createdPod.Name)
				staticPodName := mirrorPodNameForNode("test-pod", nodeName)

				updatedStaticPods[staticPodName] = newStaticPod(staticPodName, id, v1.PodRunning, true)

				return false, nil, nil
			})

			// When newNodeStateForInstallInProgress ask for pod, give it a pod that already succeeded.
			kubeClient.PrependReactor("get", "pods", func(action ktesting.Action) (handled bool, ret runtime.Object, err error) {
				podName := action.(ktesting.GetAction).GetName()
				for i := len(createdInstallerPods) - 1; i >= 0; i-- {
					pod := createdInstallerPods[i]
					if pod.Name == podName {
						return true, pod, nil
					}
				}
				if pod, exists := updatedStaticPods[podName]; exists {
					if pod == nil {
						return false, nil, nil
					}
					return true, pod, nil
				}
				for _, pod := range test.staticPods {
					if pod.Name == podName {
						return true, pod, nil
					}
				}
				return false, nil, nil
			})

			kubeInformers := informers.NewSharedInformerFactoryWithOptions(kubeClient, 1*time.Minute, informers.WithNamespace("test-"+test.name))
			fakeStaticPodOperatorClient := common.NewFakeStaticPodOperatorClient(
				&operatorv1alpha1.OperatorSpec{
					ManagementState: operatorv1alpha1.Managed,
					Version:         "3.11.1",
				},
				&operatorv1alpha1.OperatorStatus{},
				&operatorv1alpha1.StaticPodOperatorStatus{
					LatestAvailableDeploymentGeneration: test.latestAvailableDeploymentGeneration,
					NodeStatuses:                        test.nodeStatuses,
				},
				nil,
			)

			c := NewInstallerController(
				"test-"+test.name, "test-pod",
				[]string{"test-config"},
				[]string{"test-secret"},
				[]string{"/bin/true"},
				kubeInformers,
				fakeStaticPodOperatorClient,
				kubeClient,
			)
			c.installerPodImageFn = func() string { return "docker.io/foo/bar" }

			// Each node need at least 2 syncs to first create the pod and then acknowledge its existence.
			for i := 1; i <= len(test.nodeStatuses)*2+1; i++ {
				if err := c.sync(); err != nil {
					t.Errorf("%s: failed to execute %d sync: %v", test.name, i, err)
				}
			}

			for i := range test.expectedUpgradeOrder {
				if i >= len(createdInstallerPods) {
					t.Fatalf("expected more installer pod in the node order %v", test.expectedUpgradeOrder[i:])
				}

				nodeName, _ := installerNodeAndID(createdInstallerPods[i].Name)
				if expected, got := test.nodeStatuses[test.expectedUpgradeOrder[i]].NodeName, nodeName; expected != got {
					t.Errorf("expected installer pod number %d to be for node %q, but got %q", i, expected, got)
				}
			}
			if len(test.expectedUpgradeOrder) < len(createdInstallerPods) {
				t.Errorf("too many installer pods created: %#v", createdInstallerPods[len(test.expectedUpgradeOrder):])
			}
		})
	}

}
