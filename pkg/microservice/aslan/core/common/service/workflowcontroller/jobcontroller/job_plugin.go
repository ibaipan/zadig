/*
Copyright 2022 The KodeRover Authors.

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

package jobcontroller

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"
	crClient "sigs.k8s.io/controller-runtime/pkg/client"

	zadigconfig "github.com/koderover/zadig/v2/pkg/config"
	"github.com/koderover/zadig/v2/pkg/microservice/aslan/config"
	commonmodels "github.com/koderover/zadig/v2/pkg/microservice/aslan/core/common/repository/models"
	commonrepo "github.com/koderover/zadig/v2/pkg/microservice/aslan/core/common/repository/mongodb"
	"github.com/koderover/zadig/v2/pkg/setting"
	"github.com/koderover/zadig/v2/pkg/tool/kube/updater"
)

type PluginJobCtl struct {
	job         *commonmodels.JobTask
	workflowCtx *commonmodels.WorkflowTaskCtx
	logger      *zap.SugaredLogger
	kubeclient  crClient.Client
	apiServer   crClient.Reader
	jobTaskSpec *commonmodels.JobTaskPluginSpec
	ack         func()
}

func NewPluginsJobCtl(job *commonmodels.JobTask, workflowCtx *commonmodels.WorkflowTaskCtx, ack func(), logger *zap.SugaredLogger) *PluginJobCtl {
	jobTaskSpec := &commonmodels.JobTaskPluginSpec{}
	if err := commonmodels.IToi(job.Spec, jobTaskSpec); err != nil {
		logger.Error(err)
	}
	job.Spec = jobTaskSpec
	return &PluginJobCtl{
		job:         job,
		workflowCtx: workflowCtx,
		logger:      logger,
		ack:         ack,
		jobTaskSpec: jobTaskSpec,
	}
}

func (c *PluginJobCtl) prepare(ctx context.Context) {
	// set default timeout
	if c.jobTaskSpec.Properties.Timeout <= 0 {
		c.jobTaskSpec.Properties.Timeout = 600
	}
	// set default resource
	if c.jobTaskSpec.Properties.ResourceRequest == setting.Request("") {
		c.jobTaskSpec.Properties.ResourceRequest = setting.MinRequest
	}
	// set default resource
	if c.jobTaskSpec.Properties.ClusterID == "" {
		c.jobTaskSpec.Properties.ClusterID = setting.LocalClusterID
	}
}

func (c *PluginJobCtl) Clean(ctx context.Context) {}

func (c *PluginJobCtl) Run(ctx context.Context) {
	c.prepare(ctx)
	if err := c.run(ctx); err != nil {
		return
	}
	c.wait(ctx)
	c.complete(ctx)
}

func (c *PluginJobCtl) run(ctx context.Context) error {
	// get kube client
	hubServerAddr := zadigconfig.HubServerServiceAddress()
	if c.jobTaskSpec.Properties.ClusterID == setting.LocalClusterID {
		c.jobTaskSpec.Properties.Namespace = zadigconfig.Namespace()
	} else {
		c.jobTaskSpec.Properties.Namespace = setting.AttachedClusterNamespace
	}

	crClient, _, apiServer, err := GetK8sClients(hubServerAddr, c.jobTaskSpec.Properties.ClusterID)
	if err != nil {
		logError(c.job, err.Error(), c.logger)
		return err
	}
	c.kubeclient = crClient
	c.apiServer = apiServer

	jobLabel := &JobLabel{
		JobType: string(c.job.JobType),
		JobName: c.job.K8sJobName,
	}
	c.jobTaskSpec.Properties.Registries = getMatchedRegistries(c.jobTaskSpec.Plugin.Image, c.jobTaskSpec.Properties.Registries)

	customAnnotation := make(map[string]string)
	customLabel := make(map[string]string)

	for _, lb := range c.jobTaskSpec.Properties.CustomLabels {
		customLabel[lb.Key] = lb.Value.(string)
	}
	for _, annotate := range c.jobTaskSpec.Properties.CustomAnnotations {
		customAnnotation[annotate.Key] = annotate.Value.(string)
	}

	job, err := buildPlainJob(c.job.K8sJobName, c.jobTaskSpec.Properties.ResourceRequest, c.jobTaskSpec.Properties.ResReqSpec, c.job, c.jobTaskSpec, c.workflowCtx, customLabel, customAnnotation)
	if err != nil {
		msg := fmt.Sprintf("create job context error: %v", err)
		logError(c.job, msg, c.logger)
		return err
	}

	job.Namespace = c.jobTaskSpec.Properties.Namespace

	if err := ensureDeleteJob(c.jobTaskSpec.Properties.Namespace, jobLabel, c.kubeclient); err != nil {
		msg := fmt.Sprintf("delete job error: %v", err)
		logError(c.job, msg, c.logger)
		return err
	}

	if err := createOrUpdateRegistrySecrets(c.jobTaskSpec.Properties.Namespace, c.jobTaskSpec.Properties.Registries, c.kubeclient); err != nil {
		msg := fmt.Sprintf("create secret error: %v", err)
		logError(c.job, msg, c.logger)
		return errors.New(msg)
	}

	if err := updater.CreateJob(job, c.kubeclient); err != nil {
		msg := fmt.Sprintf("create job error: %v", err)
		logError(c.job, msg, c.logger)
		return err
	}
	c.logger.Infof("succeed to create job %s", c.job.K8sJobName)
	return nil
}

func (c *PluginJobCtl) wait(ctx context.Context) {
	var err error
	timeout := time.After(time.Duration(c.jobTaskSpec.Properties.Timeout) * time.Minute)
	c.job.Status, err = waitJobStart(ctx, c.jobTaskSpec.Properties.Namespace, c.job.K8sJobName, c.kubeclient, c.apiServer, timeout, c.logger)
	if err != nil {
		c.logger.Errorf("wait job start error: %v", err)
	}
	if c.job.Status == config.StatusRunning {
		c.ack()
	} else {
		return
	}
	status := waitPlainJobEnd(ctx, int(c.jobTaskSpec.Properties.Timeout), timeout, c.jobTaskSpec.Properties.Namespace, c.job.K8sJobName, c.kubeclient, c.logger)
	c.job.Status = status
}

func (c *PluginJobCtl) complete(ctx context.Context) {
	jobLabel := &JobLabel{
		JobType: string(c.job.JobType),
		JobName: c.job.K8sJobName,
	}

	// 清理用户取消和超时的任务
	defer func() {
		go func() {
			if err := ensureDeleteJob(c.jobTaskSpec.Properties.Namespace, jobLabel, c.kubeclient); err != nil {
				c.logger.Error(err)
			}
		}()
	}()

	// get job outputs info from pod terminate message.
	if err := getJobOutputFromTerminalMsg(c.jobTaskSpec.Properties.Namespace, c.job.Name, c.job, c.workflowCtx, c.kubeclient); err != nil {
		c.logger.Error(err)
		c.job.Error = err.Error()
	}

	if err := saveContainerLog(c.jobTaskSpec.Properties.Namespace, c.jobTaskSpec.Properties.ClusterID, c.workflowCtx.WorkflowName, c.job.Name, c.workflowCtx.TaskID, jobLabel, c.kubeclient); err != nil {
		c.logger.Error(err)
		if c.job.Error == "" {
			c.job.Error = err.Error()
		}
		return
	}
}

func (c *PluginJobCtl) SaveInfo(ctx context.Context) error {
	return commonrepo.NewJobInfoColl().Create(context.TODO(), &commonmodels.JobInfo{
		Type:                c.job.JobType,
		WorkflowName:        c.workflowCtx.WorkflowName,
		WorkflowDisplayName: c.workflowCtx.WorkflowDisplayName,
		TaskID:              c.workflowCtx.TaskID,
		ProductName:         c.workflowCtx.ProjectName,
		StartTime:           c.job.StartTime,
		EndTime:             c.job.EndTime,
		Duration:            c.job.EndTime - c.job.StartTime,
		Status:              string(c.job.Status),
	})
}
