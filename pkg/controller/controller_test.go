package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	csi "github.com/awslabs/volume-modifier-for-k8s/pkg/client"
	"github.com/awslabs/volume-modifier-for-k8s/pkg/modifier"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

const (
	namespace = "default"
)

type pvcModifier func(claim *v1.PersistentVolumeClaim)

func newTestController(name string) *modifyController {
	return &modifyController{
		name:                   name,
		annPrefix:              fmt.Sprintf(AnnotationPrefixPattern, name),
		claimQueue:             workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "test"),
		modificationInProgress: make(map[string]struct{}),
	}
}

// waitForModifyCount polls until client.GetModifyCallCount() reaches the
// expected value or the timeout expires.
func waitForModifyCount(t *testing.T, client *csi.FakeClient, expected int, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		if client.GetModifyCallCount() >= expected {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for modify call count %d, got %d", expected, client.GetModifyCallCount())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// waitForQueueDrain polls until the controller's claim queue is empty or the
// timeout expires. Useful for tests that expect 0 modify calls — we wait for
// the queue to drain rather than sleeping a fixed duration.
func waitForQueueDrain(t *testing.T, mc *modifyController, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		if mc.claimQueue.Len() == 0 {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for claim queue to drain, len=%d", mc.claimQueue.Len())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// waitForCacheSync waits for the controller's informer caches to sync, then
// waits for the queue to drain. This is needed for tests that set up a full
// controller with informers.
func waitForCacheSync(t *testing.T, mc *modifyController, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		if mc.pvSynced() && mc.pvcSynced() {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for informer cache sync")
		case <-time.After(10 * time.Millisecond):
		}
	}
	waitForQueueDrain(t, mc, timeout)
}

func setupController(
	t *testing.T,
	driverName string,
	clientReturnsError bool,
	objects ...runtime.Object,
) (*modifyController, *csi.FakeClient) {
	t.Helper()
	client := csi.NewFakeClient(driverName, true, clientReturnsError)

	k8sClient := fake.NewClientset(objects...)
	factory := informers.NewSharedInformerFactory(k8sClient, 0)

	mod, err := modifier.NewFromClient(driverName, client, k8sClient, 0)
	if err != nil {
		t.Fatal(err)
	}

	mc := NewModifyController(
		driverName,
		mod,
		k8sClient,
		0,
		factory,
		workqueue.DefaultControllerRateLimiter(),
		false,
	)

	stopCh := make(chan struct{})
	factory.Start(stopCh)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	go mc.Run(1, ctx)

	t.Cleanup(func() {
		cancel()
		close(stopCh)
	})

	ctrl := mc.(*modifyController)
	waitForCacheSync(t, ctrl, 3*time.Second)
	return ctrl, client
}

func newFakePVC() *v1.PersistentVolumeClaim {
	return &v1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "testPVC",
			Namespace:   namespace,
			UID:         "test",
			Annotations: make(map[string]string),
		},
		Spec: v1.PersistentVolumeClaimSpec{
			Resources: v1.VolumeResourceRequirements{
				Requests: map[v1.ResourceName]resource.Quantity{
					v1.ResourceStorage: *resource.NewQuantity(10*1024*1024*1024, resource.BinarySI),
				},
			},
			VolumeName: "testPV",
		},
		Status: v1.PersistentVolumeClaimStatus{
			Phase: v1.ClaimBound,
			Capacity: map[v1.ResourceName]resource.Quantity{
				v1.ResourceStorage: *resource.NewQuantity(10*1024*1024*1024, resource.BinarySI),
			},
		},
	}
}

func newFakePendingPVCAnnotated() *v1.PersistentVolumeClaim {
	return &v1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "testPVC",
			Namespace:   namespace,
			UID:         "test",
			Annotations: map[string]string{"ebs.csi.aws.com/iops": "5000"},
		},
		Spec: v1.PersistentVolumeClaimSpec{
			Resources: v1.VolumeResourceRequirements{
				Requests: map[v1.ResourceName]resource.Quantity{
					v1.ResourceStorage: *resource.NewQuantity(10*1024*1024*1024, resource.BinarySI),
				},
			},
		},
		Status: v1.PersistentVolumeClaimStatus{
			Phase: v1.ClaimPending,
			Capacity: map[v1.ResourceName]resource.Quantity{
				v1.ResourceStorage: *resource.NewQuantity(10*1024*1024*1024, resource.BinarySI),
			},
		},
	}
}

