package setup

import (
	"context"
	"testing"
)

func TestPinnedClientRefusesAnotherCertificate(t *testing.T) {
	srv := fakePermissions(t)
	// Every httptest TLS server shares one certificate, so stand in a different one by its bytes.
	other := append([]byte{0}, srv.Certificate().Raw...)
	if err := PVECheck(srv.URL, "pve1", PinnedClient(other))(context.Background(), goodToken); err == nil {
		t.Fatal("a server with a different certificate was trusted")
	}
}
