// Copyright Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package kube

import (
	"context"
	"fmt"
	"io"
	"time"

	kubeCore "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"istio.io/istio/pkg/test"
	appEcho "istio.io/istio/pkg/test/echo/client"
	"istio.io/istio/pkg/test/framework/components/cluster"
	"istio.io/istio/pkg/test/framework/components/echo"
	"istio.io/istio/pkg/test/framework/components/echo/common"
	"istio.io/istio/pkg/test/framework/resource"
	"istio.io/istio/pkg/test/util/retry"
)

const (
	tcpHealthPort     = 3333
	httpReadinessPort = 8080
)

var (
	_ echo.Instance = &instance{}
	_ io.Closer     = &instance{}

	startDelay = retry.Delay(2 * time.Second)
)

type instance struct {
	id          resource.ID
	cfg         echo.Config
	clusterIP   string
	ctx         resource.Context
	cluster     cluster.Cluster
	workloadMgr *workloadManager
	deployment  *deployment
}

func newInstance(ctx resource.Context, originalCfg echo.Config) (out *instance, err error) {
	cfg := originalCfg.DeepCopy()

	c := &instance{
		cfg:     cfg,
		ctx:     ctx,
		cluster: cfg.Cluster,
	}
	c.id = ctx.TrackResource(c)

	// Deploy echo to the cluster
	c.deployment, err = newDeployment(ctx, cfg)
	if err != nil {
		return nil, err
	}

	// Create the manager for echo workloads for this instance.
	c.workloadMgr, err = newWorkloadManager(ctx, cfg, c.deployment)
	if err != nil {
		return nil, err
	}

	// Now retrieve the service information to find the ClusterIP
	s, err := c.cluster.CoreV1().Services(cfg.Namespace.Name()).Get(context.TODO(), cfg.Service, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	c.clusterIP = s.Spec.ClusterIP
	switch c.clusterIP {
	case kubeCore.ClusterIPNone, "":
		if !cfg.Headless {
			return nil, fmt.Errorf("invalid ClusterIP %s for non-headless service %s/%s",
				c.clusterIP,
				c.cfg.Namespace.Name(),
				c.cfg.Service)
		}
		c.clusterIP = ""
	}

	return c, nil
}

func (c *instance) ID() resource.ID {
	return c.id
}

func (c *instance) Address() string {
	return c.clusterIP
}

func (c *instance) Workloads() ([]echo.Workload, error) {
	return c.workloadMgr.ReadyWorkloads()
}

func (c *instance) WorkloadsOrFail(t test.Failer) []echo.Workload {
	t.Helper()
	out, err := c.Workloads()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func (c *instance) firstWorkload() (echo.Workload, error) {
	workloads, err := c.Workloads()
	if err != nil {
		return nil, err
	}
	return workloads[0], nil
}

// Start this echo instance
func (c *instance) Start() error {
	return c.workloadMgr.Start()
}

func (c *instance) Close() (err error) {
	return c.workloadMgr.Close()
}

func (c *instance) Config() echo.Config {
	return c.cfg
}

func (c *instance) Call(opts echo.CallOptions) (appEcho.ParsedResponses, error) {
	w, err := c.firstWorkload()
	if err != nil {
		return nil, err
	}
	out, err := common.ForwardEcho(c.cfg.Service, w.(*workload).Instance, &opts, false)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (c *instance) CallOrFail(t test.Failer, opts echo.CallOptions) appEcho.ParsedResponses {
	t.Helper()
	r, err := c.Call(opts)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (c *instance) CallWithRetry(opts echo.CallOptions,
	retryOptions ...retry.Option) (appEcho.ParsedResponses, error) {
	w, err := c.firstWorkload()
	if err != nil {
		return nil, err
	}
	out, err := common.ForwardEcho(c.cfg.Service, w.(*workload).Instance, &opts, true, retryOptions...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (c *instance) CallWithRetryOrFail(t test.Failer, opts echo.CallOptions,
	retryOptions ...retry.Option) appEcho.ParsedResponses {
	t.Helper()
	r, err := c.CallWithRetry(opts, retryOptions...)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (c *instance) Restart() error {
	// Get the number of workloads before the restart.
	origWorkloads, err := c.Workloads()
	if err != nil {
		return fmt.Errorf("restart failed to get initial workloads: %v", err)
	}

	// Restart the deployment.
	if err := c.deployment.Restart(); err != nil {
		return err
	}

	// Wait until all pods are ready and match the original count.
	_, err = retry.Do(func() (result interface{}, completed bool, err error) {
		workloads, err := c.Workloads()
		if err != nil {
			return nil, false, err
		}

		// Make sure the number of pods matches the original.
		if len(workloads) != len(origWorkloads) {
			return nil, false, fmt.Errorf(
				"failed restarting echo %s/%s: number of pods %d does not match original %d",
				c.cfg.Namespace.Name(), c.cfg.Service, len(workloads), len(origWorkloads))
		}

		// We've successfully restarted.
		return nil, true, nil
	}, retry.Timeout(c.cfg.ReadinessTimeout), startDelay)
	return err
}
