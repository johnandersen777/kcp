package socialweb

import (
	"encoding/json"
	"testing"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

// The page composes the requester's record retention off unless it opted in
// (socialweb.retention); an existing app is brought to the same value.
func TestPageRetention(t *testing.T) {
	off := pageRetention("")
	if off.Mode != "" || off.Days == nil || *off.Days != 0 || off.InfraDays == nil || *off.InfraDays != 0 {
		t.Fatalf("pageRetention(\"\") = %+v, want days and infraDays 0", off)
	}
	if got := pageRetention("bogus"); !sameRetention(got, off) {
		t.Fatalf("an unknown mode = %+v, want off", got)
	}
	dry := pageRetention(v1alpha1.RetentionModeDryRun)
	if dry.Mode != v1alpha1.RetentionModeDryRun || dry.Days != nil || dry.InfraDays != nil {
		t.Fatalf("pageRetention(DryRun) = %+v, want the mode only", dry)
	}
	if sameRetention(off, dry) || !sameRetention(dry, pageRetention(v1alpha1.RetentionModeDryRun)) {
		t.Fatal("sameRetention does not tell off from DryRun")
	}
	if sameRetention(nil, off) {
		t.Fatal("an app from before retention (nil) counts as off")
	}
}

// The merge patch replaces retention whole: opting in drops the zero days.
func TestRetentionPatch(t *testing.T) {
	raw, err := json.Marshal(retentionPatch(pageRetention(v1alpha1.RetentionModeDryRun)))
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"days":null,"delaySeconds":null,"infraDays":null,"keep":null,"mode":"DryRun"}`; string(raw) != want {
		t.Fatalf("patch = %s, want %s", raw, want)
	}
	raw, _ = json.Marshal(retentionPatch(pageRetention("")))
	if want := `{"days":0,"delaySeconds":null,"infraDays":0,"keep":null,"mode":null}`; string(raw) != want {
		t.Fatalf("patch = %s, want %s", raw, want)
	}
}
