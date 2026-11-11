package shifts

import "testing"

// From the #159 independent review: the break start comes from the app's clock and the shift end
// from the database's, so a break that seems to start a little after the shift ends must still close.
func TestEndingTheShiftClosesABreakStampedAheadOfTheDatabaseClock(t *testing.T) {
	f := newFixture(t)
	f.scooter("SC-1")
	_, token := f.rider()
	f.start(token)
	f.send("POST", "/riders/me/breaks/start", token, "")
	f.exec("UPDATE shift_breaks SET started_at = now() + interval '1 minute' WHERE ended_at IS NULL")
	code, out := f.send("POST", "/riders/me/shifts/end", token, "")
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	if n := f.count("SELECT count(*) FROM shift_breaks WHERE ended_at IS NULL OR ended_at < started_at"); n != 0 {
		t.Errorf("%d breaks left open or ending before they start", n)
	}
}
