package online

import "testing"

func adult() User { return User{ID: 1, Name: "Мама"} }

func limited(r string) User { return User{ID: 2, Name: "Петя", MaxRating: r} }

func TestGate(t *testing.T) {
	if err := Allowed(User{ID: 3, IsKid: true}, "0+"); err != ErrKid {
		t.Fatalf("kid: %v", err)
	}
	if err := Allowed(adult(), ""); err != nil {
		t.Fatalf("adult, unrated: %v", err)
	}
	for rating, want := range map[string]error{"18+": ErrRating, "": ErrRating, "12+": nil, "6+": nil} {
		if err := Allowed(limited("12+"), rating); err != want {
			t.Errorf("12+ on %q: %v", rating, err)
		}
	}
	if err := Allowed(User{ID: 4, MaxRating: ""}, "18+"); err != nil {
		t.Fatalf("no limit: %v", err)
	}
}
