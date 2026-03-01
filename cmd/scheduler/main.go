// Command scheduler is a custom Kubernetes scheduler that co-locates KubeVirt
// VM pods with their Longhorn RWX share-manager pods on the same node.
//
// It embeds the default kube-scheduler and registers the LonghornCoSchedule
// plugin as an additional Filter and Score plugin.
package main

import (
	"os"

	"k8s.io/component-base/cli"
	_ "k8s.io/component-base/metrics/prometheus/clientgo" // register rest client metrics
	_ "k8s.io/component-base/metrics/prometheus/version"  // register version metrics
	"k8s.io/kubernetes/cmd/kube-scheduler/app"

	"github.com/michaeltrip/virthorn-scheduler/pkg/plugins/longhorn_cosched"
)

func main() {
	command := app.NewSchedulerCommand(
		app.WithPlugin(longhorn_cosched.Name, longhorn_cosched.New),
	)

	code := cli.Run(command)
	os.Exit(code)
}
