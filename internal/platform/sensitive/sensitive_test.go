package sensitive

import (
	"sort"
	"strings"
	"testing"
)

const nested = `{"id":"a","access_token":"T1","items":[{"otp":"123456","qty":1}],"order":{"handover_otp":"4821","gate":{"password":"P1"}}}`

func TestFindSeesEveryDepth(t *testing.T) {
	got := Find([]byte(nested))
	sort.Strings(got)
	want := "access_token,handover_otp,otp,password"
	if strings.Join(got, ",") != want {
		t.Errorf("got %v, want %s", got, want)
	}
	if got := Find([]byte(`{"id":"a","token_count":3,"items":[{"qty":1}]}`)); len(got) != 0 {
		t.Errorf("a clean value: %v", got)
	}
	if got := Find([]byte(`not json`)); got != nil {
		t.Errorf("invalid JSON: %v", got)
	}
}

func TestStripRemovesThemAndKeepsTheRest(t *testing.T) {
	out, err := Strip([]byte(nested))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"T1", "123456", "4821", "P1"} {
		if strings.Contains(string(out), secret) {
			t.Errorf("%s survived: %s", secret, out)
		}
	}
	for _, kept := range []string{`"id":"a"`, `"qty":1`, `"gate":{}`, `"handover_otp":null`} {
		if !strings.Contains(string(out), kept) {
			t.Errorf("%s was lost: %s", kept, out)
		}
	}
	if _, err := Strip([]byte(`not json`)); err == nil {
		t.Error("invalid JSON must be an error")
	}
}

func TestANullHoldsNoSecret(t *testing.T) {
	if got := Find([]byte(`{"id":"o1","handover_otp":null}`)); len(got) != 0 {
		t.Errorf("a full Order with no code yet was refused: %v", got)
	}
	if got := Find([]byte(`{"handover_otp":"4821"}`)); len(got) != 1 {
		t.Errorf("a real code was missed: %v", got)
	}
	if got := Find([]byte(`{"otp_available":true,"push_token":"t"}`)); len(got) != 1 || got[0] != "push_token" {
		t.Errorf("got %v", got)
	}
}
