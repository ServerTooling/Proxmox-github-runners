package setup

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var errLogin = errors.New("wrong user or password, or no Sys.Modify")

// PVELogin checks credentials against the Proxmox API's ticket endpoint (normally the local pveproxy at
// https://127.0.0.1:8006) and accepts only accounts with Sys.Modify on the nodes, the same right the
// Proxmox UI requires to change host settings. The ticket itself is discarded.
func PVELogin(base string, hc *http.Client) LoginFunc {
	return func(ctx context.Context, user, password string) error {
		form := url.Values{"username": {user}, "password": {password}}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/api2/json/access/ticket",
			strings.NewReader(form.Encode()))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := hc.Do(req)
		if err != nil {
			return fmt.Errorf("proxmox login: %w", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		var out struct {
			Data *struct {
				Ticket string                    `json:"ticket"`
				Cap    map[string]map[string]int `json:"cap"`
			} `json:"data"`
		}
		if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &out) != nil || out.Data == nil || out.Data.Ticket == "" {
			return errLogin
		}
		if out.Data.Cap["nodes"]["Sys.Modify"] != 1 {
			return errLogin
		}
		return nil
	}
}

// PinnedClient trusts exactly one certificate: the one pveproxy serves, read from the same file the
// settings page uses. Hostname checks don't apply on loopback and the cert may be self-signed or
// issued for the public name, so matching the exact certificate is the check that holds in every case.
func PinnedClient(leaf []byte) *http.Client {
	return &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{
		InsecureSkipVerify: true, // replaced by the exact-certificate check below
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 || !bytes.Equal(raw[0], leaf) {
				return errors.New("proxmox API certificate does not match the node's certificate")
			}
			return nil
		},
	}}}
}
