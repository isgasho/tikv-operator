// Copyright 2018 TiKV Project Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package member

import (
	"fmt"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	"github.com/pingcap/kvproto/pkg/pdpb"
	"github.com/tikv/tikv-operator/pkg/apis/tikv/v1alpha1"
	"github.com/tikv/tikv-operator/pkg/controller"
	"github.com/tikv/tikv-operator/pkg/label"
	"github.com/tikv/tikv-operator/pkg/pdapi"
	apps "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubeinformers "k8s.io/client-go/informers"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
)

func TestPDScalerScaleOut(t *testing.T) {
	g := NewGomegaWithT(t)
	type testcase struct {
		name             string
		update           func(cluster *v1alpha1.TikvCluster)
		pdUpgrading      bool
		hasPVC           bool
		hasDeferAnn      bool
		annoIsNil        bool
		pvcDeleteErr     bool
		statusSyncFailed bool
		err              bool
		changed          bool
	}

	testFn := func(test *testcase, t *testing.T) {
		t.Log(test.name)
		tc := newTikvClusterForPD()
		test.update(tc)

		if test.pdUpgrading {
			tc.Status.PD.Phase = v1alpha1.UpgradePhase
		}

		oldSet := newStatefulSetForPDScale()
		newSet := oldSet.DeepCopy()
		newSet.Spec.Replicas = controller.Int32Ptr(7)

		scaler, _, pvcIndexer, pvcControl := newFakePDScaler()

		pvc := newPVCForStatefulSet(oldSet, v1alpha1.PDMemberType, tc.Name)
		pvc.Name = ordinalPVCName(v1alpha1.PDMemberType, oldSet.GetName(), *oldSet.Spec.Replicas)
		if !test.annoIsNil {
			pvc.Annotations = map[string]string{}
		}

		if test.hasDeferAnn {
			pvc.Annotations = map[string]string{}
			pvc.Annotations[label.AnnPVCDeferDeleting] = time.Now().Format(time.RFC3339)
		}
		if test.hasPVC {
			pvcIndexer.Add(pvc)
		}

		if test.pvcDeleteErr {
			pvcControl.SetDeletePVCError(errors.NewInternalError(fmt.Errorf("API server failed")), 0)
		}

		tc.Status.PD.Synced = !test.statusSyncFailed

		err := scaler.ScaleOut(tc, oldSet, newSet)
		if test.err {
			g.Expect(err).To(HaveOccurred())
		} else {
			g.Expect(err).NotTo(HaveOccurred())
		}
		if test.changed {
			g.Expect(int(*newSet.Spec.Replicas)).To(Equal(6))
		} else {
			g.Expect(int(*newSet.Spec.Replicas)).To(Equal(5))
		}
	}

	tests := []testcase{
		{
			name:             "normal",
			update:           normalPDMember,
			pdUpgrading:      false,
			hasPVC:           true,
			hasDeferAnn:      false,
			annoIsNil:        true,
			pvcDeleteErr:     false,
			statusSyncFailed: false,
			err:              false,
			changed:          true,
		},
		{
			name:             "pd is upgrading",
			update:           normalPDMember,
			pdUpgrading:      true,
			hasPVC:           true,
			hasDeferAnn:      false,
			annoIsNil:        true,
			pvcDeleteErr:     false,
			statusSyncFailed: false,
			err:              false,
			changed:          false,
		},
		{
			name:             "cache don't have pvc",
			update:           normalPDMember,
			pdUpgrading:      false,
			hasPVC:           false,
			hasDeferAnn:      false,
			annoIsNil:        true,
			pvcDeleteErr:     false,
			statusSyncFailed: false,
			err:              false,
			changed:          true,
		},
		{
			name:             "pvc annotation is not nil but doesn't contain defer deletion annotation",
			update:           normalPDMember,
			pdUpgrading:      false,
			hasPVC:           true,
			hasDeferAnn:      false,
			annoIsNil:        false,
			pvcDeleteErr:     false,
			statusSyncFailed: false,
			err:              false,
			changed:          true,
		},
		{
			name:             "pvc annotations defer deletion is not nil, pvc delete failed",
			update:           normalPDMember,
			pdUpgrading:      false,
			hasPVC:           true,
			hasDeferAnn:      true,
			annoIsNil:        false,
			pvcDeleteErr:     true,
			statusSyncFailed: false,
			err:              true,
			changed:          false,
		},
		{
			name: "failover now",
			update: func(tc *v1alpha1.TikvCluster) {
				normalPDMember(tc)
				podName := ordinalPodName(v1alpha1.PDMemberType, tc.GetName(), 0)
				tc.Status.PD.FailureMembers = map[string]v1alpha1.PDFailureMember{
					podName: {PodName: podName},
				}
				pd := tc.Status.PD.Members[podName]
				pd.Health = false
				tc.Status.PD.Members[podName] = pd
			},
			pdUpgrading:      false,
			hasPVC:           true,
			hasDeferAnn:      true,
			annoIsNil:        false,
			pvcDeleteErr:     false,
			statusSyncFailed: false,
			err:              false,
			changed:          true,
		},
		{
			name: "don't have members",
			update: func(tc *v1alpha1.TikvCluster) {
				tc.Status.PD.Members = nil
			},
			pdUpgrading:      false,
			hasPVC:           true,
			hasDeferAnn:      true,
			annoIsNil:        false,
			pvcDeleteErr:     false,
			statusSyncFailed: false,
			err:              true,
			changed:          false,
		},
		{
			name: "pd member not health",
			update: func(tc *v1alpha1.TikvCluster) {
				normalPDMember(tc)
				tcName := tc.GetName()
				podName := ordinalPodName(v1alpha1.PDMemberType, tcName, 1)
				member1 := tc.Status.PD.Members[podName]
				member1.Health = false
				tc.Status.PD.Members[podName] = member1
			},
			pdUpgrading:      false,
			hasPVC:           true,
			hasDeferAnn:      false,
			annoIsNil:        true,
			pvcDeleteErr:     false,
			statusSyncFailed: false,
			err:              true,
			changed:          false,
		},
		{
			name:             "pd status sync failed",
			update:           normalPDMember,
			pdUpgrading:      false,
			hasPVC:           true,
			hasDeferAnn:      false,
			annoIsNil:        true,
			pvcDeleteErr:     false,
			statusSyncFailed: true,
			err:              true,
			changed:          false,
		},
	}

	for i := range tests {
		testFn(&tests[i], t)
	}
}

