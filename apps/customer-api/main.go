// Command customer-api serves the customer app. Its contract is contracts/customer/openapi.yaml.
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
		Name: "customer-api",
		Mount: func(r chi.Router, d service.Deps) {
			identity.MountSessions(r, identity.NewSessions(d.DB, d.Config, d.Log), identity.TypeCustomer)
		},
	}); err != nil {
		fmt.Fprintln(os.Stderr, "customer-api:", err)
		os.Exit(1)
	}
}
