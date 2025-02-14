/*
 Licensed to the Apache Software Foundation (ASF) under one
 or more contributor license agreements.  See the NOTICE file
 distributed with this work for additional information
 regarding copyright ownership.  The ASF licenses this file
 to you under the Apache License, Version 2.0 (the
 "License"); you may not use this file except in compliance
 with the License.  You may obtain a copy of the License at

     http://www.apache.org/licenses/LICENSE-2.0

 Unless required by applicable law or agreed to in writing, software
 distributed under the License is distributed on an "AS IS" BASIS,
 WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 See the License for the specific language governing permissions and
 limitations under the License.
*/

package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"go.uber.org/zap"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/cache"
	podutil "k8s.io/kubernetes/pkg/api/v1/pod"

	"github.com/apache/incubator-yunikorn-k8shim/pkg/appmgmt/interfaces"
	schedulercache "github.com/apache/incubator-yunikorn-k8shim/pkg/cache/external"
	"github.com/apache/incubator-yunikorn-k8shim/pkg/client"
	"github.com/apache/incubator-yunikorn-k8shim/pkg/common"
	"github.com/apache/incubator-yunikorn-k8shim/pkg/common/constants"
	"github.com/apache/incubator-yunikorn-k8shim/pkg/common/events"
	"github.com/apache/incubator-yunikorn-k8shim/pkg/common/utils"
	"github.com/apache/incubator-yunikorn-k8shim/pkg/dispatcher"
	"github.com/apache/incubator-yunikorn-k8shim/pkg/log"
	"github.com/apache/incubator-yunikorn-k8shim/pkg/plugin/predicates"
	"github.com/apache/incubator-yunikorn-k8shim/pkg/plugin/support"
	"github.com/apache/incubator-yunikorn-scheduler-interface/lib/go/si"
)

// context maintains scheduling state, like apps and apps' tasks.
type Context struct {
	applications   map[string]*Application        // apps
	nodes          *schedulerNodes                // nodes
	schedulerCache *schedulercache.SchedulerCache // external cache
	apiProvider    client.APIProvider             // apis to interact with api-server, scheduler-core, etc
	predManager    predicates.PredicateManager    // K8s predicates
	lock           *sync.RWMutex                  // lock
}

// Create a new context for the scheduler.
func NewContext(apis client.APIProvider) *Context {
	// create the context note that order is important:
	// volumebinder needs the informers
	// the cache needs informers and volumebinder
	// nodecontroller needs the cache
	// predictor need the cache, volumebinder and informers
	ctx := &Context{
		applications: make(map[string]*Application),
		apiProvider:  apis,
		lock:         &sync.RWMutex{},
	}

	// create the cache
	ctx.schedulerCache = schedulercache.NewSchedulerCache(apis.GetAPIs())

	// init the controllers and plugins (need the cache)
	ctx.nodes = newSchedulerNodes(apis.GetAPIs().SchedulerAPI, ctx.schedulerCache)

	// create the predicate manager
	if !apis.IsTestingMode() {
		sharedLister := support.NewSharedLister(ctx.schedulerCache)
		clientSet := apis.GetAPIs().KubeClient.GetClientSet()
		informerFactory := apis.GetAPIs().InformerFactory
		ctx.predManager = predicates.NewPredicateManager(support.NewFrameworkHandle(sharedLister, informerFactory, clientSet))
	}

	return ctx
}

func (ctx *Context) AddSchedulingEventHandlers() {
	ctx.apiProvider.AddEventHandler(&client.ResourceEventHandlers{
		Type:     client.NodeInformerHandlers,
		AddFn:    ctx.addNode,
		UpdateFn: ctx.updateNode,
		DeleteFn: ctx.deleteNode,
	})

	ctx.apiProvider.AddEventHandler(&client.ResourceEventHandlers{
		Type:     client.PodInformerHandlers,
		FilterFn: ctx.filterPods,
		AddFn:    ctx.addPodToCache,
		UpdateFn: ctx.updatePodInCache,
		DeleteFn: ctx.removePodFromCache,
	})

	nodeCoordinator := newNodeResourceCoordinator(ctx.nodes)
	ctx.apiProvider.AddEventHandler(&client.ResourceEventHandlers{
		Type:     client.PodInformerHandlers,
		FilterFn: nodeCoordinator.filterPods,
		UpdateFn: nodeCoordinator.updatePod,
		DeleteFn: nodeCoordinator.deletePod,
	})

	ctx.apiProvider.AddEventHandler(&client.ResourceEventHandlers{
		Type:     client.ConfigMapInformerHandlers,
		FilterFn: ctx.filterConfigMaps,
		AddFn:    ctx.addConfigMaps,
		UpdateFn: ctx.updateConfigMaps,
		DeleteFn: ctx.deleteConfigMaps,
	})
}

