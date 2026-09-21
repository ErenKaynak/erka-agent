//go:build !darwin && !linux

package sandbox

import "errors"

func detectBackend() (string, string) {
	return "", "no OS sandbox backend exists for this platform yet"
}

func confine(Isolation, string, string, string, string, []string) (confinedCommand, error) {
	return confinedCommand{}, errors.New("OS sandbox is unavailable on this platform")
}
