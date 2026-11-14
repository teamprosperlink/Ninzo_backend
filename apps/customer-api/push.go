package main

import (
	"log/slog"

	"github.com/teamprosperlink/Ninzo_backend/internal/fcm"
	"github.com/teamprosperlink/Ninzo_backend/internal/orders"
	"github.com/teamprosperlink/Ninzo_backend/internal/platform/service"
)

// choosePushers picks how each push topic is sent (issues 252 and 257). Rider pushes go through the
// FCM_SERVICE_ACCOUNT_JSON project. Customer pushes go through FCM_CUSTOMER_SERVICE_ACCOUNT_JSON's
// project when it is set, else through the same project as riders, as before. A topic with no
// Firebase project is only logged.
func choosePushers(stand orders.Pusher, rider, customer *fcm.Sender, d service.Deps) orders.Pusher {
	return pushersFor(stand, rider, customer, d.DB, d.Log)
}

func pushersFor(stand orders.Pusher, rider, customer *fcm.Sender, devices fcm.Devices, log *slog.Logger) orders.TopicPusher {
	byTopic := map[string]orders.Pusher{}
	if rider != nil {
		rider.Devices = devices
		byTopic["push.rider"] = rider
		byTopic["push.customer"] = rider
		log.Info("push: FCM", "topic", "push.rider", "project", rider.Project())
	}
	if customer != nil {
		customer.Devices = devices
		byTopic["push.customer"] = customer
	}
	if s, ok := byTopic["push.customer"].(*fcm.Sender); ok {
		log.Info("push: FCM", "topic", "push.customer", "project", s.Project())
	}
	return orders.TopicPusher{ByTopic: byTopic, Fallback: stand}
}