func (ctx *Context) addNode(obj interface{}) {
	node, err := convertToNode(obj)
	if err != nil {
		log.Logger().Error("node conversion failed", zap.Error(err))
		return
	}

	// add node to secondary scheduler cache
	log.Logger().Debug("adding node to cache", zap.String("NodeName", node.Name))
	ctx.schedulerCache.AddNode(node)

	// add node to internal cache
	ctx.nodes.addNode(node)

	// post the event
	events.GetRecorder().Eventf(node, v1.EventTypeNormal, "NodeAccepted",
		fmt.Sprintf("node %s is accepted by the scheduler", node.Name))
}

func (ctx *Context) updateNode(oldObj, newObj interface{}) {
	// we only trigger update when resource changes
	oldNode, err := convertToNode(oldObj)
	if err != nil {
		log.Logger().Error("old node conversion failed",
			zap.Error(err))
		return
	}

	newNode, err := convertToNode(newObj)
	if err != nil {
		log.Logger().Error("new node conversion failed",
			zap.Error(err))
		return
	}

	// update secondary cache
	if err := ctx.schedulerCache.UpdateNode(oldNode, newNode); err != nil {
		log.Logger().Error("unable to update node in scheduler cache",
			zap.Error(err))
		return
	}

	// update primary cache
	ctx.nodes.updateNode(oldNode, newNode)
}

func (ctx *Context) deleteNode(obj interface{}) {
	var node *v1.Node
	switch t := obj.(type) {
	case *v1.Node:
		node = t
	case cache.DeletedFinalStateUnknown:
		var ok bool
		node, ok = t.Obj.(*v1.Node)
		if !ok {
			log.Logger().Error("cannot convert to *v1.Node", zap.Any("object", t.Obj))
			return
		}
	default:
		log.Logger().Error("cannot convert to *v1.Node", zap.Any("object", t))
		return
	}

	// delete node from secondary cache
	log.Logger().Debug("delete node from cache", zap.String("nodeName", node.Name))
	if err := ctx.schedulerCache.RemoveNode(node); err != nil {
		log.Logger().Error("unable to delete node from scheduler cache",
			zap.Error(err))
		return
	}

	// delete node from primary cache
	ctx.nodes.deleteNode(node)

	// post the event
	events.GetRecorder().Eventf(node, v1.EventTypeNormal, "NodeDeleted",
		fmt.Sprintf("node %s is deleted from the scheduler", node.Name))
}

func (ctx *Context) addPodToCache(obj interface{}) {
	pod, err := utils.Convert2Pod(obj)
	if err != nil {
		log.Logger().Error("failed to add pod to cache", zap.Error(err))
		return
	}

	log.Logger().Debug("adding pod to cache", zap.String("podName", pod.Name))
	if err := ctx.schedulerCache.AddPod(pod); err != nil {
		log.Logger().Error("add pod to scheduler cache failed",
			zap.String("podName", pod.Name),
			zap.Error(err))
	}
}

func (ctx *Context) removePodFromCache(obj interface{}) {
	var pod *v1.Pod
	switch t := obj.(type) {
	case *v1.Pod:
		pod = t
	case cache.DeletedFinalStateUnknown:
		var ok bool
		pod, ok = t.Obj.(*v1.Pod)
		if !ok {
			log.Logger().Error("Cannot convert to *v1.Pod", zap.Any("pod", obj))
			return
		}
	default:
		log.Logger().Error("Cannot convert to *v1.Pod", zap.Any("pod", obj))
		return
	}

	log.Logger().Debug("removing pod from cache", zap.String("podName", pod.Name))
	if err := ctx.schedulerCache.RemovePod(pod); err != nil {
		log.Logger().Debug("failed to remove pod from scheduler cache",
			zap.String("podName", pod.Name),
			zap.Error(err))
	}
}

