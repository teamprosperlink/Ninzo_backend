// Command rider-api serves the rider app. Its contract is contracts/rider/openapi.yaml.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/go-chi/chi/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/service"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := service.Run(ctx, service.Options{
		Name: "rider-api",
		Mount: func(r chi.Router, d service.Deps) {
			identity.MountSessions(r, identity.NewSessions(d.DB, d.Config, d.Log), identity.TypeRider)
		},
	}); err != nil {
		fmt.Fprintln(os.Stderr, "rider-api:", err)
		os.Exit(1)
	}
}
