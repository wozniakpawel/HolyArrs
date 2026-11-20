package sources

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The fixtures under testdata/sites are trimmed copies of each site's real
// answers.

// sz is a size as the site writes it, such as 1.5 GB.
func sz(f float64, shift uint) int64 { return int64(f * float64(uint64(1)<<shift)) }

func one(t *testing.T, items []Item, err error, n int) []Item {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != n {
		t.Fatalf("got %d items, want %d: %+v", len(items), n, items)
	}
	return items
}

func wantItem(t *testing.T, got Item, title, link string, size int64, seeders, peers int, hash string) {
	t.Helper()
	if got.Title != title || !strings.HasPrefix(got.Link, link) || got.Size != size || got.InfoHash != hash || got.Protocol != "torrent" {
		t.Errorf("item %+v\nwant title %q link %q size %d hash %q", got, title, link, size, hash)
	}
	if seeders >= 0 && (got.Seeders == nil || *got.Seeders != seeders || got.Peers == nil || *got.Peers != peers) {
		t.Errorf("%q: seeders %v peers %v, want %d %d", got.Title, got.Seeders, got.Peers, seeders, peers)
	}
}

func TestEverySiteHasASearch(t *testing.T) {
	for _, s := range Sites {
		switch s.ID {
		case "knaben", "torrentscsv", "subsplease", "iptorrents":
			continue
		}
		if moreSites[s.ID] == nil {
			t.Errorf("%s has no search", s.ID)
		}
	}
}

func TestThePirateBay(t *testing.T) {
	t.Parallel()
	var asked []string
	srv := siteServer(t, "thepiratebay.json", func(r *http.Request, _ string) { asked = append(asked, r.URL.RequestURI()) })
	items, err := siteClient(srv.URL, "thepiratebay", "").Search(context.Background(), Query{Title: "Severance", TV: true, Season: 1, Episode: "1"})
	items = one(t, items, err, 3)
	if len(asked) != 1 || asked[0] != "/q.php?cat=205%2C208%2C212&q=Severance+S01E01" {
		t.Fatalf("asked %q", asked)
	}
	wantItem(t, items[0], "Severance S01E01 Good News About Hell 1080p BluRay 10Bit DDP5 1 H265-d3g", "magnet:?xt=urn:btih:9353dfdad43e6f267e9d495a6838679c1ec15d8b&dn=Severance",
		2609557292, 113, 127, "9353dfdad43e6f267e9d495a6838679c1ec15d8b")
	if !items[0].Published.Equal(time.Unix(1735661076, 0)) || items[0].Files != 3 || items[0].GUID != "https://thepiratebay.org/description.php?id=77711156" {
		t.Errorf("item %+v", items[0])
	}
	if !strings.Contains(items[0].Link, "&tr=udp%3A%2F%2Ftracker.opentrackr.org") {
		t.Errorf("magnet has no trackers: %s", items[0].Link)
	}
}

func TestThePirateBay_MovieByIMDbThenWords(t *testing.T) {
	t.Parallel()
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Query().Get("q"))
		// The site says "no results" with one empty row.
		_, _ = w.Write([]byte(`[{"id":"0","name":"No results returned","info_hash":"0000000000000000000000000000000000000000","leechers":"0","seeders":"0","num_files":"0","size":"0","username":"","added":"0","status":"member","category":"0","imdb":""}]`))
	}))
	t.Cleanup(srv.Close)
	items, err := siteClient(srv.URL, "thepiratebay", "").Search(context.Background(), Query{Title: "The Descent", Year: 2005, IMDb: "tt0435625"})
	one(t, items, err, 0)
	if strings.Join(asked, "|") != "tt0435625|The Descent 2005" {
		t.Fatalf("asked %q", asked)
	}
}

func TestYTS(t *testing.T) {
	t.Parallel()
	srv := siteServer(t, "yts.json", func(r *http.Request, _ string) {
		if r.URL.Path != "/api/v2/list_movies.json" || r.URL.Query().Get("query_term") != "tt0435625" || r.URL.Query().Get("limit") != "50" {
			t.Errorf("asked %s", r.URL.RequestURI())
		}
	})
	items, err := siteClient(srv.URL, "yts", "").Search(context.Background(), Query{Title: "The Descent", Year: 2005, IMDb: "tt0435625"})
	items = one(t, items, err, 2)
	wantItem(t, items[0], "The Descent (2005) 720p BRRip x264 -YTS", "magnet:?xt=urn:btih:5a8f6f94cf7844439cb12747f331defb7de4bc5d", 962005565, 36, 49, "5a8f6f94cf7844439cb12747f331defb7de4bc5d")
	wantItem(t, items[1], "The Descent (2005) 1080p BRRip 5.1 x264 -YTS", "magnet:?xt=urn:btih:1b70c84bd49e7079b0347a6dd3045ebfaf0515e1", 1975684956, 100, 121, "1b70c84bd49e7079b0347a6dd3045ebfaf0515e1")
	if items[0].IMDb != "435625" || !items[0].Published.Equal(time.Unix(1620478315, 0)) {
		t.Errorf("item %+v", items[0])
	}
}