func (ctx *Context) updatePodInCache(oldObj, newObj interface{}) {
	oldPod, err := utils.Convert2Pod(oldObj)
	if err != nil {
		log.Logger().Error("failed to update pod in cache", zap.Error(err))
		return
	}
	newPod, err := utils.Convert2Pod(newObj)
	if err != nil {
		log.Logger().Error("failed to update pod in cache", zap.Error(err))
		return
	}

	if err := ctx.schedulerCache.UpdatePod(oldPod, newPod); err != nil {
		log.Logger().Debug("failed to update pod in cache",
			zap.String("podName", oldPod.Name),
			zap.Error(err))
	}
}

// filter pods by scheduler name and state
func (ctx *Context) filterPods(obj interface{}) bool {
	switch obj := obj.(type) {
	case *v1.Pod:
		// if a terminated pod is added to cache, it will
		// add requested resource to the cached node, causing
		// the node uses more resources that it actually is,
		// this can only be fixed after the pod is removed.
		// (trigger the delete pod)
		return utils.GeneralPodFilter(obj) &&
			!utils.IsPodTerminated(obj)
	default:
		return false
	}
}

// filter configMap for the scheduler
func (ctx *Context) filterConfigMaps(obj interface{}) bool {
	switch obj := obj.(type) {
	case *v1.ConfigMap:
		return obj.Name == constants.DefaultConfigMapName
	default:
		return false
	}
}

// when detects the configMap for the scheduler is added, trigger hot-refresh
func (ctx *Context) addConfigMaps(obj interface{}) {
	log.Logger().Debug("configMap added")
	ctx.triggerReloadConfig()
}

// when detects the configMap for the scheduler is updated, trigger hot-refresh
func (ctx *Context) updateConfigMaps(obj, newObj interface{}) {
	if ctx.apiProvider.GetAPIs().Conf.EnableConfigHotRefresh {
		log.Logger().Debug("trigger scheduler to reload configuration")
		// When update event is received, it is not guaranteed the data mounted to the pod
		// is also updated. This is because the actual update in pod's volume is ensured
		// by kubelet, kubelet is checking whether the mounted ConfigMap is fresh on every
		// periodic sync. As a result, the total delay from the moment when the ConfigMap
		// is updated to the moment when new keys are projected to the pod can be as long
		// as kubelet sync period + ttl of ConfigMaps cache in kubelet.
		// We trigger configuration reload, on yunikorn-core side, it keeps checking config
		// file state once this is called. And the actual reload happens when it detects
		// actual changes on the content.
		ctx.triggerReloadConfig()
	} else {
		log.Logger().Warn("Skip to reload scheduler configuration")
	}
}

// when detects the configMap for the scheduler is deleted, no operation needed here
// we assume there will be a consequent add operation after delete, so we treat it like a update.
func (ctx *Context) deleteConfigMaps(obj interface{}) {
	log.Logger().Debug("configMap deleted")
}

func (ctx *Context) triggerReloadConfig() {
	log.Logger().Info("trigger scheduler configuration reloading")
	clusterId := ctx.apiProvider.GetAPIs().Conf.ClusterID
	if err := ctx.apiProvider.GetAPIs().SchedulerAPI.UpdateConfiguration(clusterId); err != nil {
		log.Logger().Error("reload configuration failed", zap.Error(err))
	}
}

// evaluate given predicates based on current context
func (ctx *Context) IsPodFitNode(name, node string, allocate bool) error {
	// simply skip if predicates are not enabled
	if ctx.apiProvider.IsTestingMode() {
		return nil
	}

	ctx.lock.RLock()
	defer ctx.lock.RUnlock()
	if pod, ok := ctx.schedulerCache.GetPod(name); ok {
		// if pod exists in cache, try to run predicates
		if targetNode := ctx.schedulerCache.GetNode(node); targetNode != nil {
			_, err := ctx.predManager.Predicates(pod, targetNode, allocate)
			return err
		}
	}
	return fmt.Errorf("predicates were not running because pod or node was not found in cache")
}

