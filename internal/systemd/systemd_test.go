package systemd

import (
	"maps"
	"testing"
)

func TestParseShow(t *testing.T) {
	out := "Id=ci-runner@0.service\nActiveState=active\n\nActiveState=failed\nId=ci-runner@1.service\n\nId=ci-runner@2.service\nActiveState=inactive\n"
	got, err := parseShow([]byte(out))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"ci-runner@0.service": "active", "ci-runner@1.service": "failed", "ci-runner@2.service": "inactive"}
	if !maps.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParseList(t *testing.T) {
	out := "ci-runner@0.service loaded active running CI runner 0\n" +
		"ci-runner@3.service loaded failed failed CI runner 3\n" +
		"ci-runner@4.service not-found inactive dead ci-runner@4.service\n"
	got := parseList([]byte(out))
	want := map[string]string{"ci-runner@0.service": "active", "ci-runner@3.service": "failed", "ci-runner@4.service": "inactive"}
	if !maps.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestExited(t *testing.T) {
	for state, want := range map[string]bool{"inactive": true, "failed": true, "active": false, "activating": false, "deactivating": false, "reloading": false, "": false} {
		if Exited(state) != want {
			t.Errorf("Exited(%q) = %v", state, !want)
		}
	}
}
