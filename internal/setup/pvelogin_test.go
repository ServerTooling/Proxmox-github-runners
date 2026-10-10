package setup

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakePVE answers the ticket call like pveproxy: 401 for bad credentials, and a ticket with the
// account's capabilities otherwise.
func fakePVE(t *testing.T) *httptest.Server {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/api2/json/access/ticket" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		switch user, pw := r.FormValue("username"), r.FormValue("password"); {
		case user == "root@pam" && pw == "pw":
			fmt.Fprint(w, `{"data":{"ticket":"PVE:t","username":"root@pam","cap":{"nodes":{"Sys.Modify":1,"Sys.Audit":1}}}}`)
		case user == "viewer@pve" && pw == "pw":
			fmt.Fprint(w, `{"data":{"ticket":"PVE:t","username":"viewer@pve","cap":{"nodes":{"Sys.Audit":1}}}}`)
		default:
			w.WriteHeader(401)
			fmt.Fprint(w, `{"data":null}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestPVELoginNeedsSysModify(t *testing.T) {
	srv := fakePVE(t)
	login := PVELogin(srv.URL, PinnedClient(srv.Certificate().Raw))
	ctx := context.Background()
	if err := login(ctx, "root@pam", "pw"); err != nil {
		t.Fatalf("root: %v", err)
	}
	if err := login(ctx, "root@pam", "bad"); err == nil {
		t.Fatal("wrong password accepted")
	}
	if err := login(ctx, "viewer@pve", "pw"); err == nil {
		t.Fatal("an account without Sys.Modify was accepted")
	}
}

func TestPinnedClientRefusesAnotherCertificate(t *testing.T) {
	srv := fakePVE(t)
	// Every httptest TLS server shares one certificate, so stand in a different one by its bytes.
	other := append([]byte{0}, srv.Certificate().Raw...)
	if err := PVELogin(srv.URL, PinnedClient(other))(context.Background(), "root@pam", "pw"); err == nil {
		t.Fatal("a server with a different certificate was trusted")
	}
}