// call volume binder to bind pod volumes if necessary,
// internally, volume binder maintains a cache (podBindingCache) for pod volumes,
// and before calling this, they should have been updated by FindPodVolumes and AssumePodVolumes.
func (ctx *Context) bindPodVolumes(pod *v1.Pod) error {
	podKey := string(pod.UID)
	// the assumePodVolumes was done in scheduler-core, because these assumed pods are cached
	// during scheduling process as they have directly impact to other scheduling processes.
	// when assumePodVolumes was called, we caches the value if all pod volumes are bound in schedulerCache,
	// then here we just need to retrieve that value from cache, to skip bindings if volumes are already bound.
	if assumedPod, exist := ctx.schedulerCache.GetPod(podKey); exist {
		if ctx.schedulerCache.ArePodVolumesAllBound(podKey) {
			log.Logger().Info("Binding Pod Volumes skipped: all volumes already bound",
				zap.String("podName", pod.Name))
		} else {
			log.Logger().Info("Binding Pod Volumes", zap.String("podName", pod.Name))
			boundClaims, claimsToBind, _, err := ctx.apiProvider.GetAPIs().VolumeBinder.GetPodVolumes(assumedPod)
			if err != nil {
				return err
			}
			node, err := ctx.schedulerCache.GetNodeInfo(assumedPod.Spec.NodeName)
			if err != nil {
				return err
			}
			volumes, _, err := ctx.apiProvider.GetAPIs().VolumeBinder.FindPodVolumes(assumedPod, boundClaims, claimsToBind, node)
			if err != nil {
				return err
			}
			return ctx.apiProvider.GetAPIs().VolumeBinder.BindPodVolumes(assumedPod, volumes)
		}
	}
	return nil
}

// assume a pod will be running on a node, in scheduler, we maintain
// a cache where stores info for each node what pods are supposed to
// be running on it. And we keep this cache in-sync between core and the shim.
// this way, the core can make allocation decisions with consideration of
// other assumed pods before they are actually bound to the node (bound is slow).
func (ctx *Context) AssumePod(name string, node string) error {
	ctx.lock.Lock()
	defer ctx.lock.Unlock()
	if pod, ok := ctx.schedulerCache.GetPod(name); ok {
		// when add assumed pod, we make a copy of the pod to avoid
		// modifying its original reference. otherwise, it may have
		// race when some other go-routines accessing it in parallel.
		if targetNode := ctx.schedulerCache.GetNode(node); targetNode != nil {
			assumedPod := pod.DeepCopy()
			// assume pod volumes, this will update bindings info in cache
			// assume pod volumes before assuming the pod
			// this will update scheduler cache with essential PV/PVC binding info
			var allBound = true
			// volume builder might be null in UTs
			if ctx.apiProvider.GetAPIs().VolumeBinder != nil {
				var err error
				boundClaims, claimsToBind, _, err := ctx.apiProvider.GetAPIs().VolumeBinder.GetPodVolumes(assumedPod)
				if err != nil {
					return err
				}
				volumes, _, err := ctx.apiProvider.GetAPIs().VolumeBinder.FindPodVolumes(pod, boundClaims, claimsToBind, targetNode.Node())
				if err != nil {
					return err
				}
				allBound, err = ctx.apiProvider.GetAPIs().VolumeBinder.AssumePodVolumes(pod, node, volumes)
				if err != nil {
					return err
				}
			}
			// assign the node name for pod
			assumedPod.Spec.NodeName = node
			return ctx.schedulerCache.AssumePod(assumedPod, allBound)
		}
	}
	return nil
}

// forget pod must be called when a pod is assumed to be running on a node,
// but then for some reason it is failed to bind or released.
func (ctx *Context) ForgetPod(name string) error {
	ctx.lock.Lock()
	defer ctx.lock.Unlock()

	if pod, ok := ctx.schedulerCache.GetPod(name); ok {
		log.Logger().Debug("forget pod", zap.String("pod", pod.Name))
		return ctx.schedulerCache.ForgetPod(pod)
	}
	log.Logger().Debug("unable to forget pod",
		zap.String("reason", fmt.Sprintf("pod %s not found in scheduler cache", name)))
	return nil
}

func (ctx *Context) UpdateApplication(app *Application) {
	ctx.lock.Lock()
	defer ctx.lock.Unlock()
	ctx.applications[app.applicationID] = app
}

