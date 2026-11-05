// Command rider-api serves the rider app. Its contract is contracts/rider/openapi.yaml.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/teamprosperlink/Ninzo_backend/internal/platform/service"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := service.Run(ctx, service.Options{Name: "rider-api"}); err != nil {
		fmt.Fprintln(os.Stderr, "rider-api:", err)
		os.Exit(1)
	}
}
