package operator

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"time"

	"github.com/ghodss/yaml"
	"github.com/golang/glog"
	cvoclientset "github.com/openshift/cluster-version-operator/pkg/generated/clientset/versioned"

	"k8s.io/api/core/v1"
	apiextclientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apiextinformersv1beta1 "k8s.io/apiextensions-apiserver/pkg/client/informers/externalversions/apiextensions/v1beta1"
	apiextlistersv1beta1 "k8s.io/apiextensions-apiserver/pkg/client/listers/apiextensions/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	appsinformersv1 "k8s.io/client-go/informers/apps/v1"
	coreinformersv1 "k8s.io/client-go/informers/core/v1"
	rbacinformersv1 "k8s.io/client-go/informers/rbac/v1"
	"k8s.io/client-go/kubernetes"
	coreclientsetv1 "k8s.io/client-go/kubernetes/typed/core/v1"
	appslisterv1 "k8s.io/client-go/listers/apps/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"

	"github.com/openshift/machine-api-operator/pkg/render"

	apiregistrationclientset "k8s.io/kube-aggregator/pkg/client/clientset_generated/clientset"
	"sigs.k8s.io/cluster-api/pkg/client/clientset_generated/clientset"
	"sigs.k8s.io/cluster-api/pkg/client/clientset_generated/clientset/scheme"
	clusterapiinformersv1alpha1 "sigs.k8s.io/cluster-api/pkg/client/informers_generated/externalversions/cluster/v1alpha1"
	clusterapilisterv1alpha1 "sigs.k8s.io/cluster-api/pkg/client/listers_generated/cluster/v1alpha1"
)

const (
	// maxRetries is the number of times a machineconfig pool will be retried before it is dropped out of the queue.
	// With the current rate-limiter in use (5ms*2^(maxRetries-1)) the following numbers represent the times
	// a machineconfig pool is going to be requeued:
	//
	// 5ms, 10ms, 20ms, 40ms, 80ms, 160ms, 320ms, 640ms, 1.3s, 2.6s, 5.1s, 10.2s, 20.4s, 41s, 82s
	maxRetries        = 15
	providerAWS       = "aws"
	providerLibvirt   = "libvirt"
	ownedManifestsDir = "owned-manifests"
)

// Operator defines machince config operator.
type Operator struct {
	namespace, name string

	imagesFile string
	config     string

	clusterAPIClient      clientset.Interface
	kubeClient            kubernetes.Interface
	apiExtClient          apiextclientset.Interface
	apiregistrationClient apiregistrationclientset.Interface
	cvoClient             cvoclientset.Interface
	eventRecorder         record.EventRecorder

	syncHandler func(ic string) error

	crdLister        apiextlistersv1beta1.CustomResourceDefinitionLister
	machineSetLister clusterapilisterv1alpha1.MachineSetLister
	deployLister     appslisterv1.DeploymentLister

	crdListerSynced       cache.InformerSynced
	machineSetSynced      cache.InformerSynced
	deployListerSynced    cache.InformerSynced
	daemonsetListerSynced cache.InformerSynced

	// queue only ever has one item, but it has nice error handling backoff/retry semantics
	queue workqueue.RateLimitingInterface
}

