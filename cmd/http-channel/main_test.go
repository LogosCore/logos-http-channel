package main

import (
	"testing"

	protocol "github.com/logoscore/logos-golang-protocol/protocol"
)

// The icons are sent at registration; module-core refuses to start with one
// core would not take.
func TestIconIsValid(t *testing.T) {
	for name, svg := range map[string]string{"icon.svg": icon, "icon-light.svg": iconLight} {
		if err := protocol.ValidateIcon(svg); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}