func newFakePV(pvcName, pvcNamespace string, pvcUID types.UID) *v1.PersistentVolume {
	pv := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "testPV",
			Annotations: make(map[string]string),
		},
		Spec: v1.PersistentVolumeSpec{
			Capacity: map[v1.ResourceName]resource.Quantity{
				v1.ResourceStorage: *resource.NewQuantity(10*1024*1024*1024, resource.BinarySI),
			},
			PersistentVolumeSource: v1.PersistentVolumeSource{
				CSI: &v1.CSIPersistentVolumeSource{
					Driver:       "test",
					VolumeHandle: "vol-123243434",
				},
			},
		},
	}
	if pvcName != "" {
		pv.Spec.ClaimRef = &v1.ObjectReference{
			Namespace: pvcNamespace,
			Name:      pvcName,
			UID:       pvcUID,
		}
		pv.Status.Phase = v1.VolumeBound
	}
	return pv
}

func newTestPVC(name, ns string, annotations map[string]string) *v1.PersistentVolumeClaim {
	return &v1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   ns,
			UID:         "test-uid",
			Annotations: annotations,
		},
		Spec: v1.PersistentVolumeClaimSpec{
			VolumeName: "testPV",
			Resources: v1.VolumeResourceRequirements{
				Requests: map[v1.ResourceName]resource.Quantity{
					v1.ResourceStorage: resource.MustParse("10Gi"),
				},
			},
		},
		Status: v1.PersistentVolumeClaimStatus{
			Phase: v1.ClaimBound,
			Capacity: map[v1.ResourceName]resource.Quantity{
				v1.ResourceStorage: resource.MustParse("10Gi"),
			},
		},
	}
}

func newTestPV(name, pvcName, pvcNamespace string, pvcUID types.UID, driverName string) *v1.PersistentVolume {
	pv := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Annotations: make(map[string]string),
		},
		Spec: v1.PersistentVolumeSpec{
			Capacity: map[v1.ResourceName]resource.Quantity{
				v1.ResourceStorage: resource.MustParse("10Gi"),
			},
			PersistentVolumeSource: v1.PersistentVolumeSource{
				CSI: &v1.CSIPersistentVolumeSource{
					Driver:       driverName,
					VolumeHandle: "vol-abc123",
				},
			},
		},
	}
	if pvcName != "" {
		pv.Spec.ClaimRef = &v1.ObjectReference{
			Namespace: pvcNamespace,
			Name:      pvcName,
			UID:       pvcUID,
		}
		pv.Status.Phase = v1.VolumeBound
	}
	return pv
}

func verifyNoAnnotationsOnPV(ann map[string]string, driverName string) error {
	for k, v := range ann {
		if strings.HasPrefix(k, driverName) {
			return fmt.Errorf("found annotation on PV: %s (value: %s)", k, v)
		}
	}
	return nil
}

func verifyAnnotationsOnPV(updatedAnnotations, expectedAnnotations map[string]string) error {
	for k, v := range expectedAnnotations {
		if updatedAnnotations[k] != v {
			return fmt.Errorf("unexpected annotation on PV: %s (value : %s)", k, v)
		}
	}
	// Begin Datadog patch
	// We can't check the annotation in the loop before because it is not present on the PVC
	if _, ok := updatedAnnotations["ebs.csi.aws.com/diskLastModificationTime"]; !ok {
		return fmt.Errorf("missing diskLastModificationTime annotation on PV (got : %s)", updatedAnnotations)
	}
	// End Datadog patch
	return nil
}

type fakeErrorStore struct {
	cache.Store
}

func (f *fakeErrorStore) GetByKey(key string) (interface{}, bool, error) {
	return nil, false, fmt.Errorf("simulated store error")
}

func (f *fakeErrorStore) List() []interface{} { return nil }

type fakeWrongTypeStore struct {
	cache.Store
}

func (f *fakeWrongTypeStore) GetByKey(key string) (interface{}, bool, error) {
	return "not-a-pvc", true, nil
}

func (f *fakeWrongTypeStore) List() []interface{} { return nil }

