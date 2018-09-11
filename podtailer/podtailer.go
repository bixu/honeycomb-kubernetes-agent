// Package podtailer contains machinery for tailing the logs of a *set* of pods
// matching a labelSelector.
package podtailer

import (
	"fmt"
	"os"
	"regexp"
	"sync"

	"k8s.io/client-go/pkg/api/v1"
	"k8s.io/client-go/pkg/types"

	"github.com/Sirupsen/logrus"
	"github.com/honeycombio/honeycomb-kubernetes-agent/config"
	"github.com/honeycombio/honeycomb-kubernetes-agent/handlers"
	"github.com/honeycombio/honeycomb-kubernetes-agent/k8sagent"
	"github.com/honeycombio/honeycomb-kubernetes-agent/processors"
	"github.com/honeycombio/honeycomb-kubernetes-agent/tailer"
	"github.com/honeycombio/honeycomb-kubernetes-agent/transmission"
	"github.com/honeycombio/honeycomb-kubernetes-agent/unwrappers"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
)

// PodSetTailer is responsible for watching for all pods that match the
// criteria defined by config, and managing tailers for each pod.
type PodSetTailer struct {
	config         *config.WatcherConfig
	nodeSelector   string
	transmitter    transmission.Transmitter
	stateRecorder  tailer.StateRecorder
	kubeClient     corev1.PodsGetter
	stop           chan bool
	wg             sync.WaitGroup
	legacyLogPaths bool
}

func NewPodSetTailer(
	config *config.WatcherConfig,
	nodeSelector string,
	transmitter transmission.Transmitter,
	stateRecorder tailer.StateRecorder,
	kubeClient corev1.PodsGetter,
	legacyLogPaths bool,
) *PodSetTailer {
	return &PodSetTailer{
		config:         config,
		nodeSelector:   nodeSelector,
		transmitter:    transmitter,
		stateRecorder:  stateRecorder,
		kubeClient:     kubeClient,
		stop:           make(chan bool),
		legacyLogPaths: legacyLogPaths,
	}
}

func (pt *PodSetTailer) run() {
	defer pt.wg.Done()
	labelSelector := *pt.config.LabelSelector
	// Exclude the agent's own logs from being watched
	if labelSelector == "" {
		labelSelector = "k8s-app!=honeycomb-agent"
	} else {
		labelSelector = labelSelector + ",k8s-app!=honeycomb-agent"
	}

	podWatcher := k8sagent.NewPodWatcher(
		pt.config.Namespace,
		labelSelector,
		pt.nodeSelector,
		pt.kubeClient)

	watcherMap := make(map[types.UID]*tailer.PathWatcher)

loop:
	for {
		select {
		case pod := <-podWatcher.Pods():
			watcher, err := pt.watcherForPod(pod, pt.config.ContainerName, podWatcher)
			if err != nil {
				// This shouldn't happen, since we check for configuration errors
				// before actually setting up the watcher
				logrus.WithError(err).Error("Error setting up watcher")
				continue loop
			}
			logrus.WithFields(logrus.Fields{
				"name":      pod.Name,
				"uid":       pod.UID,
				"namespace": pod.Namespace,
			}).Info("starting watcher for pod")
			watcher.Start()
			watcherMap[pod.UID] = watcher
		case deletedPodUID := <-podWatcher.DeletedPods():
			if watcher, ok := watcherMap[deletedPodUID]; ok {
				logrus.WithFields(logrus.Fields{
					"uid": deletedPodUID,
				}).Info("pod deleted, stopping watcher")
				watcher.Stop()
				delete(watcherMap, deletedPodUID)
			}
		case <-pt.stop:
			break loop
		}
	}

	for key := range watcherMap {
		watcherMap[key].Stop()
	}
}

func (pt *PodSetTailer) Start() {
	pt.wg.Add(1)
	go pt.run()
}

func (pt *PodSetTailer) Stop() {
	pt.stop <- true
	pt.wg.Wait()
}

func determineLogPattern(pod *v1.Pod, legacyLogPaths bool) (string, error) {
	// Old pattern was:
	// /var/log/containers/<pod_name>_<pod_namespace>_<container_name>-<container_id>.log`
	// For now, this is still supported on newer k8s clusters with a symlink
	if legacyLogPaths {
		return fmt.Sprintf("/var/log/containers/%s_%s_*.log", pod.Name, pod.Namespace), nil
	}
	// New pattern is: /var/log/pods/<podUID>/<containerName>_<instance#>.log
	// Critical pods seem to all use this config hash for their log directory
	// instead of the pod UID. Use the hash if it exists
	if hash, ok := pod.Annotations["kubernetes.io/config.hash"]; ok {
		hpath := fmt.Sprintf("/var/log/pods/%s", hash)
		if _, err := os.Stat(hpath); err == nil {
			logrus.WithFields(logrus.Fields{
				"PodName": pod.Name,
				"UID":     pod.UID,
				"Hash":    hash,
			}).Info("Critical pod detected, using config.hash for log dir")
			return fmt.Sprintf("%s/*", hpath), nil
		}
	}
	upath := fmt.Sprintf("/var/log/pods/%s", pod.UID)
	if _, err := os.Stat(upath); err == nil {
		return fmt.Sprintf("%s/*", upath), nil
	}
	return "", fmt.Errorf("Could not find specified log path for pod %s", pod.UID)
}

func determineFilterFunc(pod *v1.Pod, containerName string, legacyLogPaths bool) func(fileName string) bool {
	if containerName == "" {
		return nil
	}
	if legacyLogPaths {
		re := fmt.Sprintf(
			"^/var/log/containers/%s_%s_%s-.+\\.log",
			pod.Name,
			pod.Namespace,
			containerName,
		)
		return func(fileName string) bool {
			ok, _ := regexp.Match(re, []byte(fileName))
			return ok
		}
	}

	uid := string(pod.UID)
	if hash, ok := pod.Annotations["kubernetes.io/config.hash"]; ok {
		uid = hash
	}

	re := fmt.Sprintf("^/var/log/pods/%s/%s_[0-9]*\\.log", uid, regexp.QuoteMeta(containerName))
	return func(fileName string) bool {
		ok, _ := regexp.Match(re, []byte(fileName))
		return ok
	}
}

func (pt *PodSetTailer) watcherForPod(pod *v1.Pod, containerName string, podWatcher k8sagent.PodWatcher) (*tailer.PathWatcher, error) {
	pattern, err := determineLogPattern(pod, pt.legacyLogPaths)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"Pod": pod.UID,
		}).Warn("Error finding log path")

		// it's odd that we don't return here, should we?
	}

	// only watch logs for containers matching the given name, if
	// one is specified
	filterFunc := determineFilterFunc(pod, containerName, pt.legacyLogPaths)

	k8sMetadataProcessor := &processors.KubernetesMetadataProcessor{
		PodGetter: podWatcher,
		UID:       pod.UID}
	handlerFactory, err := handlers.NewLineHandlerFactoryFromConfig(
		pt.config,
		&unwrappers.DockerJSONLogUnwrapper{},
		pt.transmitter,
		k8sMetadataProcessor)
	if err != nil {
		// This shouldn't happen, since we check for configuration errors
		// before actually setting up the watcher
		logrus.WithError(err).Error("Error setting up watcher")
		return nil, err
	}

	logrus.WithFields(logrus.Fields{
		"Name":    pod.Name,
		"UID":     pod.UID,
		"Pattern": pattern,
	}).Info("Setting up watcher for pod")

	watcher := tailer.NewPathWatcher(pattern, filterFunc, handlerFactory, pt.stateRecorder)
	return watcher, nil
}
