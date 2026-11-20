package arrapi

import (
	"encoding/json"
	"testing"
)

// A movie's trakt rating comes from the kept Trakt rating, with its votes,
// as Radarr sends it; a movie with none sends no trakt entry.
func TestRadarrMovieRatingsFromTrakt(t *testing.T) {
	r := newPinRig(t)
	rated := r.movie("Rated", "100001", 1)
	r.movie("Plain", "100002", 2)
	r.exec("UPDATE titles SET trakt_rating = 8.3347, trakt_votes = 52731 WHERE id = ?", rated)
	type rating struct {
		Votes int     `json:"votes"`
		Value float64 `json:"value"`
		Type  string  `json:"type"`
	}
	read := func(tmdb string) map[string]rating {
		t.Helper()
		st, b := r.radarr("movie?tmdbId=" + tmdb)
		var list []struct {
			Ratings map[string]rating `json:"ratings"`
		}
		if st != 200 || json.Unmarshal(b, &list) != nil || len(list) != 1 {
			t.Fatalf("%d %s", st, b)
		}
		return list[0].Ratings
	}
	if got := read("100001")["trakt"]; got != (rating{52731, 8.3347, "user"}) {
		t.Fatalf("trakt %+v", got)
	}
	if _, ok := read("100002")["trakt"]; ok {
		t.Fatal("a movie with no Trakt rating sent one")
	}
}
