package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io/ioutil"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	mapset "github.com/deckarep/golang-set/v2"
	"github.com/prometheus/common/expfmt"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/klog/v2"
)

const (
	defaultPort = "9101"
)

func main() {
	namespace := flag.String("namespace", "default", "Detect and kill GPU zombie pod in the namespace")
	idleTimeoutSeconds := flag.Float64("idle_timeout_seconds", 90, "Kill the pod after idle timeout of # seconds")
	checkPeriodSeconds := flag.Float64("check_period_seconds", 10, "Check for zombie every # seconds")

	klog.InitFlags(nil)
	if !flag.Parsed() {
		flag.Parse()
	}
	klog.InfoS("Starting Zombie Killer")
	klog.InfoS("Listing flags",
		"namespace", *namespace,
		"idleTimeoutSeconds", *idleTimeoutSeconds,
		"checkPeriodSeconds", *checkPeriodSeconds)

	k, err := NewGpuZombieKiller(*namespace, *idleTimeoutSeconds, *checkPeriodSeconds)
	if err != nil {
		panic(err)
	}
	k.Run(k.StopCh)
}

type uuidState struct {
	pods     mapset.Set[string]
	idleTime time.Duration
}

type GpuZombieKiller struct {
	namespace          string
	idleTimeoutSeconds float64
	checkPeriodSeconds float64

	kConfig      *rest.Config
	kClient      kubernetes.Interface
	podInformer  cache.SharedIndexInformer
	nodeInformer cache.SharedIndexInformer
	StopCh       chan struct{}
	lastScratch  time.Time

	// hostSet stores nodes' IP in the cluster. These nodes are supposed to have
	// nvidia_smi_exporter (https://github.com/heyfey/nvidia_smi_exporter)
	// deployed so we can scratch GPU metrics from. We also assume one node's IP
	// would not be changed without deleting and re-adding the node.
	hostSet mapset.Set[string]
	// uuidStateMap and podUuidMap stores relation between GPUs(by uuid) and pods.
	// Note that we assume one pod's GPUs assigned would not be changed without
	// deleting and re-adding the node.
	uuidStateMap map[string]*uuidState
	podUuidMap   map[string][]string
	// Lock to protect above data structures
	lock sync.RWMutex
}

func NewGpuZombieKiller(namespace string, idleTimeoutSeconds float64, checkPeriodSeconds float64) (*GpuZombieKiller, error) {
	kConfig, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}

	kClient, err := kubernetes.NewForConfig(kConfig)
	if err != nil {
		return nil, err
	}

	sharedInformers := informers.NewSharedInformerFactoryWithOptions(
		kClient,
		0,
		informers.WithNamespace(namespace))
	podInformer := sharedInformers.Core().V1().Pods().Informer()

	sharedNodeInformers := informers.NewSharedInformerFactoryWithOptions(
		kClient,
		0)
	nodeInformer := sharedNodeInformers.Core().V1().Nodes().Informer()

	hostSet, err := getHostsIP(kClient)
	if err != nil {
		return nil, err
	}

	k := &GpuZombieKiller{
		namespace:          namespace,
		idleTimeoutSeconds: idleTimeoutSeconds,
		checkPeriodSeconds: checkPeriodSeconds,
		kConfig:            kConfig,
		kClient:            kClient,
		podInformer:        podInformer,
		nodeInformer:       nodeInformer,
		StopCh:             make(chan struct{}),
		lastScratch:        time.Now(),
		hostSet:            hostSet,
		uuidStateMap:       make(map[string]*uuidState),
		podUuidMap:         make(map[string][]string),
		lock:               sync.RWMutex{},
	}

	// setup informer callbacks
	k.podInformer.AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc:    k.addPod,
			UpdateFunc: k.updatePod,
			DeleteFunc: k.deletePod,
		},
	)

	k.nodeInformer.AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc:    k.addNode,
			UpdateFunc: k.updateNode,
			DeleteFunc: k.deleteNode,
		},
	)

	return k, nil
}

