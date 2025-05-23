package main

import (
	"log"
	"net/http"

	"github.com/garlicKim21/kube-lb/pkg/webhook"
)

func main() {
	http.HandleFunc("/vip", webhook.HandleWebhook)

	log.Println("Starting webhook server on port 8080")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		log.Fatalf("Failed to start webhook server: %v", err)
	}
}
