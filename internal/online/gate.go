package online

import (
	"errors"

	"github.com/antresol-app/antresol-addon-online/internal/corecopy/library"
)

var (
	// ErrKid: kid profiles get no online playback at all.
	ErrKid = errors.New("Недоступно в детском профиле")
	// ErrRating: the title's rating is above the profile's limit, or
	// unknown for a limited profile (the video rule).
	ErrRating = errors.New("Недоступно для этого профиля")
)

// Allowed is the gate of every online request: rating is the title's TMDB
// rating (RU certification, else US mapped), "" when unknown.
func Allowed(u User, rating string) error {
	if u.IsKid {
		return ErrKid
	}
	if u.MaxRating != "" && !library.RatingAllowed(u.MaxRating, library.Item{Kind: "movie", Rating: rating}) {
		return ErrRating
	}
	return nil
}
