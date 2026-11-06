// Command customer-api serves the customer app. Its contract is contracts/customer/openapi.yaml.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/go-chi/chi/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/config"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/service"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := service.Run(ctx, service.Options{
		Name: "customer-api",
		Validate: func(cfg config.Config) error {
			return errors.Join(identity.RequireSMS(cfg), identity.RequireGoogle(cfg))
		},
		Mount: func(r chi.Router, d service.Deps) {
			sessions := identity.NewSessions(d.DB, d.Config, d.Log)
			identity.MountSessions(r, sessions, identity.TypeCustomer)
			otps := identity.NewOTPs(d.DB, sessions, d.Config, d.Log)
			identity.MountOTP(r, otps, identity.TypeCustomer)
			identity.MountGoogle(r, identity.NewGoogleSignIn(d.DB, sessions, d.Config, d.Log))
			identity.MountPhoneVerification(r, otps, sessions.Signer)
		},
	}); err != nil {
		fmt.Fprintln(os.Stderr, "customer-api:", err)
		os.Exit(1)
	}
}
