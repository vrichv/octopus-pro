// Package privacyfilter vendors the core filter package from
// https://github.com/packyme/privacy-filter.
//
// Vendored upstream files:
//   - filter/filter.go
//   - filter/pii.go
//   - filter/secrets.go
//   - rules/gitleaks.toml
//   - filter/testdata/
//
// Local changes:
//   - package filter was renamed to package privacyfilter
//   - gitleaks.toml is embedded into the binary
//   - NewFromBytes was added so callers can initialize from embedded rules
//
// To update, manually sync the upstream files above, keep the local changes,
// then run: go test ./internal/privacyfilter/...
package privacyfilter