func TestControllerRun(t *testing.T) {
	testCases := []struct {
		name                          string
		driverName                    string
		clientReturnsError            bool
		pvc                           *v1.PersistentVolumeClaim
		pv                            *v1.PersistentVolume
		additionalPVCAnnotations      map[string]string
		expectedModifyVolumeCallCount int
		updatedCapacity               string
		expectSuccessfulModification  bool
		pvcModification               pvcModifier
	}{
		{
			name:       "volume modification succeeds after updating annotation",
			driverName: "ebs.csi.aws.com",
			pvc:        newFakePVC(),
			pv:         newFakePV("testPVC", namespace, "test"),
			additionalPVCAnnotations: map[string]string{
				"ebs.csi.aws.com/volumeType": "io2",
				"ebs.csi.aws.com/iops":       "5000",
			},
			expectedModifyVolumeCallCount: 1,
			expectSuccessfulModification:  true,
		},
		{
			name:       "volume modification fails after client returns error",
			driverName: "ebs.csi.aws.com",
			pvc:        newFakePVC(),
			pv:         newFakePV("testPVC", namespace, "test"),
			additionalPVCAnnotations: map[string]string{
				"ebs.csi.aws.com/volumeType": "io2",
				"ebs.csi.aws.com/iops":       "5000",
			},
			expectedModifyVolumeCallCount: 1,
			clientReturnsError:            true,
			expectSuccessfulModification:  false,
		},
		{
			name:                          "no volume modification after PVC resync",
			driverName:                    "ebs.csi.aws.com",
			pvc:                           newFakePVC(),
			pv:                            newFakePV("testPVC", namespace, "test"),
			expectedModifyVolumeCallCount: 0,
			expectSuccessfulModification:  false,
		},
		{
			name:                          "no volume modification after non-annotation PVC update",
			driverName:                    "ebs.csi.aws.com",
			pvc:                           newFakePVC(),
			pv:                            newFakePV("testPVC", namespace, "test"),
			expectedModifyVolumeCallCount: 0,
			updatedCapacity:               "35Gi",
			expectSuccessfulModification:  false,
		},
		{
			name:       "volume modification after PV is bound",
			driverName: "ebs.csi.aws.com",
			pvc:        newFakePendingPVCAnnotated(),
			pv:         newFakePV("testPVC", namespace, "test"),
			pvcModification: func(pvc *v1.PersistentVolumeClaim) {
				pvc.ResourceVersion = "new"
				pvc.Status.Phase = v1.ClaimBound
				pvc.Spec.VolumeName = "testPV"
			},
			expectedModifyVolumeCallCount: 1,
			expectSuccessfulModification:  true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			client := csi.NewFakeClient(tc.driverName, true, tc.clientReturnsError)
			driverName, _ := client.GetDriverName(context.TODO())

			var objects []runtime.Object
			if tc.pvc != nil {
				if tc.additionalPVCAnnotations != nil {
					for k, v := range tc.additionalPVCAnnotations {
						tc.pvc.Annotations[k] = v
					}
				}
				if tc.updatedCapacity != "" {
					capacity := resource.MustParse(tc.updatedCapacity)
					tc.pvc.Spec.Resources.Requests[v1.ResourceStorage] = capacity
				}
				objects = append(objects, tc.pvc)
			}
			if tc.pv != nil {
				tc.pv.Spec.PersistentVolumeSource.CSI.Driver = driverName
				objects = append(objects, tc.pv)
			}

			k8sClient := fake.NewClientset(objects...)
			factory := informers.NewSharedInformerFactory(k8sClient, 0)

			modifier, err := modifier.NewFromClient(tc.driverName, client, k8sClient, 0)
			if err != nil {
				t.Fatal(err)
			}

			controller := NewModifyController(
				tc.driverName,
				modifier,
				k8sClient,
				0,
				factory,
				workqueue.DefaultControllerRateLimiter(),
				false,
			)

			stopCh := make(chan struct{})
			factory.Start(stopCh)

			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(func() {
				cancel()
				close(stopCh)
			})

			ctrl := controller.(*modifyController)
			go controller.Run(1, ctx)

			waitForCacheSync(t, ctrl, 3*time.Second)

			if tc.pvcModification != nil {
				tc.pvcModification(tc.pvc)
				_, err = k8sClient.CoreV1().PersistentVolumeClaims(tc.pvc.Namespace).Update(context.TODO(), tc.pvc, metav1.UpdateOptions{})
				if err != nil {
					t.Fatal(err)
				}
			}

			if tc.expectedModifyVolumeCallCount > 0 {
				waitForModifyCount(t, client, tc.expectedModifyVolumeCallCount, 3*time.Second)
			} else {
				waitForCacheSync(t, ctrl, 3*time.Second)
			}

			if client.GetModifyCallCount() != tc.expectedModifyVolumeCallCount {
				t.Fatalf("unexpected modify volume call count: expected %d, got %d", tc.expectedModifyVolumeCallCount, client.GetModifyCallCount())
			}

			if tc.expectSuccessfulModification {
				// Poll for PV annotations — the patch happens asynchronously after the modify call.
				deadline := time.After(3 * time.Second)
				for {
					updatedPV, err := k8sClient.CoreV1().PersistentVolumes().Get(context.TODO(), tc.pv.Name, metav1.GetOptions{})
					if err != nil {
						t.Fatal(err)
					}
					if verifyAnnotationsOnPV(updatedPV.Annotations, tc.additionalPVCAnnotations) == nil {
						break
					}
					select {
					case <-deadline:
						t.Fatalf("timed out waiting for PV annotations: %v", verifyAnnotationsOnPV(updatedPV.Annotations, tc.additionalPVCAnnotations))
					case <-time.After(10 * time.Millisecond):
					}
				}
			} else {
				updatedPV, err := k8sClient.CoreV1().PersistentVolumes().Get(context.TODO(), tc.pv.Name, metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				if err := verifyNoAnnotationsOnPV(updatedPV.Annotations, driverName); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestSyncPVC_EmptyVolumeName(t *testing.T) {
	driverName := "ebs.csi.aws.com"
	pvc := &v1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "unbound-pvc",
			Namespace:   "default",
			UID:         "uid1",
			Annotations: map[string]string{"ebs.csi.aws.com/iops": "5000"},
		},
		Spec: v1.PersistentVolumeClaimSpec{
			VolumeName: "",
			Resources: v1.VolumeResourceRequirements{
				Requests: map[v1.ResourceName]resource.Quantity{
					v1.ResourceStorage: resource.MustParse("10Gi"),
				},
			},
		},
		Status: v1.PersistentVolumeClaimStatus{Phase: v1.ClaimPending},
	}

	_, client := setupController(t, driverName, false, pvc)

	if client.GetModifyCallCount() != 0 {
		t.Fatalf("expected 0 modify calls for unbound PVC, got %d", client.GetModifyCallCount())
	}
}

func TestSyncPVC_PVNotFound(t *testing.T) {
	driverName := "ebs.csi.aws.com"
	pvc := &v1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "orphan-pvc",
			Namespace:   "default",
			UID:         "uid2",
			Annotations: map[string]string{"ebs.csi.aws.com/iops": "5000"},
		},
		Spec: v1.PersistentVolumeClaimSpec{
			VolumeName: "nonexistent-pv",
			Resources: v1.VolumeResourceRequirements{
				Requests: map[v1.ResourceName]resource.Quantity{
					v1.ResourceStorage: resource.MustParse("10Gi"),
				},
			},
		},
		Status: v1.PersistentVolumeClaimStatus{Phase: v1.ClaimBound},
	}

	_, client := setupController(t, driverName, false, pvc)

	if client.GetModifyCallCount() != 0 {
		t.Fatalf("expected 0 modify calls when PV not found, got %d", client.GetModifyCallCount())
	}
}

func TestModifyPVC_PVCHasVAC(t *testing.T) {
	driverName := "ebs.csi.aws.com"
	vacName := "my-vac"

	pvc := newTestPVC("vac-pvc", "default", map[string]string{
		"ebs.csi.aws.com/iops": "5000",
	})
	pvc.Spec.VolumeAttributesClassName = &vacName

	pv := newTestPV("testPV", "vac-pvc", "default", "test-uid", driverName)

	_, client := setupController(t, driverName, false, pvc, pv)

	if client.GetModifyCallCount() != 0 {
		t.Fatalf("expected 0 modify calls when PVC has VAC, got %d", client.GetModifyCallCount())
	}
}

func TestModifyPVC_PVHasVAC(t *testing.T) {
	driverName := "ebs.csi.aws.com"
	vacName := "pv-vac"

	pvc := newTestPVC("pv-vac-pvc", "default", map[string]string{
		"ebs.csi.aws.com/iops": "5000",
	})

	pv := newTestPV("testPV", "pv-vac-pvc", "default", "test-uid", driverName)
	pv.Spec.VolumeAttributesClassName = &vacName

	_, client := setupController(t, driverName, false, pvc, pv)

	if client.GetModifyCallCount() != 0 {
		t.Fatalf("expected 0 modify calls when PV has VAC, got %d", client.GetModifyCallCount())
	}
}

func TestControllerRun_EmptyVACNotBlocked(t *testing.T) {
	driverName := "ebs.csi.aws.com"
	emptyVAC := ""

	pvc := newTestPVC("empty-vac-pvc", "default", map[string]string{
		"ebs.csi.aws.com/iops": "5000",
	})
	pvc.Spec.VolumeAttributesClassName = &emptyVAC

	pv := newTestPV("testPV", "empty-vac-pvc", "default", "test-uid", driverName)

	_, client := setupController(t, driverName, false, pvc, pv)
	waitForModifyCount(t, client, 1, 3*time.Second)

	if client.GetModifyCallCount() != 1 {
		t.Fatalf("expected 1 modify call with empty VAC, got %d", client.GetModifyCallCount())
	}
}

func TestControllerRun_NilVACNotBlocked(t *testing.T) {
	driverName := "ebs.csi.aws.com"

	pvc := newTestPVC("nil-vac-pvc", "default", map[string]string{
		fmt.Sprintf("%s/iops", driverName): "5000",
	})

	pv := newTestPV("testPV", "nil-vac-pvc", "default", "test-uid", driverName)

	_, client := setupController(t, driverName, false, pvc, pv)
	waitForModifyCount(t, client, 1, 3*time.Second)

	if client.GetModifyCallCount() != 1 {
		t.Fatalf("expected 1 modify call with nil VAC, got %d", client.GetModifyCallCount())
	}
}

func TestControllerRun_DeletePVC(t *testing.T) {
	driverName := "ebs.csi.aws.com"
	pvc := newTestPVC("del-test-pvc", "default", map[string]string{
		"ebs.csi.aws.com/iops": "5000",
	})
	pv := newTestPV("testPV", "del-test-pvc", "default", "test-uid", driverName)

	k8sClient := fake.NewClientset(pvc, pv)
	factory := informers.NewSharedInformerFactory(k8sClient, 0)
	client := csi.NewFakeClient(driverName, true, false)

	mod, err := modifier.NewFromClient(driverName, client, k8sClient, 0)
	if err != nil {
		t.Fatal(err)
	}

	mc := NewModifyController(driverName, mod, k8sClient, 0, factory,
		workqueue.DefaultControllerRateLimiter(), false)

	stopCh := make(chan struct{})
	factory.Start(stopCh)
	defer close(stopCh)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ctrl := mc.(*modifyController)
	go mc.Run(1, ctx)

	waitForModifyCount(t, client, 1, 3*time.Second)

	err = k8sClient.CoreV1().PersistentVolumeClaims("default").Delete(
		context.TODO(), "del-test-pvc", metav1.DeleteOptions{})
	if err != nil {
		t.Fatal(err)
	}

	waitForQueueDrain(t, ctrl, 3*time.Second)

	updatedPV, err := k8sClient.CoreV1().PersistentVolumes().Get(
		context.TODO(), "testPV", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if updatedPV.Annotations["ebs.csi.aws.com/iops"] != "5000" {
		t.Fatalf("expected PV annotation ebs.csi.aws.com/iops=5000, got %q",
			updatedPV.Annotations["ebs.csi.aws.com/iops"])
	}
}

func TestControllerRun_StatusAnnotationFiltered(t *testing.T) {
	driverName := "ebs.csi.aws.com"
	pvc := newTestPVC("status-pvc", "default", map[string]string{
		"ebs.csi.aws.com/iops":              "5000",
		"ebs.csi.aws.com/iops-status":       "pending",
		"ebs.csi.aws.com/volumeType":        "io2",
		"ebs.csi.aws.com/volumeType-status": "done",
	})
	pv := newTestPV("testPV", "status-pvc", "default", "test-uid", driverName)

	_, client := setupController(t, driverName, false, pvc, pv)
	waitForModifyCount(t, client, 1, 3*time.Second)

	if client.GetModifyCallCount() != 1 {
		t.Fatalf("expected 1 modify call, got %d", client.GetModifyCallCount())
	}

	params := client.GetParams()
	if params["iops"] != "5000" {
		t.Errorf("expected param iops=5000, got %q", params["iops"])
	}
	if params["volumeType"] != "io2" {
		t.Errorf("expected param volumeType=io2, got %q", params["volumeType"])
	}
	if _, ok := params["iops-status"]; ok {
		t.Error("status annotation should not be passed as param")
	}
	if _, ok := params["volumeType-status"]; ok {
		t.Error("status annotation should not be passed as param")
	}
}

func TestControllerRun_RetryOnFailure(t *testing.T) {
	driverName := "ebs.csi.aws.com"
	pvc := newTestPVC("retry-pvc", "default", map[string]string{
		"ebs.csi.aws.com/iops": "5000",
	})
	pv := newTestPV("testPV", "retry-pvc", "default", "test-uid", driverName)

	k8sClient := fake.NewClientset(pvc, pv)
	factory := informers.NewSharedInformerFactory(k8sClient, 0)
	client := csi.NewFakeClient(driverName, true, true)

	mod, err := modifier.NewFromClient(driverName, client, k8sClient, 0)
	if err != nil {
		t.Fatal(err)
	}

	mc := NewModifyController(driverName, mod, k8sClient, 0, factory,
		workqueue.DefaultControllerRateLimiter(), true)

	stopCh := make(chan struct{})
	factory.Start(stopCh)
	defer close(stopCh)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go mc.Run(1, ctx)

	waitForModifyCount(t, client, 2, 4*time.Second)

	if client.GetModifyCallCount() < 2 {
		t.Fatalf("expected multiple modify attempts with retry enabled, got %d", client.GetModifyCallCount())
	}

	updatedPV, err := k8sClient.CoreV1().PersistentVolumes().Get(
		context.TODO(), "testPV", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for k := range updatedPV.Annotations {
		if strings.HasPrefix(k, driverName) {
			t.Fatalf("PV should not have driver annotations after failed modifications, found %s", k)
		}
	}
}

func TestGetObjectKeys(t *testing.T) {
	t.Run("valid PVC object", func(t *testing.T) {
		pvc := &v1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "my-pvc", Namespace: "default"},
		}
		key, err := getObjectKeys(pvc)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if key != "default/my-pvc" {
			t.Fatalf("expected key %q, got %q", "default/my-pvc", key)
		}
	})

	t.Run("DeletedFinalStateUnknown wrapping a PVC", func(t *testing.T) {
		pvc := &v1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "deleted-pvc", Namespace: "kube-system"},
		}
		tombstone := cache.DeletedFinalStateUnknown{
			Key: "kube-system/deleted-pvc",
			Obj: pvc,
		}
		key, err := getObjectKeys(tombstone)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if key != "kube-system/deleted-pvc" {
			t.Fatalf("expected key %q, got %q", "kube-system/deleted-pvc", key)
		}
	})

	t.Run("invalid object returns error", func(t *testing.T) {
		_, err := getObjectKeys("not-a-k8s-object")
		if err == nil {
			t.Fatal("expected error for invalid object, got nil")
		}
	})
}

