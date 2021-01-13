// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-2020 Datadog, Inc.

// +build kubeapiserver,orchestrator

package orchestrator

import (
	"math/rand"
	"sync/atomic"
	"time"

	"github.com/DataDog/datadog-agent/pkg/config"
	"github.com/DataDog/datadog-agent/pkg/forwarder"
	"github.com/DataDog/datadog-agent/pkg/orchestrator"
	orchcfg "github.com/DataDog/datadog-agent/pkg/orchestrator/config"
	"github.com/DataDog/datadog-agent/pkg/util/kubernetes/apiserver"
	"github.com/DataDog/datadog-agent/pkg/util/kubernetes/apiserver/leaderelection"
	"github.com/DataDog/datadog-agent/pkg/util/kubernetes/clustername"
	"github.com/DataDog/datadog-agent/pkg/util/log"

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	appslisters "k8s.io/client-go/listers/apps/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
)

// ControllerContext holds necessary context for the controller
type ControllerContext struct {
	IsLeaderFunc                 func() bool
	UnassignedPodInformerFactory informers.SharedInformerFactory
	InformerFactory              informers.SharedInformerFactory
	Client                       kubernetes.Interface
	StopCh                       chan struct{}
	Hostname                     string
	ClusterName                  string
	ConfigPath                   string
}

// Controller is responsible of collecting & sending orchestrator info
type Controller struct {
	unassignedPodLister     corelisters.PodLister
	unassignedPodListerSync cache.InformerSynced
	deployLister            appslisters.DeploymentLister
	deployListerSync        cache.InformerSynced
	rsLister                appslisters.ReplicaSetLister
	rsListerSync            cache.InformerSynced
	serviceLister           corelisters.ServiceLister
	serviceListerSync       cache.InformerSynced
	nodesLister             corelisters.NodeLister
	nodesListerSync         cache.InformerSynced
	groupID                 int32
	hostName                string
	clusterName             string
	clusterID               string
	sender                  *orchestrator.Sender
	orchestratorConfig      *orchcfg.OrchestratorConfig
	isLeaderFunc            func() bool
}

// StartController starts the orchestrator controller
func StartController(ctx ControllerContext) error {
	if !config.Datadog.GetBool("orchestrator_explorer.enabled") {
		log.Info("Orchestrator explorer is disabled")
		return nil
	}

	if !config.Datadog.GetBool("leader_election") {
		return log.Errorf("Leader Election not enabled. Resource collection only happens on the leader nodes.")
	}

	if ctx.ClusterName == "" {
		log.Warn("Orchestrator explorer enabled but no cluster name set: disabling")
		return nil
	}
	orchestratorController, err := newController(ctx)
	if err != nil {
		log.Errorf("Error retrieving Kubernetes cluster ID: %v", err)
		return err
	}

	go orchestratorController.Run(ctx.StopCh)
	go orchestrator.Collector.Start(ctx.StopCh)

	ctx.UnassignedPodInformerFactory.Start(ctx.StopCh)
	ctx.InformerFactory.Start(ctx.StopCh)

	return apiserver.SyncInformers(map[apiserver.InformerName]cache.SharedInformer{
		apiserver.PodsInformer:        ctx.UnassignedPodInformerFactory.Core().V1().Pods().Informer(),
		apiserver.DeploysInformer:     ctx.InformerFactory.Apps().V1().Deployments().Informer(),
		apiserver.ReplicaSetsInformer: ctx.InformerFactory.Apps().V1().ReplicaSets().Informer(),
		apiserver.ServicesInformer:    ctx.InformerFactory.Core().V1().Services().Informer(),
		apiserver.NodesInformer:       ctx.InformerFactory.Core().V1().Nodes().Informer(),
	})
}

