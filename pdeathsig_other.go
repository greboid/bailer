//go:build !linux

package main

// setupParentDeathSignal is a no-op on platforms without prctl; exporters
// there rely on the Docker response stream breaking when restic exits.
func setupParentDeathSignal() error {
	return nil
}