func TestIsValidAnnotation(t *testing.T) {
	ctrl := newTestController("ebs.csi.aws.com")
	tests := []struct {
		name     string
		ann      string
		expected bool
	}{
		{"valid annotation", "ebs.csi.aws.com/volumeType", true},
		{"valid iops annotation", "ebs.csi.aws.com/iops", true},
		{"status annotation is invalid", "ebs.csi.aws.com/volumeType-status", false},
		{"different driver prefix", "other.driver.io/volumeType", false},
		{"no prefix", "volumeType", false},
		{"empty string", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ctrl.isValidAnnotation(tc.ann); got != tc.expected {
				t.Errorf("isValidAnnotation(%q) = %v, want %v", tc.ann, got, tc.expected)
			}
		})
	}
}

func TestAttributeFromValidAnnotation(t *testing.T) {
	ctrl := newTestController("ebs.csi.aws.com")
	tests := []struct {
		name, ann, expected string
	}{
		{"volumeType", "ebs.csi.aws.com/volumeType", "volumeType"},
		{"iops", "ebs.csi.aws.com/iops", "iops"},
		{"throughput", "ebs.csi.aws.com/throughput", "throughput"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ctrl.attributeFromValidAnnotation(tc.ann); got != tc.expected {
				t.Errorf("attributeFromValidAnnotation(%q) = %q, want %q", tc.ann, got, tc.expected)
			}
		})
	}
}

