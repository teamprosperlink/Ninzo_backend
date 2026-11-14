package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/teamprosperlink/Ninzo_backend/internal/fcm"
	"github.com/teamprosperlink/Ninzo_backend/internal/orders"
)

type noDevices struct{}

func (noDevices) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

type standIn struct{ topics []string }

func (s *standIn) Push(_ context.Context, _, _, topic string, _ map[string]any) error {
	s.topics = append(s.topics, topic)
	return nil
}

func sender(t *testing.T, project string) *fcm.Sender {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(priv)
	account, _ := json.Marshal(map[string]string{
		"type": "service_account", "project_id": project, "client_email": "push@" + project + ".iam.gserviceaccount.com",
		"private_key": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), "token_uri": "https://oauth2.example.com/token",
	})
	s, err := fcm.New(account)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func projectOf(p orders.Pusher) string {
	if s, ok := p.(*fcm.Sender); ok {
		return s.Project()
	}
	return "log"
}

// Which Firebase project each topic goes through (issue 257).
func TestEachTopicUsesTheRightFirebaseProject(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cases := []struct {
		name               string
		rider, customer    *fcm.Sender
		wantRider, wantCus string
	}{
		{"no keys: both only logged", nil, nil, "log", "log"},
		{"one key: both use it, as before", sender(t, "ninzo-rider"), nil, "ninzo-rider", "ninzo-rider"},
		{"two keys: customers use their own project", sender(t, "ninzo-rider"), sender(t, "ninzo-customer"), "ninzo-rider", "ninzo-customer"},
		{"customer key only: riders only logged", nil, sender(t, "ninzo-customer"), "log", "ninzo-customer"},
	}
	for _, c := range cases {
		stand := &standIn{}
		p := pushersFor(stand, c.rider, c.customer, noDevices{}, log)
		got := func(topic string) string {
			if q, ok := p.ByTopic[topic]; ok && q != nil {
				return projectOf(q)
			}
			return "log"
		}
		if got("push.rider") != c.wantRider || got("push.customer") != c.wantCus {
			t.Errorf("%s: rider %s, customer %s", c.name, got("push.rider"), got("push.customer"))
		}
		for _, s := range []*fcm.Sender{c.rider, c.customer} {
			if s != nil && s.Devices == nil {
				t.Errorf("%s: a sender has no device store, so a dead token could not be dropped", c.name)
			}
		}
	}
}
