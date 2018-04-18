/*
Copyright 2018 The Kubernetes Authors.

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

package main

import (
	"flag"
	"strings"
	"time"

	"github.com/kubernetes-sigs/poseidon/pkg/config"
	"github.com/kubernetes-sigs/poseidon/pkg/debugutil"
	"github.com/kubernetes-sigs/poseidon/pkg/firmament"
	"github.com/kubernetes-sigs/poseidon/pkg/k8sclient"
	"github.com/kubernetes-sigs/poseidon/pkg/stats"

	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/golang/glog"
)

var (
	schedulerName      string
	firmamentAddress   string
	kubeConfig         string
	kubeVersion        string
	statsServerAddress string
	schedulingInterval int
	firmamentPort      string
	enablePprof        bool
	pprofAddress       string
)

func init() {
	flag.StringVar(&schedulerName, "schedulerName", "poseidon", "The scheduler name with which pods are labeled")
	flag.StringVar(&firmamentAddress, "firmamentAddress", "firmament-service.kube-system", "Firmament scheduler service port")
	flag.StringVar(&firmamentPort, "firmamentPort", "9090", "Firmament scheduler service port")
	flag.StringVar(&pprofAddress, "pprofAddress", "0.0.0.0:8989", "Address on which to collect runtime profiling data,default to set for all interfaces ")
	flag.StringVar(&kubeConfig, "kubeConfig", "kubeconfig.cfg", "Path to the kubeconfig file")
	flag.StringVar(&kubeVersion, "kubeVersion", "1.6", "Kubernetes version")
	flag.StringVar(&statsServerAddress, "statsServerAddress", "0.0.0.0:9091", "Address on which the stats server listens")
	flag.IntVar(&schedulingInterval, "schedulingInterval", 10, "Time between scheduler runs (in seconds)")
	flag.BoolVar(&enablePprof, "enablePprof", false, "Enable runtime profiling data via HTTP server. Address is at client URL + \"/debug/pprof/\"")
	flag.Parse()
	// join the firmament address and port with a colon separator
	// Passing the firmament address with port and colon separator throws an error
	// for conversion from yaml to json
	values := []string{firmamentAddress, firmamentPort}
	firmamentAddress = strings.Join(values, ":")
}

func schedule(fc firmament.FirmamentSchedulerClient) {
	for {
		deltas := firmament.Schedule(fc)
		glog.Infof("Scheduler returned %d deltas", len(deltas.GetDeltas()))
		for _, delta := range deltas.GetDeltas() {
			switch delta.GetType() {
			case firmament.SchedulingDelta_PLACE:
				k8sclient.PodMux.RLock()
				podIdentifier, ok := k8sclient.TaskIDToPod[delta.GetTaskId()]
				k8sclient.PodMux.RUnlock()
				if !ok {
					glog.Fatalf("Placed task %d without pod pairing", delta.GetTaskId())
				}
				k8sclient.NodeMux.RLock()
				nodeName, ok := k8sclient.ResIDToNode[delta.GetResourceId()]
				k8sclient.NodeMux.RUnlock()
				if !ok {
					glog.Fatalf("Placed task %d on resource %s without node pairing", delta.GetTaskId(), delta.GetResourceId())
				}
				k8sclient.BindPodToNode(podIdentifier.Name, podIdentifier.Namespace, nodeName)
			case firmament.SchedulingDelta_PREEMPT, firmament.SchedulingDelta_MIGRATE:
				k8sclient.PodMux.RLock()
				podIdentifier, ok := k8sclient.TaskIDToPod[delta.GetTaskId()]
				k8sclient.PodMux.RUnlock()
				if !ok {
					glog.Fatalf("Preempted task %d without pod pairing", delta.GetTaskId())
				}
				// XXX(ionel): HACK! Kubernetes does not yet have support for preemption.
				// However, preemption can be achieved by deleting the preempted pod
				// and relying on the controller mechanism (e.g., job, replica set)
				// to submit another instance of this pod.
				k8sclient.DeletePod(podIdentifier.Name, podIdentifier.Namespace)
			case firmament.SchedulingDelta_NOOP:
			default:
				glog.Fatalf("Unexpected SchedulingDelta type %v", delta.GetType())
			}
		}
		// TODO(ionel): Temporary sleep statement because we currently call the scheduler even if there's no work do to.
		time.Sleep(time.Duration(config.GetSchedulingInterval()) * time.Second)
	}
}

// WaitForFirmamentService blocks till the Firmament service is available
func WaitForFirmamentService(fc firmament.FirmamentSchedulerClient) {

	serviceReq := new(firmament.HealthCheckRequest)
	err := wait.PollImmediate(2*time.Second, 10*time.Minute, func() (bool, error) {
		ok, _ := firmament.Check(fc, serviceReq)
		if !ok {
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		glog.Fatalf("Timed-out waiting for firmament service %v", err)
	}
}

func main() {
	glog.Info("Starting Poseidon...", config.GetFirmamentAddress())
	fc, conn, err := firmament.New(config.GetFirmamentAddress())
	if err != nil {
		panic(err)
	}
	defer conn.Close()
	// Check if firmament grpc service is available and then proceed
	WaitForFirmamentService(fc)
	go schedule(fc)
	go stats.StartgRPCStatsServer(config.GetStatsServerAddress(), config.GetFirmamentAddress())
	kubeMajorVer, kubeMinorVer := config.GetKubeVersion()
	if enablePprof {
		go debugutil.EnablePprof(pprofAddress)
	}
	k8sclient.New(config.GetSchedulerName(), config.GetKubeConfig(), kubeMajorVer, kubeMinorVer, config.GetFirmamentAddress())
}