func TestAnnotationsUpdated(t *testing.T) {
	ctrl := newTestController("ebs.csi.aws.com")
	tests := []struct {
		name           string
		pvcAnnotations map[string]string
		pvAnnotations  map[string]string
		expected       bool
	}{
		{"annotations differ", map[string]string{"ebs.csi.aws.com/volumeType": "io2"}, map[string]string{"ebs.csi.aws.com/volumeType": "gp3"}, true},
		{"annotations match", map[string]string{"ebs.csi.aws.com/volumeType": "io2"}, map[string]string{"ebs.csi.aws.com/volumeType": "io2"}, false},
		{"no driver annotations on PVC", map[string]string{"other/key": "value"}, map[string]string{}, false},
		{"new annotation on PVC not on PV", map[string]string{"ebs.csi.aws.com/iops": "5000"}, map[string]string{}, true},
		{"nil annotations", nil, nil, false},
		{"status annotation ignored", map[string]string{"ebs.csi.aws.com/volumeType-status": "pending"}, map[string]string{}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ctrl.annotationsUpdated(tc.pvcAnnotations, tc.pvAnnotations); got != tc.expected {
				t.Errorf("annotationsUpdated() = %v, want %v", got, tc.expected)
			}
		})
	}
}

func TestNeedsProcessing(t *testing.T) {
	ctrl := newTestController("ebs.csi.aws.com")
	tests := []struct {
		name     string
		old      *v1.PersistentVolumeClaim
		new      *v1.PersistentVolumeClaim
		expected bool
	}{
		{
			name:     "same resource version",
			old:      &v1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "1"}},
			new:      &v1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "1"}},
			expected: false,
		},
		{
			name: "annotation changed",
			old: &v1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
				ResourceVersion: "1", Annotations: map[string]string{"ebs.csi.aws.com/iops": "3000"},
			}},
			new: &v1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
				ResourceVersion: "2", Annotations: map[string]string{"ebs.csi.aws.com/iops": "5000"},
			}},
			expected: true,
		},
		{
			name: "no driver annotations changed",
			old: &v1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
				ResourceVersion: "1", Annotations: map[string]string{"other/key": "a"},
			}},
			new: &v1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
				ResourceVersion: "2", Annotations: map[string]string{"other/key": "b"},
			}},
			expected: false,
		},
		{
			name: "PVC becomes bound with driver annotations",
			old: &v1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{ResourceVersion: "1", Annotations: map[string]string{"ebs.csi.aws.com/iops": "5000"}},
				Status:     v1.PersistentVolumeClaimStatus{Phase: v1.ClaimPending},
			},
			new: &v1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{ResourceVersion: "2", Annotations: map[string]string{"ebs.csi.aws.com/iops": "5000"}},
				Status:     v1.PersistentVolumeClaimStatus{Phase: v1.ClaimBound},
			},
			expected: true,
		},
		{
			name: "PVC becomes bound without driver annotations",
			old: &v1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{ResourceVersion: "1"},
				Status:     v1.PersistentVolumeClaimStatus{Phase: v1.ClaimPending},
			},
			new: &v1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{ResourceVersion: "2"},
				Status:     v1.PersistentVolumeClaimStatus{Phase: v1.ClaimBound},
			},
			expected: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ctrl.needsProcessing(tc.old, tc.new); got != tc.expected {
				t.Errorf("needsProcessing() = %v, want %v", got, tc.expected)
			}
		})
	}
}