func getHostsIP(kClient *kubernetes.Clientset) (mapset.Set[string], error) {
	hostSet := mapset.NewSet[string]()

	nodes, err := kClient.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	for _, node := range nodes.Items {
		addresses := node.Status.Addresses
		for _, address := range addresses {
			if address.Type == corev1.NodeInternalIP { // Only take care of InternalIP for now
				hostSet.Add(address.Address)
			}
		}
	}
	return hostSet, nil
}

func (k *GpuZombieKiller) Run(stopCh <-chan struct{}) {
	klog.InfoS("Starting GPU zombie killer")
	defer klog.InfoS("Stopping GPU zombie killer")

	go k.podInformer.Run(stopCh)
	go k.nodeInformer.Run(stopCh)
	if !cache.WaitForCacheSync(
		stopCh,
		k.podInformer.HasSynced,
		k.nodeInformer.HasSynced) {
		err := errors.New("failed to WaitForCacheSync")
		klog.ErrorS(err, "failed to WaitForCacheSync")
		klog.Flush()
		os.Exit(1)
	}

	go k.detectAndKill()
	<-stopCh
}

func (k *GpuZombieKiller) addPod(obj interface{}) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		klog.ErrorS(errors.New("unexpected pod type"), "Failed to add pod",
			"pod", klog.KObj(pod))
		return
	}
	klog.V(5).InfoS("Pod added", "pod", klog.KObj(pod))

	// Add the pod if the pod is in "Running" phase
	if pod.Status.Phase == corev1.PodRunning {
		k.addPodHelper(pod)
	}
}

// addPodHelper adds the pod. The pod is supposed to be in "Running" phase.
func (k *GpuZombieKiller) addPodHelper(pod *corev1.Pod) {
	gpuSet := k.getGpusInPod(pod)
	klog.V(5).InfoS("addPodHelper", "pod", klog.KObj(pod), "gpus", gpuSet)
	// do nothing if no GPU assigned to the pod
	if gpuSet.Cardinality() == 0 {
		return
	}

	k.lock.Lock()
	defer k.lock.Unlock()

	podName := pod.GetName()
	for _, uuid := range gpuSet.ToSlice() {
		state, ok := k.uuidStateMap[uuid]
		if ok {
			state.pods.Add(podName)
		} else {
			k.uuidStateMap[uuid] = &uuidState{
				pods:     mapset.NewSet[string](),
				idleTime: 0,
			}
			k.uuidStateMap[uuid].pods.Add(podName)
		}
	}
	k.podUuidMap[podName] = gpuSet.ToSlice()
}

func (k *GpuZombieKiller) deletePod(obj interface{}) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		klog.ErrorS(errors.New("unexpected pod type"), "Failed to delete pod",
			"pod", klog.KObj(pod))
		return
	}
	klog.V(5).InfoS("Pod deleted", "pod", klog.KObj(pod))

	k.lock.Lock()
	defer k.lock.Unlock()

	podName := pod.GetName()
	for _, uuid := range k.podUuidMap[podName] {
		k.uuidStateMap[uuid].pods.Remove(podName)
	}
	delete(k.podUuidMap, podName)
}

func (k *GpuZombieKiller) updatePod(oldObj interface{}, newObj interface{}) {
	oldPod, ok := oldObj.(*corev1.Pod)
	if !ok {
		klog.ErrorS(errors.New("unexpected pod type"), "Failed to update pod",
			"pod", klog.KObj(oldPod))
		return
	}
	newPod, ok := newObj.(*corev1.Pod)
	if !ok {
		klog.ErrorS(errors.New("unexpected pod type"), "Failed to update pod",
			"pod", klog.KObj(newPod))
		return
	}
	klog.V(5).InfoS("Pod updated", "pod", klog.KObj(newPod))

	// Informer may deliver an Update event with UID changed if a delete is
	// immediately followed by a create, so manually decompose it.
	if oldPod.UID != newPod.UID {
		k.deletePod(newObj)
		k.addPod(newObj)
		return
	}

	// Add the pod if the pod is turnning into "Running" phase
	if oldPod.Status.Phase != corev1.PodRunning &&
		newPod.Status.Phase == corev1.PodRunning {
		k.addPodHelper(newPod)
	}
}

