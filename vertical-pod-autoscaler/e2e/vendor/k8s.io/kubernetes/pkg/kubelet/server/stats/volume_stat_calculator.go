/*
Copyright 2016 The Kubernetes Authors.

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

package stats

import (
	"sync"
	"sync/atomic"
	"time"

	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	stats "k8s.io/kubernetes/pkg/kubelet/apis/stats/v1alpha1"
	"k8s.io/kubernetes/pkg/kubelet/util/format"
	"k8s.io/kubernetes/pkg/volume"

	"k8s.io/klog"
)

// volumeStatCalculator calculates volume metrics for a given pod periodically in the background and caches the result
type volumeStatCalculator struct {
	statsProvider Provider
	jitterPeriod  time.Duration
	pod           *v1.Pod
	stopChannel   chan struct{}
	startO        sync.Once
	stopO         sync.Once
	latest        atomic.Value
}

// PodVolumeStats encapsulates the VolumeStats for a pod.
// It consists of two lists, for local ephemeral volumes, and for persistent volumes respectively.
type PodVolumeStats struct {
	EphemeralVolumes  []stats.VolumeStats
	PersistentVolumes []stats.VolumeStats
}

// newVolumeStatCalculator creates a new VolumeStatCalculator
func newVolumeStatCalculator(statsProvider Provider, jitterPeriod time.Duration, pod *v1.Pod) *volumeStatCalculator {
	return &volumeStatCalculator{
		statsProvider: statsProvider,
		jitterPeriod:  jitterPeriod,
		pod:           pod,
		stopChannel:   make(chan struct{}),
	}
}

// StartOnce starts pod volume calc that will occur periodically in the background until s.StopOnce is called
func (s *volumeStatCalculator) StartOnce() *volumeStatCalculator {
	s.startO.Do(func() {
		go wait.JitterUntil(func() {
			s.calcAndStoreStats()
		}, s.jitterPeriod, 1.0, true, s.stopChannel)
	})
	return s
}

// StopOnce stops background pod volume calculation.  Will not stop a currently executing calculations until
// they complete their current iteration.
func (s *volumeStatCalculator) StopOnce() *volumeStatCalculator {
	s.stopO.Do(func() {
		close(s.stopChannel)
	})
	return s
}

// getLatest returns the most recent PodVolumeStats from the cache
func (s *volumeStatCalculator) GetLatest() (PodVolumeStats, bool) {
	result := s.latest.Load()
	if result == nil {
		return PodVolumeStats{}, false
	}
	return result.(PodVolumeStats), true
}

// calcAndStoreStats calculates PodVolumeStats for a given pod and writes the result to the s.latest cache.
// If the pod references PVCs, the prometheus metrics for those are updated with the result.
func (s *volumeStatCalculator) calcAndStoreStats() {
	// Find all Volumes for the Pod
	volumes, found := s.statsProvider.ListVolumesForPod(s.pod.UID)
	if !found {
		return
	}

	// Get volume specs for the pod - key'd by volume name
	volumesSpec := make(map[string]v1.Volume)
	for _, v := range s.pod.Spec.Volumes {
		volumesSpec[v.Name] = v
	}

	// Call GetMetrics on each Volume and copy the result to a new VolumeStats.FsStats
	var ephemeralStats []stats.VolumeStats
	var persistentStats []stats.VolumeStats
	for name, v := range volumes {
		metric, err := v.GetMetrics()
		if err != nil {
			// Expected for Volumes that don't support Metrics
			if !volume.IsNotSupported(err) {
				klog.V(4).Infof("Failed to calculate volume metrics for pod %s volume %s: %+v", format.Pod(s.pod), name, err)
			}
			continue
		}
		// Lookup the volume spec and add a 'PVCReference' for volumes that reference a PVC
		volSpec := volumesSpec[name]
		var pvcRef *stats.PVCReference
		if pvcSource := volSpec.PersistentVolumeClaim; pvcSource != nil {
			pvcRef = &stats.PVCReference{
				Name:      pvcSource.ClaimName,
				Namespace: s.pod.GetNamespace(),
			}
		}
		volumeStats := s.parsePodVolumeStats(name, pvcRef, metric, volSpec)
		if isVolumeEphemeral(volSpec) {
			ephemeralStats = append(ephemeralStats, volumeStats)
		} else {
			persistentStats = append(persistentStats, volumeStats)
		}

	}

	// Store the new stats
	s.latest.Store(PodVolumeStats{EphemeralVolumes: ephemeralStats,
		PersistentVolumes: persistentStats})
}

// parsePodVolumeStats converts (internal) volume.Metrics to (external) stats.VolumeStats structures
func (s *volumeStatCalculator) parsePodVolumeStats(podName string, pvcRef *stats.PVCReference, metric *volume.Metrics, volSpec v1.Volume) stats.VolumeStats {

	var available, capacity, used, inodes, inodesFree, inodesUsed uint64
	if metric.Available != nil {
		available = uint64(metric.Available.Value())
	}
	if metric.Capacity != nil {
		capacity = uint64(metric.Capacity.Value())
	}
	if metric.Used != nil {
		used = uint64(metric.Used.Value())
	}
	if metric.Inodes != nil {
		inodes = uint64(metric.Inodes.Value())
	}
	if metric.InodesFree != nil {
		inodesFree = uint64(metric.InodesFree.Value())
	}
	if metric.InodesUsed != nil {
		inodesUsed = uint64(metric.InodesUsed.Value())
	}

	return stats.VolumeStats{
		Name:   podName,
		PVCRef: pvcRef,
		FsStats: stats.FsStats{Time: metric.Time, AvailableBytes: &available, CapacityBytes: &capacity,
			UsedBytes: &used, Inodes: &inodes, InodesFree: &inodesFree, InodesUsed: &inodesUsed},
	}
}

func isVolumeEphemeral(volume v1.Volume) bool {
	if (volume.EmptyDir != nil && volume.EmptyDir.Medium == v1.StorageMediumDefault) ||
		volume.ConfigMap != nil || volume.GitRepo != nil {
		return true
	}
	return false
}
