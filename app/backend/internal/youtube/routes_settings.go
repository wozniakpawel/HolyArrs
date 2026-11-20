package youtube

import (
	"context"
	"errors"
	"net/http"

	"github.com/HolyArrs/holyarrs/app/backend/internal/core/webserver"
	"github.com/HolyArrs/holyarrs/app/backend/internal/library"
)

// settingsView is the YouTube settings card: the settings, the library
// total over everyone (counts only, no titles) and yt-dlp's version.
type settingsView struct {
	Settings
	Totals  Totals `json:"totals"`
	Version string `json:"version"` // "" when yt-dlp is missing or does not answer
}

func (h *Handlers) view(ctx context.Context) (settingsView, error) {
	v := settingsView{Settings: LoadSettings(ctx, h.Svc.Store.Read())}
	t, err := ReadTotals(ctx, h.Svc.Store.Read())
	if err != nil {
		return v, err
	}
	v.Totals = t
	if r := h.Svc.runner(); r.Tools.YTDLP != "" {
		v.Version, _ = r.Version(ctx)
	}
	return v, nil
}

func (h *Handlers) getSettings(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.caller(w, r); !ok {
		return
	}
	v, err := h.view(r.Context())
	if err != nil {
		webserver.Internal(w, "youtube", err)
		return
	}
	webserver.WriteJSON(w, http.StatusOK, v)
}

func (h *Handlers) putSettings(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.caller(w, r); !ok {
		return
	}
	// Fields left out keep their saved values.
	in := LoadSettings(r.Context(), h.Svc.Store.Read())
	if !webserver.Decode(w, r, &in, `send {"folder": "/data/youtube", "min_free_gb": 10}`) {
		return
	}
	if _, err := h.Svc.SaveSettings(r.Context(), in, webserver.RequestActor(r)); err != nil {
		switch {
		case errors.Is(err, ErrFolderHasVideos):
			webserver.Fail(w, http.StatusConflict, err.Error())
		case errors.Is(err, ErrMinFree), folderError(err):
			webserver.Fail(w, http.StatusBadRequest, err.Error())
		default:
			webserver.Internal(w, "youtube", err)
		}
		return
	}
	h.getSettings(w, r)
}

// folderError reports whether err is a refused folder rather than a fault.
func folderError(err error) bool {
	for _, e := range []error{library.ErrFolderPath, library.ErrFolderAbsent, library.ErrFolderReadOnly, library.ErrFolderOverlap} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}