// inform the scheduler that the application is completed,
// the complete state may further explained to completed_with_errors(failed) or successfully_completed,
// either way we need to release all allocations (if exists) for this application
func (ctx *Context) NotifyApplicationComplete(appID string) {
	if app := ctx.GetApplication(appID); app != nil {
		log.Logger().Debug("NotifyApplicationComplete",
			zap.String("appID", appID),
			zap.String("currentAppState", app.GetApplicationState()))
		ev := NewSimpleApplicationEvent(appID, events.CompleteApplication)
		dispatcher.Dispatch(ev)
	}
}

func (ctx *Context) NotifyApplicationFail(appID string) {
	if app := ctx.GetApplication(appID); app != nil {
		log.Logger().Debug("NotifyApplicationFail",
			zap.String("appID", appID),
			zap.String("currentAppState", app.GetApplicationState()))
		ev := NewSimpleApplicationEvent(appID, events.FailApplication)
		dispatcher.Dispatch(ev)
	}
}

func (ctx *Context) NotifyTaskComplete(appID, taskID string) {
	log.Logger().Debug("NotifyTaskComplete",
		zap.String("appID", appID),
		zap.String("taskID", taskID))
	if app := ctx.GetApplication(appID); app != nil {
		log.Logger().Debug("release allocation",
			zap.String("appID", appID),
			zap.String("taskID", taskID))
		ev := NewSimpleTaskEvent(appID, taskID, events.CompleteTask)
		dispatcher.Dispatch(ev)
		appEv := NewSimpleApplicationEvent(appID, events.AppTaskCompleted)
		dispatcher.Dispatch(appEv)
	}
}

// update application tags in the AddApplicationRequest based on the namespace annotation
// adds the following tags to the request based on annotations (if exist):
//    - namespace.resourcequota
//    - namespace.parentqueue
func (ctx *Context) updateApplicationTags(request *interfaces.AddApplicationRequest, namespace string) {
	namespaceObj := ctx.getNamespaceObject(namespace)
	if namespaceObj == nil {
		return
	}
	// add resource quota info as an app tag
	resourceQuota := utils.GetNamespaceQuotaFromAnnotation(namespaceObj)
	if resourceQuota != nil && !common.IsZero(resourceQuota) {
		if quotaStr, err := json.Marshal(resourceQuota); err == nil {
			request.Metadata.Tags[constants.AppTagNamespaceResourceQuota] = string(quotaStr)
		}
	}
	// add parent queue info as an app tag
	parentQueue := namespaceObj.Annotations["yunikorn.apache.org/parentqueue"]
	if parentQueue != "" {
		request.Metadata.Tags[constants.AppTagNamespaceParentQueue] = parentQueue
	}
}

// returns the namespace object from the namespace's name
// if the namespace is unable to be listed from api-server, a nil is returned
func (ctx *Context) getNamespaceObject(namespace string) *v1.Namespace {
	if namespace == "" {
		log.Logger().Debug("could not get namespace from empty string")
		return nil
	}

	nsLister := ctx.apiProvider.GetAPIs().NamespaceInformer.Lister()
	namespaceObj, err := nsLister.Get(namespace)
	if err != nil {
		// every app should belong to a namespace,
		// if we cannot list the namespace here, probably something is wrong
		// log an error here and skip retrieving the resource quota
		log.Logger().Error("failed to get app namespace", zap.Error(err))
		return nil
	}
	return namespaceObj
}

func (ctx *Context) AddApplication(request *interfaces.AddApplicationRequest) interfaces.ManagedApp {
	log.Logger().Debug("AddApplication", zap.Any("Request", request))
	if app := ctx.GetApplication(request.Metadata.ApplicationID); app != nil {
		return app
	}

	ctx.lock.Lock()
	defer ctx.lock.Unlock()

	if ns, ok := request.Metadata.Tags[constants.AppTagNamespace]; ok {
		log.Logger().Debug("app namespace info",
			zap.String("appID", request.Metadata.ApplicationID),
			zap.String("namespace", ns))
		ctx.updateApplicationTags(request, ns)
	}

	app := NewApplication(
		request.Metadata.ApplicationID,
		request.Metadata.QueueName,
		request.Metadata.User,
		request.Metadata.Tags,
		ctx.apiProvider.GetAPIs().SchedulerAPI)
	app.setTaskGroups(request.Metadata.TaskGroups)
	if request.Metadata.SchedulingPolicyParameters != nil {
		app.SetPlaceholderTimeout(request.Metadata.SchedulingPolicyParameters.GetPlaceholderTimeout())
		app.setSchedulingStyle(request.Metadata.SchedulingPolicyParameters.GetGangSchedulingStyle())
	}
	app.setOwnReferences(request.Metadata.OwnerReferences)

	// add into cache
	ctx.applications[app.applicationID] = app
	log.Logger().Info("app added",
		zap.String("appID", app.applicationID))

	return app
}

