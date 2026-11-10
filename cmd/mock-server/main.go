// Command mock-server serves sample answers for the customer, rider, partner and console contracts.
package main

import (
	"log"
	"net/http"
	"os"
	"time"

	"github.com/teamprosperlink/Ninzo_backend/internal/mockserver"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	server := &http.Server{
		Addr:              ":" + port,
		Handler:           mockserver.New().Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	log.Printf("mock server listening on :%s", port)
	log.Fatal(server.ListenAndServe())
}
