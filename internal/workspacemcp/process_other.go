//go:build !linux && !darwin && !freebsd && !netbsd && !openbsd && !dragonfly

package workspacemcp

import "os/exec"

// Windows needs an atomic create-in-job (or suspended-create/assign/resume)
// launcher with kill-on-close and no breakaway. Assigning a job after Start
// races descendants; exec.Cmd does not expose the initial thread to resume.
// Until such a launcher is implemented and tested natively, Call refuses to
// start. Other platforms without the group supervisor also fail closed.
const processTreeSupported = false

func configureProcess(*exec.Cmd) {}

func stopProcess(*exec.Cmd) {}
