// Command sign signs release checksums in CI. The ed25519 private key comes from the
// UPDATE_SIGNING_KEY secret as PKCS#8 PEM (`openssl genpkey -algorithm ed25519`).
//
//	go run ./tools/sign pubkey            print the base64 DER public key to stamp into binaries
//	go run ./tools/sign sign FILE         write FILE.sig (base64 signature of FILE)
package main

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
)

func key() (ed25519.PrivateKey, error) {
	raw := os.Getenv("UPDATE_SIGNING_KEY")
	if raw == "" {
		return nil, errors.New("UPDATE_SIGNING_KEY is not set; see README > Releases")
	}
	block, _ := pem.Decode([]byte(raw))
	if block == nil {
		return nil, errors.New("UPDATE_SIGNING_KEY is not PEM")
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	ek, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("UPDATE_SIGNING_KEY is not an ed25519 key")
	}
	return ek, nil
}

func run() error {
	if len(os.Args) < 2 {
		return errors.New("usage: sign pubkey | sign FILE")
	}
	k, err := key()
	if err != nil {
		return err
	}
	switch os.Args[1] {
	case "pubkey":
		der, err := x509.MarshalPKIXPublicKey(k.Public())
		if err != nil {
			return err
		}
		fmt.Println(base64.StdEncoding.EncodeToString(der))
		return nil
	case "sign":
		if len(os.Args) != 3 {
			return errors.New("usage: sign sign FILE")
		}
		data, err := os.ReadFile(os.Args[2])
		if err != nil {
			return err
		}
		sig := base64.StdEncoding.EncodeToString(ed25519.Sign(k, data))
		return os.WriteFile(os.Args[2]+".sig", []byte(sig+"\n"), 0o644)
	}
	return fmt.Errorf("unknown command %q", os.Args[1])
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "sign:", err)
		os.Exit(1)
	}
}