// getGpusInPod returns all GPU uuid for the pod. This function only works
// correctly for "Running" pod.
func (k *GpuZombieKiller) getGpusInPod(pod *corev1.Pod) mapset.Set[string] {
	cmd := []string{
		"sh",
		"-c",
		"printenv | grep NVIDIA_VISIBLE_DEVICES=GPU | sed 's/^.*=//'",
	}

	gpuSet := mapset.NewSet[string]()

	// Run the cmd inside each container of the pod, get the GPU uuid
	// 	E.g.
	// 	printenv | grep NVIDIA_VISIBLE_DEVICES=GPU | sed 's/^.*=//'
	// 	GPU-04daf357-ba4b-735b-235e-078f907541bb,GPU-bd387ea4-03fd-20e4-0173-e0c6f29ff1ad
	// Add the GPU uuid to gpuSet
	for _, container := range pod.Spec.Containers {
		const tty = false
		req := k.kClient.CoreV1().RESTClient().Post().
			Resource("pods").
			Name(pod.GetName()).
			Namespace(k.namespace).SubResource("exec").Param("container", container.Name)
		req.VersionedParams(
			&corev1.PodExecOptions{
				Command: cmd,
				Stdin:   false,
				Stdout:  true,
				Stderr:  true,
				TTY:     tty,
			},
			scheme.ParameterCodec,
		)
		var stdout, stderr bytes.Buffer
		exec, err := remotecommand.NewSPDYExecutor(k.kConfig, "POST", req.URL())
		if err != nil {
			continue
		}
		err = exec.Stream(remotecommand.StreamOptions{
			Stdin:  nil,
			Stdout: &stdout,
			Stderr: &stderr,
		})
		if err != nil {
			continue
		}

		s := strings.TrimSuffix(stdout.String(), "\n")
		gpuList := strings.Split(s, ",")
		for _, gpu := range gpuList {
			if gpu != "" {
				gpuSet.Add(gpu)
			}
		}
	}
	return gpuSet
}

func (k *GpuZombieKiller) detectAndKill() {
	for {
		uuidUtilMap := k.scratchGpu()

		k.lock.Lock()
		for uuid, util := range uuidUtilMap {
			uuidState, ok := k.uuidStateMap[uuid]
			if ok {
				if util == 0 {
					// Only consider idle if the GPU is assigned to any pod
					if uuidState.pods.Cardinality() != 0 {
						uuidState.idleTime += time.Since(k.lastScratch)
					} else {
						uuidState.idleTime = 0
						continue
					}

					// Delete pods and reset idle time
					if uuidState.idleTime > time.Duration(k.idleTimeoutSeconds)*time.Second {
						k.deletePods(uuidState.pods)
						uuidState.idleTime = 0
					}
				} else {
					uuidState.idleTime = 0
				}
			}
		}
		k.lastScratch = time.Now()
		k.lock.Unlock()

		time.Sleep(time.Duration(k.checkPeriodSeconds) * time.Second)
	}
}

