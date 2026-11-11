// Command console-api serves the ops console. Its contract is contracts/console/openapi.yaml.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/go-chi/chi/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/console"
	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/config"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/ratelimit"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/service"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := service.Run(ctx, service.Options{
		Name: "console-api",
		Validate: func(cfg config.Config) error {
			return errors.Join(identity.RequireClientIP(cfg))
		},
		Mount: func(r chi.Router, d service.Deps) {
			limits := identity.NewLimits(ratelimit.NewMemory(), d.Config, d.Log)
			sessions := identity.NewSessions(d.DB, d.Config, d.Log)
			sessions.Limits = limits
			sealer, err := console.NewSealer([]byte(d.Config.JWTSigningKey))
			if err != nil {
				panic(err)
			}
			svc := &console.Service{Pool: d.DB, Log: d.Log, Sessions: sessions, Sealer: sealer, Limits: limits, TOTPOptional: d.Config.ConsoleTOTPOptional}
			signedIn := []func(http.Handler) http.Handler{identity.RequireActor(sessions.Signer, identity.TypeConsole), limits.BySession()}
			svc.MountAuth(r, signedIn...)
			svc.MountUsers(r, signedIn...)
			svc.MountApprovals(r, console.Kinds{"flag": console.ApplyFlag, "settings": console.ApplySettings, "kill_switch": console.ApplyKillSwitch}, signedIn...)
			svc.MountFlags(r, signedIn...)
			svc.MountRiders(r, signedIn...)
			svc.MountZones(r, signedIn...)
			svc.MountMenu(r, signedIn...)
			svc.MountGates(r, signedIn...)
			svc.MountHubs(r, signedIn...)
			svc.MountReviews(r, signedIn...)
		},
	}); err != nil {
		fmt.Fprintln(os.Stderr, "console-api:", err)
		os.Exit(1)
	}
}