func TestPDScalerScaleIn(t *testing.T) {
	g := NewGomegaWithT(t)
	type testcase struct {
		name             string
		pdUpgrading      bool
		hasPVC           bool
		pvcUpdateErr     bool
		deleteMemberErr  bool
		statusSyncFailed bool
		err              bool
		changed          bool
		isLeader         bool
	}

	testFn := func(test *testcase, t *testing.T) {
		t.Log(test.name)
		tc := newTikvClusterForPD()

		if test.pdUpgrading {
			tc.Status.PD.Phase = v1alpha1.UpgradePhase
		}

		oldSet := newStatefulSetForPDScale()
		newSet := oldSet.DeepCopy()
		newSet.Spec.Replicas = controller.Int32Ptr(3)

		scaler, pdControl, pvcIndexer, pvcControl := newFakePDScaler()

		if test.hasPVC {
			pvc := newScaleInPVCForStatefulSet(oldSet, v1alpha1.PDMemberType, tc.Name)
			pvcIndexer.Add(pvc)
		}

		pdClient := controller.NewFakePDClient(pdControl, tc)

		pdClient.AddReaction(pdapi.GetPDLeaderActionType, func(action *pdapi.Action) (interface{}, error) {
			leader := pdpb.Member{
				Name: fmt.Sprintf("%s-pd-%d", tc.GetName(), 0),
			}
			return &leader, nil
		})

		if test.deleteMemberErr {
			pdClient.AddReaction(pdapi.DeleteMemberActionType, func(action *pdapi.Action) (interface{}, error) {
				return nil, fmt.Errorf("error")
			})
		}
		if test.pvcUpdateErr {
			pvcControl.SetUpdatePVCError(errors.NewInternalError(fmt.Errorf("API server failed")), 0)
		}

		if test.isLeader {
			pdClient.AddReaction(pdapi.GetPDLeaderActionType, func(action *pdapi.Action) (interface{}, error) {
				leader := pdpb.Member{
					Name: fmt.Sprintf("%s-pd-%d", tc.GetName(), 4),
				}
				return &leader, nil
			})
			pdClient.AddReaction(pdapi.TransferPDLeaderActionType, func(action *pdapi.Action) (i interface{}, e error) {
				return nil, nil
			})
		}

		tc.Status.PD.Synced = !test.statusSyncFailed

		err := scaler.ScaleIn(tc, oldSet, newSet)
		if test.err {
			g.Expect(err).To(HaveOccurred())
		} else {
			g.Expect(err).NotTo(HaveOccurred())
		}
		if test.changed {
			g.Expect(int(*newSet.Spec.Replicas)).To(Equal(4))
		} else {
			g.Expect(int(*newSet.Spec.Replicas)).To(Equal(5))
		}
	}

	tests := []testcase{
		{
			name:             "normal",
			pdUpgrading:      false,
			hasPVC:           true,
			pvcUpdateErr:     false,
			deleteMemberErr:  false,
			statusSyncFailed: false,
			err:              false,
			changed:          true,
			isLeader:         false,
		},
		{
			name:             "pd is upgrading",
			pdUpgrading:      true,
			hasPVC:           true,
			pvcUpdateErr:     false,
			deleteMemberErr:  false,
			statusSyncFailed: false,
			err:              false,
			changed:          false,
			isLeader:         false,
		},
		{
			name:             "error when delete member",
			hasPVC:           true,
			pvcUpdateErr:     false,
			pdUpgrading:      false,
			deleteMemberErr:  true,
			statusSyncFailed: false,
			err:              true,
			changed:          false,
			isLeader:         false,
		},
		{
			name:             "cache don't have pvc",
			pdUpgrading:      false,
			hasPVC:           false,
			pvcUpdateErr:     false,
			deleteMemberErr:  false,
			statusSyncFailed: false,
			err:              true,
			changed:          false,
			isLeader:         false,
		},
		{
			name:             "error when update pvc",
			pdUpgrading:      false,
			hasPVC:           true,
			pvcUpdateErr:     true,
			deleteMemberErr:  false,
			statusSyncFailed: false,
			err:              true,
			changed:          false,
			isLeader:         false,
		},
		{
			name:             "pd status sync failed",
			pdUpgrading:      false,
			hasPVC:           true,
			pvcUpdateErr:     false,
			deleteMemberErr:  false,
			statusSyncFailed: true,
			err:              true,
			changed:          false,
			isLeader:         false,
		},
	}

	for i := range tests {
		testFn(&tests[i], t)
	}
}