func TestPvcNeedsModification(t *testing.T) {
	driverName := "ebs.csi.aws.com"
	tests := []struct {
		name     string
		setup    func(ctrl *modifyController)
		pv       *v1.PersistentVolume
		pvc      *v1.PersistentVolumeClaim
		expected bool
	}{
		{
			name: "modification already in progress",
			setup: func(ctrl *modifyController) {
				ctrl.modificationInProgress["default/test-pvc"] = struct{}{}
			},
			pv: &v1.PersistentVolume{},
			pvc: &v1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "test-pvc", Namespace: "default"},
				Spec:       v1.PersistentVolumeClaimSpec{VolumeName: "pv1"},
				Status:     v1.PersistentVolumeClaimStatus{Phase: v1.ClaimBound},
			},
			expected: false,
		},
		{
			name: "PVC not bound",
			pv:   &v1.PersistentVolume{},
			pvc: &v1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "test-pvc", Namespace: "default"},
				Spec:       v1.PersistentVolumeClaimSpec{VolumeName: "pv1"},
				Status:     v1.PersistentVolumeClaimStatus{Phase: v1.ClaimPending},
			},
			expected: false,
		},
		{
			name: "empty volume name",
			pv:   &v1.PersistentVolume{},
			pvc: &v1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "test-pvc", Namespace: "default"},
				Spec:       v1.PersistentVolumeClaimSpec{VolumeName: ""},
				Status:     v1.PersistentVolumeClaimStatus{Phase: v1.ClaimBound},
			},
			expected: false,
		},
		{
			name: "annotations not updated returns false",
			pv:   &v1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{"ebs.csi.aws.com/iops": "5000"}}},
			pvc: &v1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "test-pvc", Namespace: "default", Annotations: map[string]string{"ebs.csi.aws.com/iops": "5000"}},
				Spec:       v1.PersistentVolumeClaimSpec{VolumeName: "pv1"},
				Status:     v1.PersistentVolumeClaimStatus{Phase: v1.ClaimBound},
			},
			expected: false,
		},
		{
			name: "annotations updated returns true",
			pv:   &v1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{"ebs.csi.aws.com/iops": "3000"}}},
			pvc: &v1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "test-pvc", Namespace: "default", Annotations: map[string]string{"ebs.csi.aws.com/iops": "5000"}},
				Spec:       v1.PersistentVolumeClaimSpec{VolumeName: "pv1"},
				Status:     v1.PersistentVolumeClaimStatus{Phase: v1.ClaimBound},
			},
			expected: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := newTestController(driverName)
			if tc.setup != nil {
				tc.setup(ctrl)
			}
			if got := ctrl.pvcNeedsModification(tc.pv, tc.pvc); got != tc.expected {
				t.Errorf("pvcNeedsModification() = %v, want %v", got, tc.expected)
			}
		})
	}
}

