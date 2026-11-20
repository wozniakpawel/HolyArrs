package arrapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/HolyArrs/holyarrs/app/backend/internal/core/database"
	"github.com/HolyArrs/holyarrs/app/backend/internal/core/releasename"
	"github.com/HolyArrs/holyarrs/app/backend/internal/importer"
	"github.com/HolyArrs/holyarrs/app/backend/internal/mediafiles"
)

// The calls below are the ones phone apps such as Helmarr make for their
// History, disk and episode-file screens.

// historyKinds maps the Events a History row is made from to the old apps'
// event types. Other Events have no row there.
var historyKinds = map[string]string{
	"download_sent":      "grabbed",
	"import_done":        "downloadFolderImported",
	"import_partly_done": "downloadFolderImported",
	"download_failed":    "downloadFailed",
	"import_failed":      "downloadFailed",
	"renamed":            "fileRenamed", // side-specific name set in historyType
}

func historyType(side, kind string) string {
	t := historyKinds[kind]
	if t == "fileRenamed" {
		if side == "sonarr" {
			return "episodeFileRenamed"
		}
		return "movieFileRenamed"
	}
	return t
}

// historyRow is one History row as the old apps answer it.
type historyRow struct {
	ID                  int64          `json:"id"`
	EpisodeID           int64          `json:"episodeId,omitempty"`
	SeriesID            int64          `json:"seriesId,omitempty"`
	MovieID             int64          `json:"movieId,omitempty"`
	SourceTitle         string         `json:"sourceTitle"`
	Languages           []language     `json:"languages"`
	Quality             map[string]any `json:"quality"`
	CustomFormats       []any          `json:"customFormats"`
	CustomFormatScore   int            `json:"customFormatScore"`
	QualityCutoffNotMet bool           `json:"qualityCutoffNotMet"`
	Date                utc            `json:"date"`
	DownloadID          string         `json:"downloadId,omitempty"`
	EventType           string         `json:"eventType"`
	Data                map[string]any `json:"data"`
	Episode             *episode       `json:"episode,omitempty"`
	Series              *series        `json:"series,omitempty"`
	Movie               *movie         `json:"movie,omitempty"`
}

// historyMax caps how many Events one History answer reads. The old apps
// page through history, and phone apps ask for at most a few hundred rows.
const historyMax = 5000

// history pages this side's download and import Events, newest first.
func (h *handler) history(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p := pagingOf(r)
	if query(r, "pageSize") == "" {
		p.size = 10
	}
	if query(r, "sortDirection") == "" {
		p.asc, p.word = false, "descending"
	}
	rows, err := h.historyRows(ctx)
	if err != nil {
		internalError(w, err)
		return
	}
	if p.asc {
		slices.Reverse(rows)
	}
	if v := query(r, "eventType"); v != "" {
		rows = slices.DeleteFunc(rows, func(x historyRow) bool {
			return !strings.EqualFold(x.EventType, v) && v != historyNumber(h.side, x.EventType)
		})
	}
	if ids := idsParam(r, "episodeId"); len(ids) > 0 {
		rows = slices.DeleteFunc(rows, func(x historyRow) bool { return !slices.Contains(ids, x.EpisodeID) })
	}
	if ids := idsParam(r, "movieIds"); len(ids) > 0 {
		rows = slices.DeleteFunc(rows, func(x historyRow) bool { return !slices.Contains(ids, x.MovieID) })
	}
	from := min((p.page-1)*p.size, len(rows))
	to := min(from+p.size, len(rows))
	page := rows[from:to]
	if err := h.historyInclude(ctx, r, page); err != nil {
		internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"page": p.page, "pageSize": p.size, "sortKey": "date", "sortDirection": p.word,
		"totalRecords": len(rows), "records": page})
}

// historyNumber is the number the old apps also accept for an event type.
func historyNumber(side, t string) string {
	n := map[string]int{"grabbed": 1, "downloadFolderImported": 3, "downloadFailed": 4}[t]
	if t == "episodeFileRenamed" || t == "movieFileRenamed" {
		n = 6
	}
	if n == 0 {
		return ""
	}
	return strconv.Itoa(n)
}