func (ctx *Context) GetApplication(appID string) interfaces.ManagedApp {
	ctx.lock.RLock()
	defer ctx.lock.RUnlock()
	if app, ok := ctx.applications[appID]; ok {
		return app
	}
	return nil
}

func (ctx *Context) RemoveApplication(appID string) error {
	ctx.lock.Lock()
	defer ctx.lock.Unlock()
	if app, exist := ctx.applications[appID]; exist {
		//get the non-terminated task alias
		nonTerminatedTaskAlias := app.getNonTerminatedTaskAlias()
		// check there are any non-terminated task or not
		if len(nonTerminatedTaskAlias) > 0 {
			return fmt.Errorf("failed to remove application %s because it still has task in non-terminated task, tasks: %s", appID, strings.Join(nonTerminatedTaskAlias, ","))
		}
		// send the update request to scheduler core
		rr := common.CreateUpdateRequestForRemoveApplication(app.applicationID, app.partition)
		if err := ctx.apiProvider.GetAPIs().SchedulerAPI.UpdateApplication(&rr); err != nil {
			log.Logger().Error("failed to send remove application request to core", zap.Error(err))
		}
		delete(ctx.applications, appID)
		log.Logger().Info("app removed",
			zap.String("appID", appID))

		return nil
	}
	return fmt.Errorf("application %s is not found in the context", appID)
}

func (ctx *Context) RemoveApplicationInternal(appID string) error {
	ctx.lock.Lock()
	defer ctx.lock.Unlock()
	if _, exist := ctx.applications[appID]; exist {
		delete(ctx.applications, appID)
		return nil
	}
	return fmt.Errorf("application %s is not found in the context", appID)
}

// this implements ApplicationManagementProtocol
func (ctx *Context) AddTask(request *interfaces.AddTaskRequest) interfaces.ManagedTask {
	log.Logger().Debug("AddTask",
		zap.String("appID", request.Metadata.ApplicationID),
		zap.String("taskID", request.Metadata.TaskID))
	if managedApp := ctx.GetApplication(request.Metadata.ApplicationID); managedApp != nil {
		if app, valid := managedApp.(*Application); valid {
			existingTask, err := app.GetTask(request.Metadata.TaskID)
			if err != nil {
				task := NewFromTaskMeta(request.Metadata.TaskID, app, ctx, request.Metadata)
				app.addTask(task)
				log.Logger().Info("task added",
					zap.String("appID", app.applicationID),
					zap.String("taskID", task.taskID),
					zap.String("taskState", task.GetTaskState()))

				return task
			}
			return existingTask
		}
	}
	return nil
}

func (ctx *Context) RemoveTask(appID, taskID string) error {
	ctx.lock.RLock()
	defer ctx.lock.RUnlock()
	if app, ok := ctx.applications[appID]; ok {
		return app.removeTask(taskID)
	}
	return fmt.Errorf("application %s is not found in the context", appID)
}

func (ctx *Context) getTask(appID string, taskID string) (*Task, error) {
	ctx.lock.RLock()
	defer ctx.lock.RUnlock()
	if app, ok := ctx.applications[appID]; ok {
		if managedTask, err := app.GetTask(taskID); err == nil {
			if task, valid := managedTask.(*Task); valid {
				return task, nil
			}
		}
	}
	return nil, fmt.Errorf("application %s is not found in context", appID)
}

func (ctx *Context) SelectApplications(filter func(app *Application) bool) []*Application {
	ctx.lock.RLock()
	defer ctx.lock.RUnlock()

	apps := make([]*Application, 0)
	for _, app := range ctx.applications {
		if filter != nil && !filter(app) {
			continue
		}
		apps = append(apps, app)
	}

	return apps
}

