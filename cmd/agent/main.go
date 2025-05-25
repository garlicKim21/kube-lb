package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/garlicKim21/kube-lb/pkg/api"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
)

type Controller struct {
	clientset  *kubernetes.Clientset
	informer   cache.SharedIndexInformer
	httpClient *http.Client
	clusterID  string
	webhookURL string
}

func NewController(clientset *kubernetes.Clientset, clusterID, webhookURL string) *Controller {
	factory := informers.NewSharedInformerFactory(clientset, 30*time.Second)
	informer := factory.Core().V1().Services().Informer()

	return &Controller{
		clientset:  clientset,
		informer:   informer,
		httpClient: &http.Client{Timeout: 10 * time.Second},
		clusterID:  clusterID,
		webhookURL: webhookURL,
	}
}

func (c *Controller) Run(stopCh <-chan struct{}) {
	c.informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.handleAddService,
		UpdateFunc: c.handleUpdateService,
	})

	go c.informer.Run(stopCh)

	if !cache.WaitForCacheSync(stopCh, c.informer.HasSynced) {
		log.Fatal("Failed to sync informer cache")
	}

	<-stopCh
}

func (c *Controller) handleAddService(obj any) {
	svc, ok := obj.(*corev1.Service)
	if !ok {
		log.Printf("Unexpected object type: %T", obj)
		return
	}
	if svc.Spec.Type == corev1.ServiceTypeLoadBalancer {
		c.processService(svc)
	}
}

func (c *Controller) handleUpdateService(oldObj, newObj any) {
	newSvc, ok := newObj.(*corev1.Service)
	if !ok {
		log.Printf("Unexpected object type: %T", newObj)
		return
	}
	if newSvc.Spec.Type == corev1.ServiceTypeLoadBalancer {
		c.processService(newSvc)
	}
}

func (c *Controller) processService(svc *corev1.Service) {
	req := api.WebhookRequest{
		ClusterID:   c.clusterID,
		ServiceName: svc.Name,
		Namespace:   svc.Namespace,
		NodePort:    svc.Spec.Ports[0].NodePort,
		NodeIPs:     getNodeIPs(c.clientset),
	}
	reqBody, err := json.Marshal(req)
	if err != nil {
		log.Printf("Failed to marshal webhook request: %v", err)
		return
	}

	resp, err := c.httpClient.Post(c.webhookURL, "application/json", bytes.NewBuffer(reqBody))
	if err != nil {
		log.Printf("Failed to call webhook: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("Webhook returned non-OK status: %d", resp.StatusCode)
		return
	}

	var webhookResp api.WebhookResponse
	if err := json.NewDecoder(resp.Body).Decode(&webhookResp); err != nil {
		log.Printf("Failed to decode webhook response: %v", err)
		return
	}

	svcCopy, err := c.clientset.CoreV1().Services(svc.Namespace).Get(context.TODO(), svc.Name, metav1.GetOptions{})
	if err != nil {
		log.Printf("Failed to get service %s/%s: %v", svc.Namespace, svc.Name, err)
		return
	}
	svcCopy.Spec.ExternalIPs = []string{webhookResp.VIP}
	_, err = c.clientset.CoreV1().Services(svc.Namespace).Update(context.TODO(), svcCopy, metav1.UpdateOptions{})
	if err != nil {
		log.Printf("Failed to update service %s/%s: %v", svc.Namespace, svc.Name, err)
		return
	}
	log.Printf("Updated service %s/%s with VIP %s", svc.Namespace, svc.Name, webhookResp.VIP)
}

func labelWorkerNodes(clientset *kubernetes.Clientset) error {
	nodes, err := clientset.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list nodes: %v", err)
	}

	for _, node := range nodes.Items {
		// 컨트롤 플레인 노드인지 확인
		isControlPlane := false
		for _, label := range []string{
			"node-role.kubernetes.io/control-plane",
			"node-role.kubernetes.io/master",
		} {
			if _, exists := node.Labels[label]; exists {
				isControlPlane = true
				break
			}
		}

		// 컨트롤 플레인 노드가 아닌 경우에만 워커 노드 라벨 추가
		if !isControlPlane {
			nodeCopy := node.DeepCopy()
			if nodeCopy.Labels == nil {
				nodeCopy.Labels = make(map[string]string)
			}
			nodeCopy.Labels["kube-lb.io/worker-node"] = ""

			_, err := clientset.CoreV1().Nodes().Update(context.TODO(), nodeCopy, metav1.UpdateOptions{})
			if err != nil {
				return fmt.Errorf("failed to label node %s: %v", node.Name, err)
			}
			log.Printf("Labeled node %s as worker node", node.Name)
		}
	}
	return nil
}

func getNodeIPs(clientset *kubernetes.Clientset) []string {
	nodes, err := clientset.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{
		LabelSelector: "kube-lb.io/worker-node",
	})
	if err != nil {
		log.Printf("Failed to list nodes: %v", err)
		return nil
	}
	var nodeIPs []string
	for _, node := range nodes.Items {
		for _, addr := range node.Status.Addresses {
			if addr.Type == corev1.NodeInternalIP {
				nodeIPs = append(nodeIPs, addr.Address)
			}
		}
	}
	return nodeIPs
}

func main() {
	clusterID := flag.String("cluster-id", os.Getenv("CLUSTER_ID"), "Cluster identifier")
	webhookURL := flag.String("webhook-url", os.Getenv("WEBHOOK_URL"), "Webhook server URL")
	flag.Parse()

	if *clusterID == "" {
		log.Fatal("Cluster ID must be specified via --cluster-id or CLUSTER_ID environment variable")
	}

	if *webhookURL == "" {
		log.Fatal("Webhook URL must be specified via --webhook-url or WEBHOOK_URL environment variable")
	}

	config, err := rest.InClusterConfig()
	if err != nil {
		log.Fatalf("Failed to load in-cluster config: %v", err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatalf("Failed to create clientset: %v", err)
	}

	// 노드 라벨링 수행
	if err := labelWorkerNodes(clientset); err != nil {
		log.Fatalf("Failed to label worker nodes: %v", err)
	}

	controller := NewController(clientset, *clusterID, *webhookURL)
	stopCh := make(chan struct{})
	defer close(stopCh)
	controller.Run(stopCh)
}