func newController(ctx ControllerContext) (*Controller, error) {
	podInformer := ctx.UnassignedPodInformerFactory.Core().V1().Pods()
	clusterID, err := clustername.GetClusterID()
	if err != nil {
		return nil, err
	}

	deployInformer := ctx.InformerFactory.Apps().V1().Deployments()
	rsInformer := ctx.InformerFactory.Apps().V1().ReplicaSets()
	serviceInformer := ctx.InformerFactory.Core().V1().Services()
	nodesInformer := ctx.InformerFactory.Core().V1().Nodes()

	orchestratorCfg := orchcfg.NewDefaultOrchestratorConfig()
	if err := orchestratorCfg.LoadYamlConfig(ctx.ConfigPath); err != nil {
		log.Errorf("Error loading the orchestrator config: %s", err)
	}

	orchestrator.InitManifestCollector(orchestratorCfg, ctx.Hostname)

	oc := &Controller{
		unassignedPodLister:     podInformer.Lister(),
		unassignedPodListerSync: podInformer.Informer().HasSynced,
		deployLister:            deployInformer.Lister(),
		deployListerSync:        deployInformer.Informer().HasSynced,
		rsLister:                rsInformer.Lister(),
		rsListerSync:            rsInformer.Informer().HasSynced,
		serviceLister:           serviceInformer.Lister(),
		serviceListerSync:       serviceInformer.Informer().HasSynced,
		nodesLister:             nodesInformer.Lister(),
		nodesListerSync:         nodesInformer.Informer().HasSynced,
		groupID:                 rand.Int31(),
		hostName:                ctx.Hostname,
		clusterName:             ctx.ClusterName,
		clusterID:               clusterID,
		orchestratorConfig:      orchestratorCfg,
		sender:                  orchestrator.NewSender(orchestratorCfg, ctx.Hostname),
		isLeaderFunc:            ctx.IsLeaderFunc,
	}

	return oc, nil
}

// Run starts the orchestrator controller
func (o *Controller) Run(stopCh <-chan struct{}) {
	log.Infof("Starting orchestrator controller")
	defer log.Infof("Stopping orchestrator controller")

	if err := o.runLeaderElection(); err != nil {
		log.Errorf("Error running the leader engine: %s", err)
		return
	}

	if err := o.sender.Start(); err != nil {
		log.Errorf("Error starting orchestrator forwarder: %s", err)
		return
	}
	defer o.sender.Stop()

	if !cache.WaitForCacheSync(stopCh, o.unassignedPodListerSync, o.deployListerSync, o.rsListerSync, o.serviceListerSync, o.nodesListerSync) {
		return
	}

	processors := []func(){
		o.processPods,
		o.processReplicaSets,
		o.processDeploys,
		o.processServices,
		o.processNodes,
	}

	spreadProcessors(processors, 2*time.Second, 10*time.Second, stopCh)

	<-stopCh
}

func (o *Controller) processDeploys() {
	if !o.isLeaderFunc() {
		return
	}

	deployList, err := o.deployLister.List(labels.Everything())
	if err != nil {
		log.Errorf("Unable to list deployments: %s", err)
		return
	}

	msg, err := processDeploymentList(deployList, atomic.AddInt32(&o.groupID, 1), o.orchestratorConfig, o.clusterID)
	if err != nil {
		log.Errorf("Unable to process deployment list: %v", err)
		return
	}

	stats := CheckStats{
		CacheHits: len(deployList) - len(msg),
		CacheMiss: len(msg),
		NodeType:  orchestrator.K8sDeployment,
	}

	orchestrator.KubernetesResourceCache.Set(BuildStatsKey(orchestrator.K8sDeployment), stats, orchestrator.NoExpiration)

	o.sender.SendMessages(msg, forwarder.PayloadTypeDeployment)
}

