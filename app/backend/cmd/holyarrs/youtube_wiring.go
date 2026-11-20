package main

import (
	"context"
	"os"
	"os/exec"

	"github.com/HolyArrs/holyarrs/app/backend/internal/accounts"
	"github.com/HolyArrs/holyarrs/app/backend/internal/core/startup"
	"github.com/HolyArrs/holyarrs/app/backend/internal/core/webserver"
	"github.com/HolyArrs/holyarrs/app/backend/internal/importer"
	"github.com/HolyArrs/holyarrs/app/backend/internal/library"
	ytlib "github.com/HolyArrs/holyarrs/app/backend/internal/youtube"
)

// youtubeService builds the YouTube library from the app's services.
func (a *handlers) youtubeService() *ytlib.Service {
	st := a.s.Store
	svc := &ytlib.Service{Store: st, Queue: a.s.Queue,
		Tools: func() ytlib.Tools { return ytlib.FindTools(os.Getenv, exec.LookPath) },
		Run:   func(t ytlib.Tools) ytlib.Runner { return ytlib.Runner{Tools: t} },
		CheckFolder: func(ctx context.Context, path string) error {
			return library.CheckPlainFolder(ctx, st, path)
		},
		Person: func(ctx context.Context, userID int64) (accounts.PersonAccess, error) {
			return accounts.ReadPersonAccess(ctx, st.Read(), userID)
		},
		FreeSpace: importer.FreeSpace,
	}
	if a.s.Live != nil {
		svc.Live = a.s.Live
	}
	return svc
}

// youtubeRoutes registers the YouTube routes while the youtube feature
// switch is on; with it off they are not there at all.
func (a *handlers) youtubeRoutes(rt *webserver.Router) {
	if !startup.FeatureOn(startup.YouTube) {
		return
	}
	(&ytlib.Handlers{Svc: a.youtubeService()}).Routes(rt)
}