func TestAddPVC(t *testing.T) {
	ctrl := newTestController("ebs.csi.aws.com")

	t.Run("valid PVC is added to queue", func(t *testing.T) {
		pvc := &v1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "test-pvc", Namespace: "default"},
		}
		ctrl.addPVC(pvc)
		if ctrl.claimQueue.Len() == 0 {
			t.Fatal("expected item in queue after addPVC")
		}
	})

	t.Run("invalid object does not panic", func(t *testing.T) {
		ctrl.addPVC("not-a-k8s-object")
	})
}

func TestUpdatePVC(t *testing.T) {
	ctrl := newTestController("ebs.csi.aws.com")

	t.Run("non-PVC old object is ignored", func(t *testing.T) {
		before := ctrl.claimQueue.Len()
		ctrl.updatePVC("not-a-pvc", &v1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "new", ResourceVersion: "2"},
		})
		if ctrl.claimQueue.Len() != before {
			t.Fatal("queue should not change for non-PVC old object")
		}
	})

	t.Run("non-PVC new object is ignored", func(t *testing.T) {
		before := ctrl.claimQueue.Len()
		ctrl.updatePVC(&v1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "old", ResourceVersion: "1"},
		}, "not-a-pvc")
		if ctrl.claimQueue.Len() != before {
			t.Fatal("queue should not change for non-PVC new object")
		}
	})

	t.Run("nil old object is ignored", func(t *testing.T) {
		before := ctrl.claimQueue.Len()
		ctrl.updatePVC(nil, &v1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "new", ResourceVersion: "2"},
		})
		if ctrl.claimQueue.Len() != before {
			t.Fatal("queue should not change for nil old object")
		}
	})
}