// scratchGpu scratchs all GPU states and returns [uuid]gpu_utilization map
func (k *GpuZombieKiller) scratchGpu() map[string]float64 {
	k.lock.RLock()
	defer k.lock.RUnlock()

	uuidUtilMap := make(map[string]float64)

	for _, host := range k.hostSet.ToSlice() {
		url := "http://" + host + ":" + defaultPort + "/metrics"
		klog.V(5).InfoS("Scratching GPU metrics", "url", url)

		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			klog.V(5).InfoS("Failed to create request, skipping...",
				"url", url, "error", err)
			continue
		}

		req.Header.Add("Accept", "*/*")
		req.Header.Add("User-Agent", "Thunder Client (https://www.thunderclient.com)")

		res, err := http.DefaultClient.Do(req)
		if err != nil {
			klog.V(5).InfoS("Failed to send request, skipping...",
				"request", req, "error", err)
			continue
		}

		defer res.Body.Close()
		body, err := ioutil.ReadAll(res.Body)
		if err != nil {
			klog.V(5).InfoS("Failed to read respond, skipping...",
				"respond", res, "error", err)
			continue
		}

		parser := &expfmt.TextParser{}
		families, err := parser.TextToMetricFamilies(strings.NewReader(string(body)))
		if err != nil {
			klog.V(5).InfoS("Failed to parse input, skipping...", "error", err)
			continue
		}

		family := families["nvidia_utilization_gpu_uuid"]
		for _, m := range family.GetMetric() {
			uuid := ""
			for _, label := range m.GetLabel() {
				if label.GetName() == "uuid" {
					uuid = label.GetValue()
				}
			}
			if uuid == "" {
				// Should be able to find label:uuid in the metric
				klog.V(5).InfoS("Failed to find label:uuid in metric",
					"metric", m, "url", url)
				continue
			}

			uuidUtilMap[uuid] = m.GetUntyped().GetValue()
		}
	}

	klog.V(5).InfoS("Scratched GPU metrics", "uuidUtilMap", uuidUtilMap)
	return uuidUtilMap
}

func (k *GpuZombieKiller) deletePods(pods mapset.Set[string]) {
	for _, pod := range pods.ToSlice() {
		err := k.kClient.CoreV1().Pods(k.namespace).Delete(context.TODO(), pod, metav1.DeleteOptions{})
		if err != nil {
			klog.ErrorS(err, "Failed to delete GPU zombie pod",
				"pod", klog.KRef(k.namespace, pod))
		} else {
			klog.InfoS("Deleted GPU zombie pod",
				"pod", klog.KRef(k.namespace, pod))
		}
	}
}

func (k *GpuZombieKiller) addNode(obj interface{}) {
	node, ok := obj.(*corev1.Node)
	if !ok {
		klog.ErrorS(errors.New("unexpected node type"), "Failed to add node",
			"node", klog.KObj(node))
		return
	}

	k.lock.Lock()
	defer k.lock.Unlock()

	addresses := node.Status.Addresses
	for _, address := range addresses {
		if address.Type == corev1.NodeInternalIP { // Only take care of InternalIP for now
			k.hostSet.Add(address.Address)
		}
	}
	klog.InfoS("Node added", "node", klog.KObj(node), "hosts", k.hostSet)
}

func (k *GpuZombieKiller) deleteNode(obj interface{}) {
	node, ok := obj.(*corev1.Node)
	if !ok {
		klog.ErrorS(errors.New("unexpected node type"), "Failed to delete node",
			"node", klog.KObj(node))
		return
	}

	k.lock.Lock()
	defer k.lock.Unlock()

	addresses := node.Status.Addresses
	for _, address := range addresses {
		if address.Type == corev1.NodeInternalIP { // Only take care of InternalIP for now
			k.hostSet.Remove(address.Address)
		}
	}
	klog.InfoS("Node added", "node", klog.KObj(node), "hosts", k.hostSet)
}

// Note that for now we assume InternalIP of the same node would not be chaneged.
func (k *GpuZombieKiller) updateNode(oldObj interface{}, newObj interface{}) {
	oldNode, ok := oldObj.(*corev1.Node)
	if !ok {
		klog.ErrorS(errors.New("unexpected node type"), "Failed to update node",
			"node", klog.KObj(oldNode))
		return
	}
	newNode, ok := newObj.(*corev1.Node)
	if !ok {
		klog.ErrorS(errors.New("unexpected node type"), "Failed to update node",
			"node", klog.KObj(newNode))
		return
	}
	// Informer may deliver an Update event with UID changed if a delete is
	// immediately followed by a create.
	if oldNode.UID != newNode.UID {
		k.deleteNode(oldNode)
		k.addNode(newNode)
	}

	// Otherwise do nothing. Note that for now we assume one node's InternalIP
	// would not be chaneged without deleting and re-adding the node.
}