// historyRows reads this side's rows, newest first.
func (h *handler) historyRows(ctx context.Context) ([]historyRow, error) {
	kind := map[string]string{"sonarr": "show", "radarr": "movie"}[h.side]
	db := h.Store.Read()
	kinds := map[int64]string{}
	if err := eachRow(ctx, db, "SELECT id, kind FROM titles WHERE state != 'removed'", func(rs *sql.Rows) error {
		var id int64
		var k string
		if err := rs.Scan(&id, &k); err != nil {
			return err
		}
		kinds[id] = k
		return nil
	}); err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(historyKinds))
	args := []any{}
	for k := range historyKinds {
		keys = append(keys, "?")
		args = append(args, k)
	}
	args = append(args, historyMax)
	var evs []database.Event
	err := eachRow(ctx, h.Store.ReadEvents(),
		"SELECT id, at, kind, title_id, slot_id, download_id, summary, data FROM events WHERE title_id IS NOT NULL AND kind IN ("+
			strings.Join(keys, ",")+") ORDER BY at DESC, id DESC LIMIT ?", func(rs *sql.Rows) error {
			var e database.Event
			var at, data string
			if err := rs.Scan(&e.ID, &at, &e.Kind, &e.TitleID, &e.SlotID, &e.DownloadID, &e.Summary, &data); err != nil {
				return err
			}
			e.At, _ = time.Parse(time.RFC3339Nano, at)
			_ = json.Unmarshal([]byte(data), &e.Data)
			evs = append(evs, e)
			return nil
		}, args...)
	if err != nil {
		return nil, err
	}
	// Episodes come from the Event's Slot.
	var slots []int64
	for _, e := range evs {
		if e.SlotID != nil {
			slots = append(slots, *e.SlotID)
		}
	}
	items := map[int64]int64{}
	if len(slots) > 0 {
		if err := eachRow(ctx, db, "SELECT id, item_id FROM slots WHERE id IN "+idList(slots), func(rs *sql.Rows) error {
			var id, item int64
			if err := rs.Scan(&id, &item); err != nil {
				return err
			}
			items[id] = item
			return nil
		}); err != nil {
			return nil, err
		}
	}
	downloads := map[int64]string{}
	var dls []int64
	for _, e := range evs {
		if e.DownloadID != nil {
			dls = append(dls, *e.DownloadID)
		}
	}
	if len(dls) > 0 {
		_ = eachRow(ctx, db, "SELECT id, COALESCE(external_id, '') FROM downloads WHERE id IN "+idList(dls), func(rs *sql.Rows) error {
			var id int64
			var ext string
			if err := rs.Scan(&id, &ext); err != nil {
				return err
			}
			downloads[id] = strings.ToUpper(ext)
			return nil
		})
	}
	out := []historyRow{}
	for _, e := range evs {
		if kinds[*e.TitleID] != kind {
			continue
		}
		row := historyRow{ID: e.ID, Date: utc(e.At), EventType: historyType(h.side, e.Kind), Languages: []language{}, CustomFormats: []any{},
			Data: map[string]any{}}
		if kind == "show" {
			row.SeriesID = *e.TitleID
			if e.SlotID != nil {
				row.EpisodeID = items[*e.SlotID]
			}
		} else {
			row.MovieID = *e.TitleID
		}
		name, _ := e.Data["release"].(string)
		if name == "" {
			name = e.Summary
		}
		row.SourceTitle = name
		row.Quality = qualityOfRelease(name)
		if e.DownloadID != nil {
			row.DownloadID = downloads[*e.DownloadID]
		}
		for k, v := range map[string]string{"downloadClient": "downloader", "reason": "reason"} {
			if s, ok := e.Data[v].(string); ok && s != "" {
				row.Data[k] = s
			}
		}
		out = append(out, row)
	}
	return out, nil
}

// qualityOfRelease names a release's quality the way mediaFileOf names a
// file's, from what the release name says.
func qualityOfRelease(name string) map[string]any {
	p := releasename.Parse(name, releasename.Hints{})
	q := qualityName(p.Source.Value, p.Resolution.Value)
	if q == "" {
		q = "Unknown"
	}
	return map[string]any{"quality": map[string]any{"name": q, "resolution": p.Resolution.Value},
		"revision": map[string]any{"version": 1, "real": 0, "isRepack": false}}
}

