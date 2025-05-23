package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/garlicKim21/kube-lb/pkg/api"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type VIPPool struct {
	k8sClient *kubernetes.Clientset
}

func NewVIPPool() (*VIPPool, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		log.Printf("Failed to load in-cluster config: %v", err)
		return nil, fmt.Errorf("failed to load in-cluster config: %v", err)
	}

	k8sClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Printf("Failed to create Kubernetes client: %v", err)
		return nil, fmt.Errorf("failed to create Kubernetes client: %v", err)
	}

	return &VIPPool{
		k8sClient: k8sClient,
	}, nil
}

var vipPool *VIPPool

func init() {
	var err error
	vipPool, err = NewVIPPool()
	if err != nil {
		log.Fatalf("Failed to initialize VIP pool: %v", err)
	}
}

func HandleWebhook(w http.ResponseWriter, r *http.Request) {
	if vipPool == nil {
		http.Error(w, "VIP pool not initialized", http.StatusServiceUnavailable)
		log.Println("VIP pool not initialized, rejecting request")
		return
	}

	var req api.WebhookRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		log.Printf("Failed to decode request: %v", err)
		return
	}

	if req.ClusterID == "" || req.Namespace == "" || req.ServiceName == "" || req.NodePort == 0 || len(req.NodeIPs) == 0 {
		http.Error(w, "Missing required fields", http.StatusBadRequest)
		log.Printf("Invalid request: %+v", req)
		return
	}

	log.Printf("Received request: ClusterID=%s, Service=%s/%s, NodePort=%d, NodeIPs=%v",
		req.ClusterID, req.Namespace, req.ServiceName, req.NodePort, req.NodeIPs)

	serviceName := fmt.Sprintf("%s-%s-%s", req.ClusterID, req.Namespace, req.ServiceName)
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName,
			Namespace: "kube-lb-services",
			Labels: map[string]string{
				"color": "blue",
			},
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{
				{
					Name:       "http",
					Port:       80,
					Protocol:   corev1.ProtocolTCP,
					TargetPort: intstr.FromInt(int(req.NodePort)),
				},
			},
			Type:     corev1.ServiceTypeLoadBalancer,
			Selector: nil,
		},
	}

	// Service 생성
	_, err := vipPool.k8sClient.CoreV1().Services("kube-lb-services").Create(context.TODO(), svc, metav1.CreateOptions{})
	if err != nil {
		http.Error(w, "Failed to create service", http.StatusInternalServerError)
		log.Printf("Failed to create service: %v", err)
		return
	}
	log.Printf("Created service %s/%s", "kube-lb-services", serviceName)

	// Cilium이 VIP를 할당할 때까지 대기
	var vip string
	for i := 0; i < 10; i++ {
		time.Sleep(1 * time.Second)
		svc, err = vipPool.k8sClient.CoreV1().Services("kube-lb-services").Get(context.TODO(), serviceName, metav1.GetOptions{})
		if err == nil && len(svc.Status.LoadBalancer.Ingress) > 0 {
			vip = svc.Status.LoadBalancer.Ingress[0].IP
			break
		}
	}
	if vip == "" {
		http.Error(w, "Failed to get VIP from Cilium", http.StatusInternalServerError)
		log.Printf("Failed to get VIP from Cilium for service %s", serviceName)
		return
	}
	log.Printf("Allocated VIP %s for service %s", vip, serviceName)

	// EndpointSlice 생성
	endpointSlice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName + "-endpoint-slice",
			Namespace: "kube-lb-services",
			Labels: map[string]string{
				"endpointslice.kubernetes.io/managed-by": "kube-lb",
			},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Ports: []discoveryv1.EndpointPort{
			{
				Name:     ptrTo("http"),
				Port:     ptrTo(int32(req.NodePort)),
				Protocol: ptrTo(corev1.ProtocolTCP),
			},
		},
		Endpoints: make([]discoveryv1.Endpoint, len(req.NodeIPs)),
	}

	// Endpoint 생성
	endpoints := &corev1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName,
			Namespace: "kube-lb-services",
		},
		Subsets: []corev1.EndpointSubset{
			{
				Addresses: make([]corev1.EndpointAddress, len(req.NodeIPs)),
				Ports: []corev1.EndpointPort{
					{
						Name:     "http",
						Port:     int32(req.NodePort),
						Protocol: corev1.ProtocolTCP,
					},
				},
			},
		},
	}
	for i, ip := range req.NodeIPs {
		endpointSlice.Endpoints[i] = discoveryv1.Endpoint{
			Addresses: []string{ip},
		}
		endpoints.Subsets[0].Addresses[i] = corev1.EndpointAddress{
			IP: ip,
		}
	}

	// Create/Update Endpoints
	_, err = vipPool.k8sClient.CoreV1().Endpoints("kube-lb-services").Get(context.TODO(), serviceName, metav1.GetOptions{})
	if err != nil {
		_, err = vipPool.k8sClient.CoreV1().Endpoints("kube-lb-services").Create(context.TODO(), endpoints, metav1.CreateOptions{})
		if err != nil {
			http.Error(w, "Failed to create endpoints", http.StatusInternalServerError)
			log.Printf("Failed to create endpoints: %v", err)
			return
		}
		log.Printf("Created endpoints %s/%s", "kube-lb-services", serviceName)
	} else {
		_, err = vipPool.k8sClient.CoreV1().Endpoints("kube-lb-services").Update(context.TODO(), endpoints, metav1.UpdateOptions{})
		if err != nil {
			http.Error(w, "Failed to update endpoints", http.StatusInternalServerError)
			log.Printf("Failed to update endpoints: %v", err)
			return
		}
		log.Printf("Updated endpoints %s/%s", "kube-lb-services", serviceName)
	}

	// Create/Update EndpointSlice
	_, err = vipPool.k8sClient.DiscoveryV1().EndpointSlices("kube-lb-services").Get(context.TODO(), serviceName+"-endpoint-slice", metav1.GetOptions{})
	if err != nil {
		_, err = vipPool.k8sClient.DiscoveryV1().EndpointSlices("kube-lb-services").Create(context.TODO(), endpointSlice, metav1.CreateOptions{})
		if err != nil {
			http.Error(w, "Failed to create endpoint slice", http.StatusInternalServerError)
			log.Printf("Failed to create endpoint slice: %v", err)
			return
		}
		log.Printf("Created endpoint slice %s/%s", "kube-lb-services", serviceName+"-endpoint-slice")
	} else {
		_, err = vipPool.k8sClient.DiscoveryV1().EndpointSlices("kube-lb-services").Update(context.TODO(), endpointSlice, metav1.UpdateOptions{})
		if err != nil {
			http.Error(w, "Failed to update endpoint slice", http.StatusInternalServerError)
			log.Printf("Failed to update endpoint slice: %v", err)
			return
		}
		log.Printf("Updated endpoint slice %s/%s", "kube-lb-services", serviceName+"-endpoint-slice")
	}

	// 응답 전달
	resp := api.WebhookResponse{VIP: vip}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		http.Error(w, "Failed to encode response", http.StatusInternalServerError)
		log.Printf("Failed to encode response: %v", err)
		return
	}

	log.Printf("Successfully allocated VIP: %s", vip)
}

func ptrTo[T any](v T) *T {
	return &v
}
