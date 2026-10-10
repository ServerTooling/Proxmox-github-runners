// Package buildinfo holds values stamped in at release build time with -ldflags -X.
package buildinfo

var (
	// Version is the release tag (build-<n> or v<semver>); "dev" for local builds, which never self-update.
	Version = "dev"
	// UpdatePublicKey is the base64 DER (SPKI) ed25519 key that release SHA256SUMS files are signed with.
	UpdatePublicKey = ""
	// RunnerVersion and RunnerSHA256 pin the actions/runner linux tarball; CI reads them from
	// the newest actions/runner release (packaging/runner-version is the floor) and verifies the hash
	// against the upstream release notes before stamping.
	RunnerVersion = ""
	RunnerSHA256  = ""
)
