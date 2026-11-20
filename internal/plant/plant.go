// Package plant is a deliberate CodeQL finding, to see the gate fail once.
// It is reverted in the next commit (#202).
package plant

import (
	"net/http"
	"os/exec"
)

// Handler runs a shell command taken from the request: command injection.
func Handler(_ http.ResponseWriter, r *http.Request) {
	_ = exec.Command("sh", "-c", r.URL.Query().Get("cmd")).Run()
}
