// Command rider-api serves the rider app. Its contract is contracts/rider/openapi.yaml.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/go-chi/chi/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/earnings"
	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/orders"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/config"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/ratelimit"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/service"
	"github.com/teamprosperlink/Ninzo_backend/internal/profile"
	"github.com/teamprosperlink/Ninzo_backend/internal/relocation"
	"github.com/teamprosperlink/Ninzo_backend/internal/restock"
	"github.com/teamprosperlink/Ninzo_backend/internal/shifts"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := service.Run(ctx, service.Options{
		Name: "rider-api",
		Validate: func(cfg config.Config) error {
			return errors.Join(identity.RequireSMS(cfg), identity.RequireClientIP(cfg))
		},
		Mount: func(r chi.Router, d service.Deps) {
			limits := identity.NewLimits(ratelimit.NewMemory(), d.Config, d.Log)
			sessions := identity.NewSessions(d.DB, d.Config, d.Log)
			sessions.Limits = limits
			identity.MountSessions(r, sessions, identity.TypeRider)
			otps := identity.NewOTPs(d.DB, sessions, d.Config, d.Log)
			otps.Limits = limits
			identity.MountOTP(r, otps, identity.TypeRider)
			profiles := &profile.Service{Pool: d.DB, Log: d.Log}
			profiles.MountRider(r, identity.RequireActor(sessions.Signer, identity.TypeRider), limits.BySession())
			work := &shifts.Service{Pool: d.DB, Log: d.Log}
			work.MountRider(r, identity.RequireActor(sessions.Signer, identity.TypeRider), limits.BySession())
			delivery := &orders.Service{Pool: d.DB, Log: d.Log}
			delivery.MountRider(r, identity.RequireActor(sessions.Signer, identity.TypeRider), limits.BySession())
			delivery.MountTracking(r, identity.RequireActor(sessions.Signer, identity.TypeRider), limits.ByPings())
			delivery.MountRiderStream(r, &orders.Streams{Signer: sessions.Signer})
			restocks := &restock.Service{Pool: d.DB, Log: d.Log}
			restocks.MountRider(r, identity.RequireActor(sessions.Signer, identity.TypeRider), limits.BySession())
			restocks.MountSuggestions(r, identity.RequireActor(sessions.Signer, identity.TypeRider), limits.BySession())
			restocks.MountRequests(r, identity.RequireActor(sessions.Signer, identity.TypeRider), limits.BySession())
			moves := &relocation.Service{Pool: d.DB, Log: d.Log}
			moves.MountRider(r, identity.RequireActor(sessions.Signer, identity.TypeRider), limits.BySession())
			pay := &earnings.Service{Pool: d.DB, Log: d.Log}
			pay.MountRider(r, identity.RequireActor(sessions.Signer, identity.TypeRider), limits.BySession())
		},
	}); err != nil {
		fmt.Fprintln(os.Stderr, "rider-api:", err)
		os.Exit(1)
	}
}