func (o *Controller) processReplicaSets() {
	if !o.isLeaderFunc() {
		return
	}

	rsList, err := o.rsLister.List(labels.Everything())
	if err != nil {
		log.Errorf("Unable to list replica sets: %s", err)
		return
	}

	msg, err := processReplicaSetList(rsList, atomic.AddInt32(&o.groupID, 1), o.orchestratorConfig, o.clusterID)
	if err != nil {
		log.Errorf("Unable to process replica set list: %v", err)
		return
	}

	stats := CheckStats{
		CacheHits: len(rsList) - len(msg),
		CacheMiss: len(msg),
		NodeType:  orchestrator.K8sReplicaSet,
	}

	orchestrator.KubernetesResourceCache.Set(BuildStatsKey(orchestrator.K8sReplicaSet), stats, orchestrator.NoExpiration)

	o.sender.SendMessages(msg, forwarder.PayloadTypeReplicaSet)
}

func (o *Controller) processPods() {
	if !o.isLeaderFunc() {
		return
	}

	podList, err := o.unassignedPodLister.List(labels.Everything())
	if err != nil {
		log.Errorf("Unable to list pods: %s", err)
		return
	}

	// we send an empty hostname for unassigned pods
	podMsgs, err := orchestrator.ProcessPodList(podList, atomic.AddInt32(&o.groupID, 1), "", o.clusterID, o.orchestratorConfig)
	if err != nil {
		log.Errorf("Unable to process pod list: %v", err)
		return
	}

	stats := CheckStats{
		CacheHits: len(podList) - len(podMsgs),
		CacheMiss: len(podMsgs),
		NodeType:  orchestrator.K8sPod,
	}

	orchestrator.KubernetesResourceCache.Set(BuildStatsKey(orchestrator.K8sPod), stats, orchestrator.NoExpiration)

	o.sender.SendMessages(podMsgs, forwarder.PayloadTypePod)
}

func (o *Controller) processServices() {
	if !o.isLeaderFunc() {
		return
	}

	serviceList, err := o.serviceLister.List(labels.Everything())
	if err != nil {
		log.Errorf("Unable to list services: %s", err)
	}
	groupID := atomic.AddInt32(&o.groupID, 1)

	messages, err := processServiceList(serviceList, groupID, o.orchestratorConfig, o.clusterID)
	if err != nil {
		log.Errorf("Unable to process service list: %s", err)
		return
	}

	stats := CheckStats{
		CacheHits: len(serviceList) - len(messages),
		CacheMiss: len(messages),
		NodeType:  orchestrator.K8sService,
	}

	orchestrator.KubernetesResourceCache.Set(BuildStatsKey(orchestrator.K8sService), stats, orchestrator.NoExpiration)

	o.sender.SendMessages(messages, forwarder.PayloadTypeService)
}

func (o *Controller) processNodes() {
	if !o.isLeaderFunc() {
		return
	}

	nodesList, err := o.nodesLister.List(labels.Everything())
	if err != nil {
		log.Errorf("Unable to list nodes: %s", err)
	}
	groupID := atomic.AddInt32(&o.groupID, 1)

	messages, err := processNodesList(nodesList, groupID, o.orchestratorConfig, o.clusterID)
	if err != nil {
		log.Errorf("Unable to process node list: %s", err)
		return
	}

	stats := CheckStats{
		CacheHits: len(nodesList) - len(messages),
		CacheMiss: len(messages),
		NodeType:  orchestrator.K8sNode,
	}

	orchestrator.KubernetesResourceCache.Set(BuildStatsKey(orchestrator.K8sNode), stats, orchestrator.NoExpiration)

	o.sender.SendMessages(messages, forwarder.PayloadTypeNode)
}

func (o *Controller) runLeaderElection() error {
	engine, err := leaderelection.GetLeaderEngine()
	if err != nil {
		return err
	}
	err = engine.EnsureLeaderElectionRuns()
	if err != nil {
		return err
	}
	return nil
}

func spreadProcessors(processors []func(), spreadInterval, processorPeriod time.Duration, stopCh <-chan struct{}) {
	for idx, p := range processors {
		processor := p
		time.AfterFunc(time.Duration(idx)*spreadInterval, func() {
			go wait.Until(processor, processorPeriod, stopCh)
		})
	}
}