// New returns a new machine config operator.
func New(
	namespace, name string,
	imagesFile string,

	config string,

	machineSetInformer clusterapiinformersv1alpha1.MachineSetInformer,
	configMapInformer coreinformersv1.ConfigMapInformer,
	serviceAccountInfomer coreinformersv1.ServiceAccountInformer,
	crdInformer apiextinformersv1beta1.CustomResourceDefinitionInformer,
	deployInformer appsinformersv1.DeploymentInformer,
	clusterRoleInformer rbacinformersv1.ClusterRoleInformer,
	clusterRoleBindingInformer rbacinformersv1.ClusterRoleBindingInformer,

	kubeClient kubernetes.Interface,
	apiExtClient apiextclientset.Interface,
	apiregistrationClient apiregistrationclientset.Interface,
	cvoClient cvoclientset.Interface,
	clusterAPIClient clientset.Interface,
) *Operator {
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(glog.Infof)
	eventBroadcaster.StartRecordingToSink(&coreclientsetv1.EventSinkImpl{Interface: kubeClient.CoreV1().Events("")})

	optr := &Operator{
		namespace:             namespace,
		name:                  name,
		imagesFile:            imagesFile,
		clusterAPIClient:      clusterAPIClient,
		kubeClient:            kubeClient,
		apiExtClient:          apiExtClient,
		apiregistrationClient: apiregistrationClient,
		cvoClient:             cvoClient,
		eventRecorder:         eventBroadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: "machineapioperator"}),
		queue:                 workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "machineapioperator"),
	}

	machineSetInformer.Informer().AddEventHandler(optr.eventHandler())
	configMapInformer.Informer().AddEventHandler(optr.eventHandler())
	serviceAccountInfomer.Informer().AddEventHandler(optr.eventHandler())
	crdInformer.Informer().AddEventHandler(optr.eventHandler())
	deployInformer.Informer().AddEventHandler(optr.eventHandler())
	clusterRoleInformer.Informer().AddEventHandler(optr.eventHandler())
	clusterRoleBindingInformer.Informer().AddEventHandler(optr.eventHandler())

	optr.config = config
	optr.syncHandler = optr.sync

	optr.crdLister = crdInformer.Lister()
	optr.crdListerSynced = crdInformer.Informer().HasSynced
	optr.machineSetLister = machineSetInformer.Lister()
	optr.machineSetSynced = machineSetInformer.Informer().HasSynced
	optr.deployLister = deployInformer.Lister()
	optr.deployListerSynced = deployInformer.Informer().HasSynced

	return optr
}

// Run runs the machine config operator.
func (optr *Operator) Run(workers int, stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer optr.queue.ShutDown()

	glog.Info("Starting MachineAPIOperator")
	defer glog.Info("Shutting down MachineAPIOperator")

	if !cache.WaitForCacheSync(stopCh,
		optr.deployListerSynced) {
		glog.Error("failed to sync caches")
		return
	}
	glog.Info("Synched up caches")
	go func() {
		err := wait.Poll(machineRolloutPollInterval, machineRolloutTimeout, func() (bool, error) {
			//TODO(vikasc) move operatorconfig rendering logic to main() to fail fast
			operatorConfig, err := optr.getOperatorConfig()
			if err != nil {
				return false, fmt.Errorf("error decoding operator config: %v", err)
			}
			err = optr.updateImageDetails(operatorConfig)
			if err != nil {
				return false, fmt.Errorf("error getting image details: %v", err)
			}
			glog.Infof("images %+v", operatorConfig.Images)

			glog.Info("Trying to deploy Cluster object")
			if err := optr.syncCluster(*operatorConfig); err != nil {
				glog.Infof("Cannot create cluster, retrying: %v", err)
				return false, nil
			}
			glog.Info("Created Cluster object")
			glog.Info("Trying to deploy MachineSet object")
			if err := optr.syncMachineSets(*operatorConfig); err != nil {
				glog.Infof("Cannot create MachineSet, retrying: %v", err)
				return false, nil
			}
			glog.Info("Created MachineSet object Successfully")
			return true, nil
		})
		if err != nil {
			glog.Fatalf("Error out while trying to deploy machines: %v", err)
		}
	}()

	for i := 0; i < workers; i++ {
		go wait.Until(optr.worker, time.Second, stopCh)
	}

	<-stopCh
}

func (optr *Operator) eventHandler() cache.ResourceEventHandler {
	workQueueKey := fmt.Sprintf("%s/%s", optr.namespace, optr.name)
	return cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { optr.queue.Add(workQueueKey) },
		UpdateFunc: func(old, new interface{}) { optr.queue.Add(workQueueKey) },
		DeleteFunc: func(obj interface{}) { optr.queue.Add(workQueueKey) },
	}
}

