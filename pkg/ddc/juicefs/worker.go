/*
Copyright 2021 Juicedata Inc

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package juicefs

import (
	"context"
	"fmt"
	datav1alpha1 "github.com/fluid-cloudnative/fluid/api/v1alpha1"
	"github.com/fluid-cloudnative/fluid/pkg/common"
	"github.com/fluid-cloudnative/fluid/pkg/utils"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/util/retry"
	"reflect"
)

func (j JuiceFSEngine) CheckWorkersReady() (ready bool, err error) {
	var (
		fuseReady, fusePartialReady bool
		fuseName                    = j.getFuseDaemonsetName()
		namespace                   = j.namespace
	)

	runtime, err := j.getRuntime()
	if err != nil {
		return ready, err
	}

	j.Log.Info("Fuse deploy mode", "global", runtime.Spec.Fuse.Global)
	fuses, err := j.getDaemonset(fuseName, namespace)
	if fuses.Status.NumberAvailable > 0 {
		if runtime.Spec.Fuse.Global {
			if fuses.Status.DesiredNumberScheduled == fuses.Status.CurrentNumberScheduled {
				fuseReady = true
			} else {
				fusePartialReady = true
			}
		} else {
			if runtime.Spec.Replicas == fuses.Status.NumberReady {
				fuseReady = true
			} else if fuses.Status.NumberReady >= 1 {
				fusePartialReady = true
			}
		}
	}

	if fuseReady || fusePartialReady {
		ready = true
	} else {
		j.Log.Info("fuses are not ready", "fuseReady", fuseReady, "fusePartialReady", fusePartialReady)
		return
	}
	// update the status as the workers are ready
	err = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		runtime, err := j.getRuntime()
		if err != nil {
			return err
		}
		runtimeToUpdate := runtime.DeepCopy()
		if len(runtimeToUpdate.Status.Conditions) == 0 {
			runtimeToUpdate.Status.Conditions = []datav1alpha1.RuntimeCondition{}
		}
		fuseCond := utils.NewRuntimeCondition(datav1alpha1.RuntimeFusesReady, datav1alpha1.RuntimeFusesReadyReason,
			"The fuses are ready.", corev1.ConditionTrue)

		if fusePartialReady {
			fuseCond = utils.NewRuntimeCondition(datav1alpha1.RuntimeFusesReady, datav1alpha1.RuntimeFusesReadyReason,
				"The fuses are partially ready.", corev1.ConditionTrue)

			runtimeToUpdate.Status.FusePhase = datav1alpha1.RuntimePhasePartialReady
		}
		runtimeToUpdate.Status.Conditions =
			utils.UpdateRuntimeCondition(runtimeToUpdate.Status.Conditions,
				fuseCond)

		if !reflect.DeepEqual(runtime.Status, runtimeToUpdate.Status) {
			return j.Client.Status().Update(context.TODO(), runtimeToUpdate)
		}

		return nil
	})

	return
}

func (j JuiceFSEngine) ShouldSetupWorkers() (should bool, err error) {
	runtime, err := j.getRuntime()
	if err != nil {
		return
	}

	switch runtime.Status.FusePhase {
	case datav1alpha1.RuntimePhaseNone:
		should = true
	default:
		should = false
	}

	return
}

func (j JuiceFSEngine) SetupWorkers() (err error) {
	runtime, err := j.getRuntime()
	if err != nil {
		j.Log.Error(err, "setupWorker")
		return err
	}

	replicas := runtime.Replicas()
	currentReplicas, err := j.AssignNodesToCache(replicas)
	if err != nil {
		return err
	}

	j.Log.Info("check the desired and current replicas",
		"desiredReplicas", replicas, "currentReplicas", currentReplicas)

	if currentReplicas == 0 {
		return fmt.Errorf("the number of the current clients which can be scheduled is 0")
	}

	// 2. Update the status
	err = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		runtime, err := j.getRuntime()
		if err != nil {
			j.Log.Error(err, "setupWorker")
			return err
		}

		runtimeToUpdate := runtime.DeepCopy()

		runtimeToUpdate.Status.FusePhase = datav1alpha1.RuntimePhaseNotReady

		if runtimeToUpdate.Spec.Fuse.Global {
			fuseName := j.getFuseDaemonsetName()
			fuses, err := j.getDaemonset(fuseName, j.namespace)
			if err != nil {
				j.Log.Error(err, "setupWorker")
				return err
			}

			// Clean the label to start the daemonset deployment
			fusesToUpdate := fuses.DeepCopy()
			j.Log.Info("check node labels of fuse before cleaning balloon key", "labels", fusesToUpdate.Spec.Template.Spec.NodeSelector)
			delete(fusesToUpdate.Spec.Template.Spec.NodeSelector, common.FLUID_FUSE_BALLOON_KEY)
			j.Log.Info("check node labels of fuse after cleaning balloon key", "labels", fusesToUpdate.Spec.Template.Spec.NodeSelector)
			err = j.Client.Update(context.TODO(), fusesToUpdate)
			if err != nil {
				j.Log.Error(err, "setupWorker")
				return err
			}
		} else {
			runtimeToUpdate.Status.DesiredFuseNumberScheduled = replicas
			runtimeToUpdate.Status.CurrentFuseNumberScheduled = currentReplicas
		}
		if len(runtimeToUpdate.Status.Conditions) == 0 {
			runtimeToUpdate.Status.Conditions = []datav1alpha1.RuntimeCondition{}
		}
		fuseCond := utils.NewRuntimeCondition(datav1alpha1.RuntimeFusesInitialized, datav1alpha1.RuntimeFusesInitializedReason,
			"The fuses are initialized.", corev1.ConditionTrue)
		runtimeToUpdate.Status.Conditions =
			utils.UpdateRuntimeCondition(runtimeToUpdate.Status.Conditions, fuseCond)

		if !reflect.DeepEqual(runtime.Status, runtimeToUpdate.Status) {
			return j.Client.Status().Update(context.TODO(), runtimeToUpdate)
		}

		return nil
	})

	return
}