func newFakePDScaler() (*pdScaler, *pdapi.FakePDControl, cache.Indexer, *controller.FakePVCControl) {
	kubeCli := kubefake.NewSimpleClientset()

	kubeInformerFactory := kubeinformers.NewSharedInformerFactory(kubeCli, 0)
	pvcInformer := kubeInformerFactory.Core().V1().PersistentVolumeClaims()
	pdControl := pdapi.NewFakePDControl(kubeCli)
	pvcControl := controller.NewFakePVCControl(pvcInformer)

	return &pdScaler{generalScaler{pdControl, pvcInformer.Lister(), pvcControl}},
		pdControl, pvcInformer.Informer().GetIndexer(), pvcControl
}

func newStatefulSetForPDScale() *apps.StatefulSet {
	set := &apps.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "scaler",
			Namespace: metav1.NamespaceDefault,
		},
		Spec: apps.StatefulSetSpec{
			Replicas: controller.Int32Ptr(5),
		},
	}
	return set
}

func newPVCForStatefulSet(set *apps.StatefulSet, memberType v1alpha1.MemberType, name string) *corev1.PersistentVolumeClaim {
	podName := ordinalPodName(memberType, name, *set.Spec.Replicas)
	l := label.New().Instance(name)
	l[label.AnnPodNameKey] = podName
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ordinalPVCName(memberType, set.GetName(), *set.Spec.Replicas),
			Namespace: metav1.NamespaceDefault,
			Labels:    l,
		},
	}
}

func newScaleInPVCForStatefulSet(set *apps.StatefulSet, memberType v1alpha1.MemberType, name string) *corev1.PersistentVolumeClaim {
	podName := ordinalPodName(memberType, name, *set.Spec.Replicas-1)
	l := label.New().Instance(name)
	l[label.AnnPodNameKey] = podName
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ordinalPVCName(memberType, set.GetName(), *set.Spec.Replicas-1),
			Namespace: metav1.NamespaceDefault,
			Labels:    l,
		},
	}
}

func normalPDMember(tc *v1alpha1.TikvCluster) {
	tcName := tc.GetName()
	tc.Status.PD.Members = map[string]v1alpha1.PDMember{
		ordinalPodName(v1alpha1.PDMemberType, tcName, 0): {Health: true},
		ordinalPodName(v1alpha1.PDMemberType, tcName, 1): {Health: true},
		ordinalPodName(v1alpha1.PDMemberType, tcName, 2): {Health: true},
		ordinalPodName(v1alpha1.PDMemberType, tcName, 3): {Health: true},
		ordinalPodName(v1alpha1.PDMemberType, tcName, 4): {Health: true},
	}
}