func (optr *Operator) worker() {
	for optr.processNextWorkItem() {
	}
}

func (optr *Operator) processNextWorkItem() bool {
	key, quit := optr.queue.Get()
	if quit {
		return false
	}
	defer optr.queue.Done(key)

	err := optr.syncHandler(key.(string))
	optr.handleErr(err, key)

	return true
}

func (optr *Operator) handleErr(err error, key interface{}) {
	if err == nil {
		//TODO: set operator Done.

		optr.queue.Forget(key)
		return
	}

	//TODO: set operator degraded.

	if optr.queue.NumRequeues(key) < maxRetries {
		glog.V(2).Infof("Error syncing operator %v: %v", key, err)
		optr.queue.AddRateLimited(key)
		return
	}

	utilruntime.HandleError(err)
	glog.V(2).Infof("Dropping operator %q out of the queue: %v", key, err)
	optr.queue.Forget(key)
}

func (optr *Operator) updateImageDetails(config *render.OperatorConfig) error {
	filesData := map[string][]byte{}
	files := []string{
		optr.imagesFile,
	}
	for _, file := range files {
		data, err := ioutil.ReadFile(file)
		if err != nil {
			return err
		}
		filesData[file] = data
	}
	var imgs render.Images
	if err := json.Unmarshal(filesData[optr.imagesFile], &imgs); err != nil {
		return err
	}
	(*config).Images = &imgs
	return nil
}

func (optr *Operator) sync(key string) error {
	startTime := time.Now()
	glog.V(4).Infof("Started syncing operator %q (%v)", key, startTime)
	defer func() {
		glog.V(4).Infof("Finished syncing operator %q (%v)", key, time.Since(startTime))
	}()

	if err := optr.syncCustomResourceDefinitions(); err != nil {
		return err
	}

	// TODO(alberto) operatorConfig as CRD?
	glog.Infof("Getting operator config using kubeclient")
	operatorConfig, err := optr.getOperatorConfig()
	if err != nil {
		glog.Fatalf("Error decoding operator config: %v", err)
		return err
	}
	err = optr.updateImageDetails(operatorConfig)
	if err != nil {
		glog.Fatalf("Error getting image details: %v", err)
		return err
	}

	err = optr.syncClusterAPIServer(*operatorConfig)
	if err != nil {
		glog.Fatalf("Failed sync-up cluster apiserver: %v", err)
		return err
	}
	glog.Info("Synched up cluster api server")
	err = optr.syncClusterAPIController(*operatorConfig)
	if err != nil {
		glog.Fatalf("Failed sync-up cluster api controller: %v", err)
		return err
	}
	glog.Info("Synched up cluster api controller")
	return optr.syncAll(*operatorConfig)
}

func (optr *Operator) getOperatorConfig() (*render.OperatorConfig, error) {
	clusterConfigNamespace := "kube-system"
	clusterConfigName := "cluster-config-v1"
	clusterConfig, err := optr.kubeClient.CoreV1().ConfigMaps(clusterConfigNamespace).Get(clusterConfigName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("could not find cluster-config-v1 namespace: %v", err)
	}
	return optr.mcFromClusterConfig(clusterConfig)
}

func (optr *Operator) mcFromClusterConfig(cm *v1.ConfigMap) (*render.OperatorConfig, error) {
	mcKey := "mao-config"
	var operatorConfig render.OperatorConfig
	mcData, ok := cm.Data[mcKey]
	if !ok {
		return nil, fmt.Errorf("%s doesn't exist", mcKey)
	}

	if err := yaml.Unmarshal([]byte(mcData), &operatorConfig); err != nil {
		return nil, fmt.Errorf("failed unmarshalling config file: %v", err)
	}
	if operatorConfig.TargetNamespace == "" {
		operatorConfig.TargetNamespace = optr.namespace
	}
	return &operatorConfig, nil
}
