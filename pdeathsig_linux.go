//go:build linux

package main

import (
	"fmt"
	"os"
	"syscall"
)

const prSetPdeathsig = 1

// setupParentDeathSignal asks the kernel to kill this process when its parent
// dies. The export-volume subprocess is a grandchild (bailer → restic →
// bailer export-volume); bailer cannot manage it directly, so this guarantees
// the exporter terminates when the restic process is cancelled or killed.
func setupParentDeathSignal() error {
	if _, _, errno := syscall.Syscall(syscall.SYS_PRCTL, prSetPdeathsig, uintptr(syscall.SIGKILL), 0); errno != 0 {
		return fmt.Errorf("prctl(PR_SET_PDEATHSIG): %w", errno)
	}
	// The parent may have died between the process start and this call.
	if os.Getppid() == 1 {
		return fmt.Errorf("parent process already exited")
	}
	return nil
}