// historyInclude fills the episode, series or movie of each row when the
// caller asks for them.
func (h *handler) historyInclude(ctx context.Context, r *http.Request, rows []historyRow) error {
	if h.side == "radarr" {
		if !queryBool(r, "includeMovie") {
			return nil
		}
		cache := map[int64]*movie{}
		for i := range rows {
			id := rows[i].MovieID
			if _, ok := cache[id]; !ok {
				m, found, err := h.oneMovie(ctx, id, false)
				if err != nil {
					return err
				}
				if found {
					cache[id] = &m
				} else {
					cache[id] = nil
				}
			}
			rows[i].Movie = cache[id]
		}
		return nil
	}
	if queryBool(r, "includeSeries") {
		var ids []int64
		for _, x := range rows {
			ids = append(ids, x.SeriesID)
		}
		by, err := h.seriesByID(ctx, ids)
		if err != nil {
			return err
		}
		for i := range rows {
			if s, ok := by[rows[i].SeriesID]; ok {
				rows[i].Series = &s
			}
		}
	}
	if queryBool(r, "includeEpisode") {
		var ids []int64
		for _, x := range rows {
			if x.EpisodeID > 0 {
				ids = append(ids, x.EpisodeID)
			}
		}
		if len(ids) == 0 {
			return nil
		}
		eps, err := h.episodesWhere(ctx, "id IN "+idList(ids))
		if err != nil {
			return err
		}
		by := map[int64]episode{}
		for _, e := range eps {
			by[e.ID] = e
		}
		for i := range rows {
			if e, ok := by[rows[i].EpisodeID]; ok {
				rows[i].Episode = &e
			}
		}
	}
	return nil
}

// eachRow runs a query and hands each row to f.
func eachRow(ctx context.Context, db *sql.DB, q string, f func(*sql.Rows) error, args ...any) error {
	rs, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return err
	}
	defer rs.Close()
	for rs.Next() {
		if err := f(rs); err != nil {
			return err
		}
	}
	return rs.Err()
}

// diskSpace lists the disks under this side's Library folders, once each.
func (h *handler) diskSpace(w http.ResponseWriter, r *http.Request) {
	fs, err := database.Select[mediafiles.LibraryFolder](r.Context(), h.Store.Read(), "library_folders", "kind = ? ORDER BY id", h.folderKind())
	if err != nil {
		internalError(w, err)
		return
	}
	out := []map[string]any{}
	seen := map[[2]int64]bool{}
	for _, f := range fs {
		free, total := importer.FreeSpace(f.Path), importer.DiskSize(f.Path)
		if free < 0 || total < 0 {
			continue
		}
		key := [2]int64{free, total}
		if seen[key] { // the same disk under another folder
			continue
		}
		seen[key] = true
		out = append(out, map[string]any{"id": len(out) + 1, "path": f.Path, "label": f.Path, "freeSpace": free, "totalSpace": total})
	}
	writeJSON(w, http.StatusOK, out)
}

// customFilters answers that no saved filters exist; HolyArrs keeps its
// own in the page address.
func (h *handler) customFilters(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, []any{})
}

// episodeFiles lists the main-Version files of a show's episodes. It
// needs seriesId.
func (h *handler) episodeFiles(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var eps []episode
	var err error
	switch {
	case query(r, "seriesId") != "":
		var t mediafiles.Title
		t, err = h.title(ctx, atoi64(query(r, "seriesId")), "show")
		if err == nil {
			eps, err = h.episodesWhere(ctx, "title_id = ?", t.ID)
		}
	default:
		message(w, http.StatusBadRequest, "send seriesId")
		return
	}
	if err == nil {
		err = h.withFiles(ctx, eps)
	}
	if err != nil {
		internalError(w, err)
		return
	}
	// The fields Sonarr always sends with a file stand at their neutral values.
	type episodeFile struct {
		mediaFile
		CustomFormats       []any  `json:"customFormats"`
		CustomFormatScore   int    `json:"customFormatScore"`
		IndexerFlags        int    `json:"indexerFlags"`
		ReleaseType         string `json:"releaseType"`
		QualityCutoffNotMet bool   `json:"qualityCutoffNotMet"`
	}
	out := []episodeFile{}
	seen := map[int64]bool{}
	for _, e := range eps {
		if e.EpisodeFile == nil || seen[e.EpisodeFile.ID] {
			continue
		}
		seen[e.EpisodeFile.ID] = true
		f := *e.EpisodeFile
		f.SeriesID = e.SeriesID
		if f.SeasonNumber == nil {
			f.SeasonNumber = ptr(e.SeasonNumber)
		}
		out = append(out, episodeFile{mediaFile: f, CustomFormats: []any{}, ReleaseType: "singleEpisode"})
	}
	writeJSON(w, http.StatusOK, out)
}
