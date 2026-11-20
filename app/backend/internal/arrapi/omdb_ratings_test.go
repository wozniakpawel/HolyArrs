package arrapi

import (
	"encoding/json"
	"testing"
)

// A movie's imdb rating comes from the kept OMDb scores, with metacritic and
// rottenTomatoes added as Radarr sends them; unknown scores stay as before.
func TestRadarrMovieRatingsFromOMDb(t *testing.T) {
	r := newPinRig(t)
	scored := r.movie("Scored", "100001", 1)
	r.movie("Plain", "100002", 2)
	r.exec("UPDATE titles SET score = 7.4, votes = 900, imdb_rating = 8.1, imdb_votes = 412345, rotten_tomatoes = 91, metacritic = 77 WHERE id = ?", scored)
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
	got := read("100001")
	want := map[string]rating{"imdb": {412345, 8.1, "user"}, "tmdb": {900, 7.4, "user"}, "metacritic": {0, 77, "user"}, "rottenTomatoes": {0, 91, "user"}}
	if len(got) != len(want) {
		t.Fatalf("ratings %+v", got)
	}
	for k, w := range want {
		if got[k] != w {
			t.Fatalf("%s: %+v, want %+v", k, got[k], w)
		}
	}
	plain := read("100002")
	if len(plain) != 2 || plain["imdb"] != (rating{0, 0, "user"}) {
		t.Fatalf("no OMDb scores: %+v", plain)
	}
}
