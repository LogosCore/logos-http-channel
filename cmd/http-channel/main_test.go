package main

import (
	"testing"

	protocol "github.com/logoscore/logos-golang-protocol/protocol"
)

// The icon is sent at registration; module-core refuses to start with one
// core would not take.
func TestIconIsValid(t *testing.T) {
	if err := protocol.ValidateIcon(icon); err != nil {
		t.Fatal(err)
	}
}
