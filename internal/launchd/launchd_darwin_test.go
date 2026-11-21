package launchd

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A plist may be stored in binary form. The real plutil converts it; only
// launchctl is faked, so no job is loaded on this machine.
func TestUnderReadsABinaryPlist(t *testing.T) {
	agents, ws := t.TempDir(), t.TempDir()
	p := plist(t, agents, "com.example.binary", "\t<key>WorkingDirectory</key>\n\t<string>"+ws+"</string>")
	if out, err := exec.Command("plutil", "-convert", "binary1", p).CombinedOutput(); err != nil {
		t.Fatalf("plutil: %v: %s", err, out)
	}
	tools := func(name string, args ...string) ([]byte, error) {
		if name == "launchctl" {
			return []byte("PID\tStatus\tLabel\n-\t0\tcom.example.binary\n"), nil
		}
		return exec.Command(name, args...).Output()
	}
	got, err := (&Jobs{Dirs: []string{agents}, Run: tools}).Under(filepath.Join(ws))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !strings.Contains(got[0], "com.example.binary runs from it (WorkingDirectory ") {
		t.Fatalf("Under = %q", got)
	}
}