func TestYTS_TitleWithoutYear(t *testing.T) {
	t.Parallel()
	srv := siteServer(t, "yts.json", func(r *http.Request, _ string) {
		if r.URL.Query().Get("query_term") != "The Descent" {
			t.Errorf("asked %s", r.URL.RequestURI())
		}
	})
	_, err := siteClient(srv.URL, "yts", "").Search(context.Background(), Query{Title: "The Descent", Year: 2005})
	if err != nil {
		t.Fatal(err)
	}
}

func TestBangumiMoe(t *testing.T) {
	t.Parallel()
	srv := siteServer(t, "bangumimoe.json", func(r *http.Request, body string) {
		var b map[string]string
		if r.Method != http.MethodPost || r.URL.Path != "/api/v2/torrent/search" || json.Unmarshal([]byte(body), &b) != nil || b["query"] != "Frieren S01" {
			t.Errorf("asked %s %s %s", r.Method, r.URL.Path, body)
		}
	})
	items, err := siteClient(srv.URL, "bangumimoe", "").Search(context.Background(), Query{Title: "Frieren", TV: true, Season: 1})
	items = one(t, items, err, 2)
	wantItem(t, items[0], "[7³ACG] 葬送的芙莉莲/Sousou no Frieren S01 | 01-28+SPx11 [简繁字幕] BDrip 1080p x265 OPUS 2.0", "magnet:?xt=urn:btih:f8651444a2c0b3ff7e3da3cbd75da58ef5f09e19",
		sz(52.02, 30), 0, 0, "f8651444a2c0b3ff7e3da3cbd75da58ef5f09e19")
	if items[0].GUID != srv.URL+"/torrent/6a90acf49605d5c690106883" || items[0].Published.Format(time.RFC3339) != "2026-08-27T21:32:36Z" {
		t.Errorf("item %+v", items[0])
	}
}

func TestBangumiMoe_Newest(t *testing.T) {
	t.Parallel()
	srv := siteServer(t, "bangumimoe.json", func(r *http.Request, _ string) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/torrent/latest" {
			t.Errorf("asked %s %s", r.Method, r.URL.Path)
		}
	})
	items, err := siteClient(srv.URL, "bangumimoe", "").Feed(context.Background(), nil)
	one(t, items, err, 2)
}

func TestNyaa(t *testing.T) {
	t.Parallel()
	srv := siteServer(t, "nyaasi.xml", func(r *http.Request, _ string) {
		q := r.URL.Query()
		if q.Get("page") != "rss" || q.Get("q") != "Frieren 05" || q.Get("c") != "0_0" || q.Get("f") != "0" {
			t.Errorf("asked %s", r.URL.RequestURI())
		}
	})
	items, err := siteClient(srv.URL, "nyaasi", "").Search(context.Background(), Query{Title: "Frieren 05", TV: true})
	items = one(t, items, err, 3)
	wantItem(t, items[0], "Frieren - Oltre la Fine del Viaggio (2023) [BluRay.1080p.x265.FLAC.ITA.JPN.Subs.Ita.Eng.Jpn] [42.6GB] [stress]",
		"https://nyaa.si/download/2180356.torrent", sz(42.6, 30), 5, 6, "444d6b7665bf616065d4c3793642bc41c17e767e")
	if items[0].GUID != "https://nyaa.si/view/2180356" || items[0].Published.Format(time.RFC3339) != "2026-10-30T15:32:00Z" {
		t.Errorf("item %+v", items[0])
	}
}

func TestTokyoToshokan(t *testing.T) {
	t.Parallel()
	srv := siteServer(t, "tokyotosho.xml", func(r *http.Request, _ string) {
		if r.URL.Path != "/rss.php" || r.URL.Query().Get("terms") != "Frieren" {
			t.Errorf("asked %s", r.URL.RequestURI())
		}
	})
	items, err := siteClient(srv.URL, "tokyotosho", "").Search(context.Background(), Query{Title: "Frieren", TV: true})
	items = one(t, items, err, 3)
	wantItem(t, items[0], "Lila Minx - Frieren.zip", "magnet:?xt=urn:btih:RVO3WDXD45Q3VY6YTNB2FLIR4HSRT3SN", sz(25.47, 20), -1, -1, "8d5dbb0ee3e761bae3d89b43a2ad11e1e519ee4d")
	if items[0].Seeders != nil || items[0].GUID != "https://www.tokyotosho.info/details.php?id=2148176" || items[0].Published.Format(time.RFC3339) != "2026-11-19T00:27:33Z" {
		t.Errorf("item %+v", items[0])
	}
}
