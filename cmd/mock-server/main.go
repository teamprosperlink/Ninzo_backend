// Command mock-server serves sample answers for the customer and rider contracts.
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
	}
	log.Printf("mock server listening on :%s", port)
	log.Fatal(server.ListenAndServe())
}
