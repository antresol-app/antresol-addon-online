package online

import (
	"context"

	"github.com/antresol-app/antresol/addonkit"
)

// User is the profile a request acts as: the X-Antresol-User header the
// core sends with every request through its proxy (addonkit.User). The
// scanner and the channels act as identities no profile has (negative
// ids, no uid).
type User = addonkit.User

type profileKey struct{}
type mediaKeyKey struct{}
type relativeKey struct{}

// WithUser is ctx carrying u (the routes put the header's profile there).
func WithUser(ctx context.Context, u User) context.Context {
	return context.WithValue(ctx, profileKey{}, u)
}

// UserFrom is the profile WithUser put in ctx.
func UserFrom(ctx context.Context) (User, bool) {
	u, ok := ctx.Value(profileKey{}).(User)
	return u, ok
}

// WithMediaKey is ctx of a request let in with media key mk (a player
// that sends no session, such as AirPlay): the stream proxy hands it on
// in the URLs it writes.
func WithMediaKey(ctx context.Context, mk string) context.Context {
	return context.WithValue(ctx, mediaKeyKey{}, mk)
}

// ViaMediaKey is the media key a request was let in with.
func ViaMediaKey(ctx context.Context) (string, bool) {
	k, ok := ctx.Value(mediaKeyKey{}).(string)
	return k, ok
}

// withRelativeURIs is ctx of a request whose rewritten playlists give
// their URIs relative to the playlist (the channels' socket routes, read
// by the core's relay under the stream's folder).
func withRelativeURIs(ctx context.Context) context.Context {
	return context.WithValue(ctx, relativeKey{}, true)
}

func relativeURIs(ctx context.Context) bool {
	v, _ := ctx.Value(relativeKey{}).(bool)
	return v
}