func TestDeletePVC(t *testing.T) {
	ctrl := newTestController("ebs.csi.aws.com")

	t.Run("valid PVC is removed from queue", func(t *testing.T) {
		pvc := &v1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "del-pvc", Namespace: "default"},
		}
		ctrl.addPVC(pvc)
		ctrl.deletePVC(pvc)
	})

	t.Run("DeletedFinalStateUnknown is handled", func(t *testing.T) {
		pvc := &v1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "tomb-pvc", Namespace: "default"},
		}
		tombstone := cache.DeletedFinalStateUnknown{Key: "default/tomb-pvc", Obj: pvc}
		ctrl.deletePVC(tombstone)
	})

	t.Run("invalid object does not panic", func(t *testing.T) {
		ctrl.deletePVC("not-a-k8s-object")
	})
}

func TestInProgressList(t *testing.T) {
	ctrl := newTestController("ebs.csi.aws.com")
	pvc := &v1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pvc", Namespace: "default"},
	}

	if ctrl.ifPVCModificationInProgress(pvc) {
		t.Fatal("expected PVC not to be in progress initially")
	}

	ctrl.addPVCToInProgressList(pvc)
	if !ctrl.ifPVCModificationInProgress(pvc) {
		t.Fatal("expected PVC to be in progress after adding")
	}

	ctrl.removePVCFromInProgressList(pvc)
	if ctrl.ifPVCModificationInProgress(pvc) {
		t.Fatal("expected PVC not to be in progress after removal")
	}
}

func TestSyncPVC_ClaimsStoreError(t *testing.T) {
	ctrl := newTestController("ebs.csi.aws.com")
	ctrl.claims = &fakeErrorStore{}
	ctrl.volumes = &fakeErrorStore{}

	err := ctrl.syncPVC("default/test-pvc")
	if err == nil {
		t.Fatal("expected error from claims store, got nil")
	}
	if !strings.Contains(err.Error(), "cannot get PVC for key") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestSyncPVC_WrongTypeInClaimsStore(t *testing.T) {
	ctrl := newTestController("ebs.csi.aws.com")
	ctrl.claims = &fakeWrongTypeStore{}
	ctrl.volumes = &fakeErrorStore{}

	err := ctrl.syncPVC("default/test-pvc")
	if err == nil {
		t.Fatal("expected error for wrong type in claims store, got nil")
	}
	if !strings.Contains(err.Error(), "expected PVC for key") {
		t.Fatalf("unexpected error message: %v", err)
	}
}