func (ctx *Context) PublishEvents(eventRecords []*si.EventRecord) {
	if len(eventRecords) > 0 {
		for _, record := range eventRecords {
			switch record.Type {
			case si.EventRecord_REQUEST:
				taskID := record.ObjectID
				appID := record.GroupID
				if task, err := ctx.getTask(appID, taskID); err == nil {
					events.GetRecorder().Event(task.GetTaskPod(),
						v1.EventTypeNormal, record.Reason, record.Message)
				} else {
					log.Logger().Warn("task event is not published because task is not found",
						zap.String("appID", appID),
						zap.String("taskID", taskID),
						zap.String("event", record.String()))
				}
			case si.EventRecord_NODE:
				nodeID := record.ObjectID
				nodeInfo := ctx.schedulerCache.GetNode(nodeID)
				if nodeInfo == nil {
					log.Logger().Warn("node event is not published because nodeInfo is not found",
						zap.String("nodeID", nodeID),
						zap.String("event", record.String()))
					continue
				}
				node := nodeInfo.Node()
				if node == nil {
					log.Logger().Warn("node event is not published because node is not found",
						zap.String("nodeID", nodeID),
						zap.String("event", record.String()))
					continue
				}
				events.GetRecorder().Event(node,
					v1.EventTypeNormal, record.Reason, record.Message)
			default:
				log.Logger().Warn("Unsupported event type, currently only supports to publish request event records",
					zap.String("type", record.Type.String()))
			}
		}
	}
}

// update task's pod condition when the condition has not yet updated,
// return true if the update was done and false if the update is skipped due to any error, or a dup operation
func (ctx *Context) updatePodCondition(task *Task, podCondition *v1.PodCondition) bool {
	if task.GetTaskState() == events.States().Task.Scheduling {
		// only update the pod when pod condition changes
		// minimize the overhead added to the api-server/etcd
		if !utils.PodUnderCondition(task.pod, podCondition) {
			log.Logger().Debug("updating pod condition",
				zap.String("namespace", task.pod.Namespace),
				zap.String("name", task.pod.Name),
				zap.Any("podCondition", podCondition))
			// call api-server to do the pod condition update
			if podutil.UpdatePodCondition(&task.pod.Status, podCondition) {
				if !ctx.apiProvider.IsTestingMode() {
					_, err := ctx.apiProvider.GetAPIs().KubeClient.GetClientSet().CoreV1().
						Pods(task.pod.Namespace).UpdateStatus(context.Background(), task.pod, metav1.UpdateOptions{})
					if err == nil {
						return true
					}
					// only log the error here, no need to handle it if the update failed
					log.Logger().Error("update pod condition failed",
						zap.Error(err))
				}
			}
		}
	}
	return false
}

// this function handles the pod scheduling failures with respect to the different causes,
// and update the pod condition accordingly. the cluster autoscaler depends on the certain
// pod condition in order to trigger auto-scaling.
func (ctx *Context) HandleContainerStateUpdate(request *si.UpdateContainerSchedulingStateRequest) {
	// the allocationKey equals to the taskID
	if task, err := ctx.getTask(request.ApplicartionID, request.AllocationKey); err == nil {
		switch request.State {
		case si.UpdateContainerSchedulingStateRequest_SKIPPED:
			// auto-scaler scans pods whose pod condition is PodScheduled=false && reason=Unschedulable
			// if the pod is skipped because the queue quota has been exceed, we do not trigger the auto-scaling
			if ctx.updatePodCondition(task,
				&v1.PodCondition{
					Type:    v1.PodScheduled,
					Status:  v1.ConditionFalse,
					Reason:  "SchedulingSkipped",
					Message: request.Reason,
				}) {
				events.GetRecorder().Eventf(task.pod,
					v1.EventTypeNormal, "PodUnschedulable",
					"Task %s is skipped from scheduling because the queue quota has been exceed", task.alias)
			}
		case si.UpdateContainerSchedulingStateRequest_FAILED:
			// set pod condition to Unschedulable in order to trigger auto-scaling
			if ctx.updatePodCondition(task,
				&v1.PodCondition{
					Type:    v1.PodScheduled,
					Status:  v1.ConditionFalse,
					Reason:  v1.PodReasonUnschedulable,
					Message: request.Reason,
				}) {
				events.GetRecorder().Eventf(task.pod,
					v1.EventTypeNormal, "PodUnschedulable",
					"Task %s is pending for the requested resources become available", task.alias)
			}
		default:
			log.Logger().Warn("no handler for container scheduling state",
				zap.String("state", request.State.String()))
		}
	}
}

