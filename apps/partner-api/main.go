// Command partner-api serves the partner (hub) app. Its contract is contracts/partner/openapi.yaml.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/go-chi/chi/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/hub"
	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/orders"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/config"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/ratelimit"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/service"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := service.Run(ctx, service.Options{
		Name: "partner-api",
		Validate: func(cfg config.Config) error {
			return errors.Join(identity.RequireSMS(cfg), identity.RequireClientIP(cfg))
		},
		Mount: func(r chi.Router, d service.Deps) {
			limits := identity.NewLimits(ratelimit.NewMemory(), d.Config, d.Log)
			sessions := identity.NewSessions(d.DB, d.Config, d.Log)
			sessions.Limits = limits
			identity.MountSessions(r, sessions, identity.TypeStaff)
			otps := identity.NewOTPs(d.DB, sessions, d.Config, d.Log)
			otps.Limits = limits
			identity.MountOTP(r, otps, identity.TypeStaff)
			hubs := &hub.Service{Pool: d.DB, Log: d.Log}
			hubs.MountStaff(r, identity.RequireActor(sessions.Signer, identity.TypeStaff), limits.BySession())
			hubs.MountQueue(r, identity.RequireActor(sessions.Signer, identity.TypeStaff), limits.BySession())
			hubs.MountSeal(r, identity.RequireActor(sessions.Signer, identity.TypeStaff), limits.BySession())
			hubs.MountEvents(r, identity.RequireActor(sessions.Signer, identity.TypeStaff), limits.BySession())
			streams := &orders.Service{Pool: d.DB, Log: d.Log}
			streams.MountHubStream(r, &orders.Streams{Signer: sessions.Signer}, hubs.Feed())
		},
	}); err != nil {
		fmt.Fprintln(os.Stderr, "partner-api:", err)
		os.Exit(1)
	}
}
