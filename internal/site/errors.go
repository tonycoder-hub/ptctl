package site

import "errors"

// Adapters may wrap these failure classes so the CLI can name a secret-free
// cause instead of its generic failure line. An adapter that never wraps them
// keeps the generic line, so adopting one is a deliberate public-text change.
var (
	ErrCredentialMalformed = errors.New("the credential read from stdin does not have the format this site uses")
	ErrCredentialRejected  = errors.New("the site rejected the credential")
	ErrRateLimited         = errors.New("the site rate limited the request; ptctl never retries automatically, so wait before trying again")
	ErrAddressBlocked      = errors.New("the site host resolved only to private or reserved addresses, which ptctl refuses to dial; a proxy using fake-IP DNS (for example 198.18.0.0/15) causes this")
)