func (ctx *Context) ApplicationEventHandler() func(obj interface{}) {
	return func(obj interface{}) {
		if event, ok := obj.(events.ApplicationEvent); ok {
			managedApp := ctx.GetApplication(event.GetApplicationID())
			if managedApp == nil {
				log.Logger().Error("failed to handle application event",
					zap.String("reason", "application not exist"))
				return
			}

			if app, ok := managedApp.(*Application); ok {
				if app.canHandle(event) {
					if err := app.handle(event); err != nil {
						log.Logger().Error("failed to handle application event",
							zap.String("event", string(event.GetEvent())),
							zap.Error(err))
					}
				}
			}
		}
	}
}

func (ctx *Context) TaskEventHandler() func(obj interface{}) {
	return func(obj interface{}) {
		if event, ok := obj.(events.TaskEvent); ok {
			task, err := ctx.getTask(event.GetApplicationID(), event.GetTaskID())
			if err != nil {
				log.Logger().Error("failed to handle application event", zap.Error(err))
				return
			}

			if task.canHandle(event) {
				if err = task.handle(event); err != nil {
					log.Logger().Error("failed to handle task event",
						zap.String("applicationID", task.applicationID),
						zap.String("taskID", task.taskID),
						zap.String("event", string(event.GetEvent())),
						zap.Error(err))
				}
			}
		}
	}
}

func (ctx *Context) SchedulerNodeEventHandler() func(obj interface{}) {
	if ctx != nil && ctx.nodes != nil {
		return ctx.nodes.schedulerNodeEventHandler()
	}
	// this is not required in some tests
	return nil
}

func findYKConfigMap(configMaps []*v1.ConfigMap) (*v1.ConfigMap, error) {
	if len(configMaps) == 0 {
		return nil, fmt.Errorf("configmap with label app:yunikorn not found")
	}
	for _, c := range configMaps {
		if c.Name == constants.DefaultConfigMapName {
			return c, nil
		}
	}
	return nil, fmt.Errorf("configmap with name %s not found", constants.DefaultConfigMapName)
}

/*
Save the configmap and returns the old one and an error if the process failed
*/
func (ctx *Context) SaveConfigmap(request *si.UpdateConfigurationRequest) *si.UpdateConfigurationResponse {
	// if hot-refresh is enabled, configMap change through the API is not allowed
	if ctx.apiProvider.GetAPIs().Conf.EnableConfigHotRefresh {
		return &si.UpdateConfigurationResponse{
			Success: false,
			Reason: fmt.Sprintf("hot-refresh is enabled. To use the API for configuration update, " +
				"set enableConfigHotRefresh = false and restart the scheduler"),
		}
	}
	slt := labels.SelectorFromSet(labels.Set{constants.LabelApp: "yunikorn"})

	configMaps, err := ctx.apiProvider.GetAPIs().ConfigMapInformer.Lister().List(slt)
	if err != nil {
		return &si.UpdateConfigurationResponse{
			Success: false,
			Reason:  err.Error(),
		}
	}
	ykconf, err := findYKConfigMap(configMaps)
	if err != nil {
		return &si.UpdateConfigurationResponse{
			Success: false,
			Reason:  err.Error(),
		}
	}

	newConfData := map[string]string{"queues.yaml": strings.ReplaceAll(request.Configs, "\r\n", "\n")}
	newConf := ykconf.DeepCopy()
	oldConfData := ykconf.Data["queues.yaml"]
	newConf.Data = newConfData
	_, err = ctx.apiProvider.GetAPIs().KubeClient.GetClientSet().CoreV1().ConfigMaps(ykconf.Namespace).Update(context.Background(), newConf, metav1.UpdateOptions{})
	if err != nil {
		return &si.UpdateConfigurationResponse{
			Success: false,
			Reason:  err.Error(),
		}
	}
	log.Logger().Info("ConfigMap updated successfully")
	return &si.UpdateConfigurationResponse{
		Success:   true,
		OldConfig: oldConfData,
	}
}
