package identity

import (
	"context"
	"errors"
	"testing"
)

// staff makes a hub and a staff member at it, and returns the staff id and the hub id.
func (f *otpFixture) staff(phone, role string) (string, string) {
	f.t.Helper()
	ctx := context.Background()
	var zone, hub, person, id string
	if err := f.s.Pool.QueryRow(ctx, `INSERT INTO zones (name, city, status) VALUES ('Staff zone ' || $1, 'Bengaluru', 'active') RETURNING id::text`, phone).Scan(&zone); err != nil {
		f.t.Fatal(err)
	}
	if err := f.s.Pool.QueryRow(ctx, "INSERT INTO hubs (zone_id, name, lat, lng) VALUES ($1, 'Hub', 12.93, 77.62) RETURNING id::text", zone).Scan(&hub); err != nil {
		f.t.Fatal(err)
	}
	if err := f.s.Pool.QueryRow(ctx, "INSERT INTO people (phone) VALUES ($1) RETURNING id::text", phone).Scan(&person); err != nil {
		f.t.Fatal(err)
	}
	if err := f.s.Pool.QueryRow(ctx, "INSERT INTO hub_staff (person_id, hub_id, role) VALUES ($1, $2, $3) RETURNING id::text", person, hub, role).Scan(&id); err != nil {
		f.t.Fatal(err)
	}
	return id, hub
}

func TestStaffSignIn(t *testing.T) {
	f := newOTPFixture(t)
	id, hub := f.staff(phoneA, "hub_lead")
	v := f.signIn(TypeStaff, phoneA)
	if v.Actor.ID != id || v.Actor.Type != TypeStaff || v.Status != "hub_lead" || v.HubID != hub {
		t.Errorf("%+v", v)
	}
	// A staff token is not a rider's.
	if _, err := f.s.Signer.Verify(v.Pair.AccessToken, TypeRider); err == nil {
		t.Error("a staff token was accepted as a rider's")
	}
	f.refused(v.Pair.RefreshToken, TypeRider)
	f.allowResend()
	again := f.signIn(TypeStaff, phoneA)
	f.mustRefresh(again.Pair.RefreshToken, TypeStaff)
}

func TestAnUnknownStaffNumberGetsNoCode(t *testing.T) {
	f := newOTPFixture(t)
	ctx := context.Background()
	f.rider(phoneA) // A rider is not hub staff.
	req, err := f.o.Request(ctx, TypeStaff, phoneA)
	if err != nil {
		t.Fatalf("the reply must look normal: %v", err)
	}
	if f.sender.count() != 0 {
		t.Error("a code was sent to a number with no staff account")
	}
	if _, err := f.o.Verify(ctx, TypeStaff, req.ID, testCode); !errors.Is(err, ErrInvalidOTP) {
		t.Errorf("%v", err)
	}
}

func TestSuspendedStaffLearnOnlyAfterAProvenCode(t *testing.T) {
	f := newOTPFixture(t)
	ctx := context.Background()
	id, _ := f.staff(phoneA, "hub_staff")
	v := f.signIn(TypeStaff, phoneA)
	f.exec("UPDATE hub_staff SET status = 'suspended' WHERE id = $1", id)
	f.refused(v.Pair.RefreshToken, TypeStaff)
	f.allowResend()
	req, err := f.o.Request(ctx, TypeStaff, phoneA)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.o.Verify(ctx, TypeStaff, req.ID, "000000"); !errors.Is(err, ErrInvalidOTP) {
		t.Errorf("a wrong code reveals nothing: %v", err)
	}
	if _, err := f.o.Verify(ctx, TypeStaff, req.ID, testCode); !errors.Is(err, ErrAccountBlocked) {
		t.Errorf("suspended: %v", err)
	}
	f.exec("UPDATE hub_staff SET status = 'active' WHERE id = $1", id)
	f.exec("UPDATE people SET status = 'blocked' WHERE phone = $1", phoneA)
	f.allowResend()
	again, err := f.o.Request(ctx, TypeStaff, phoneA)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.o.Verify(ctx, TypeStaff, again.ID, testCode); !errors.Is(err, ErrAccountBlocked) {
		t.Errorf("blocked person: %v", err)
	}
}

func TestARiderRefreshIsRefusedAsStaff(t *testing.T) {
	f := newOTPFixture(t)
	f.rider(phoneB)
	v := f.signIn(TypeRider, phoneB)
	f.refused(v.Pair.RefreshToken, TypeStaff)
	f.mustRefresh(v.Pair.RefreshToken, TypeRider) // Refused as staff, it still works as a rider.
}
