package api

type WebhookRequest struct {
	ClusterID   string   `json:"cluster_id"`
	ServiceName string   `json:"service_name"`
	Namespace   string   `json:"namespace"`
	NodePort    int32    `json:"node_port"`
	NodeIPs     []string `json:"node_ips"`
}

type WebhookResponse struct {
	VIP string `json:"vip"`
}
