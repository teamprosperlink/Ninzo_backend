// Command customer-api serves the customer app. Its contract is contracts/customer/openapi.yaml.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/teamprosperlink/Ninzo_backend/internal/availability"
	"github.com/teamprosperlink/Ninzo_backend/internal/catalog"
	"github.com/teamprosperlink/Ninzo_backend/internal/geo"
	"github.com/teamprosperlink/Ninzo_backend/internal/identity"
	"github.com/teamprosperlink/Ninzo_backend/internal/orders"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/config"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/outbox"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/ratelimit"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/service"
	"github.com/teamprosperlink/Ninzo_backend/internal/profile"
	"github.com/teamprosperlink/Ninzo_backend/internal/relocation"
	"github.com/teamprosperlink/Ninzo_backend/internal/restock"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := service.Run(ctx, service.Options{
		Name: "customer-api",
		Validate: func(cfg config.Config) error {
			return errors.Join(identity.RequireSMS(cfg), identity.RequireMethods(cfg), identity.RequireClientIP(cfg), orders.RequirePayments(cfg))
		},
		Mount: func(r chi.Router, d service.Deps) {
			limits := identity.NewLimits(ratelimit.NewMemory(), d.Config, d.Log)
			sessions := identity.NewSessions(d.DB, d.Config, d.Log)
			sessions.Limits = limits
			identity.MountSessions(r, sessions, identity.TypeCustomer)
			otps := identity.NewOTPs(d.DB, sessions, d.Config, d.Log)
			otps.Limits = limits
			google := identity.NewGoogleSignIn(d.DB, sessions, d.Config, d.Log)
			google.Limits = limits
			// Each sign-in method is a component. Add a new one to this list.
			identity.MountMethods(r, d.Config.SignInMethods, identity.PhoneMethod{OTPs: otps, ActorType: identity.TypeCustomer}, google)
			identity.MountPhoneVerification(r, otps, sessions.Signer)
			identity.MountMethodChange(r, d.Config.SignInMethods, otps, google, sessions.Signer)
			profiles := &profile.Service{Pool: d.DB, Log: d.Log}
			profiles.MountCustomer(r, identity.RequireActor(sessions.Signer, identity.TypeCustomer), limits.BySession())
			places := &geo.Service{Pool: d.DB, Log: d.Log}
			places.MountCustomer(r, identity.RequireActor(sessions.Signer, identity.TypeCustomer), limits.BySession())
			menu := &catalog.Service{Pool: d.DB, Log: d.Log}
			menu.MountCustomer(r, identity.RequireActor(sessions.Signer, identity.TypeCustomer), limits.BySession())
			reach := &availability.Service{Pool: d.DB, Log: d.Log}
			reach.MountCustomer(r, identity.RequireActor(sessions.Signer, identity.TypeCustomer), limits.BySession())
			// The test payment stands in for Razorpay (decision 0013). Production refuses to start with it.
			ordering := &orders.Service{Pool: d.DB, Log: d.Log, Payments: orders.TestPayments{}}
			ordering.MountCustomer(r, d.Config.Env != config.EnvProduction, identity.RequireActor(sessions.Signer, identity.TypeCustomer), limits.BySession())
			// Holds end and order clocks move in the background, inside this API for now (decision 0013).
			go ordering.RunJobs(ctx, 5*time.Second)
			ordering.MountCustomerStream(r, &orders.Streams{Signer: sessions.Signer})
			ordering.MountSupport(r, identity.RequireActor(sessions.Signer, identity.TypeCustomer), limits.BySession())
			// Pushes go through the outbox. The log pusher stands in until FCM is connected.
			pushes := &outbox.Worker{Pool: d.DB, Handle: ordering.Deliver(orders.LogPusher{Log: d.Log}), Log: d.Log, Topics: orders.PushTopics}
			go pushes.Run(ctx)
			// Restock's own jobs: the stock check and the expiry of uncollected cassettes. Hub staff do
			// the hub's part through the partner API; the simulator is for tests only.
			restocks := &restock.Service{Pool: d.DB, Log: d.Log}
			go restocks.RunJobs(ctx, 5*time.Second)
			// Suggested moves toward demand (v0.7.0 part D), in this API with the other passes.
			moves := &relocation.Service{Pool: d.DB, Log: d.Log}
			go moves.Run(ctx, 30*time.Second)
		},
	}); err != nil {
		fmt.Fprintln(os.Stderr, "customer-api:", err)
		os.Exit(1)
	}
}
