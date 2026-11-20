package youtube

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/HolyArrs/holyarrs/app/backend/internal/accounts"
	"github.com/HolyArrs/holyarrs/app/backend/internal/core/database"
	"github.com/HolyArrs/holyarrs/app/backend/internal/core/jobs"
)

// Settings errors.
var (
	ErrFolderHasVideos = errors.New("downloaded videos are in the YouTube folder; the folder cannot change while they are there")
	ErrMinFree         = errors.New("the free space to keep must be between 1 and 100000 GB")
)

// Service is the YouTube library: settings, status, follows and downloads.
type Service struct {
	Store *database.Store
	Queue *jobs.Queue
	// Live sends an event to one person's open screens only.
	Live interface {
		PublishOwn(topic string, data any, userID int64) uint64
	}
	// Tools finds yt-dlp and ffmpeg; it runs on every call, so a program
	// installed while the app runs is found.
	Tools func() Tools
	// Run makes the yt-dlp runner for the tools found.
	Run func(Tools) Runner
	// CheckFolder checks a YouTube folder before it is saved.
	CheckFolder func(ctx context.Context, path string) error
	// Person reads what a person may do now.
	Person func(ctx context.Context, userID int64) (accounts.PersonAccess, error)
	// FreeSpace is the free bytes on the disk holding path, -1 when unknown.
	FreeSpace func(path string) int64
	Now       func() time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) runner() Runner {
	t := s.Tools()
	if s.Run != nil {
		return s.Run(t)
	}
	return Runner{Tools: t, Now: s.Now}
}

// Status is what the YouTube page needs before anything else: the missing
// programs, whether the folder is set, and whether downloads wait for space.
type Status struct {
	Missing    []string `json:"missing"`
	FolderSet  bool     `json:"folder_set"`
	LowSpace   bool     `json:"low_space"`
	FreeBytes  int64    `json:"free_bytes"` // -1 when unknown or no folder
	FloorBytes int64    `json:"floor_bytes"`
}

// Status reports the programs, the folder and its free space.
func (s *Service) Status(ctx context.Context) Status {
	set := LoadSettings(ctx, s.Store.Read())
	st := Status{Missing: s.Tools().Missing(), FolderSet: set.Folder != "", FreeBytes: -1, FloorBytes: set.floorBytes()}
	if st.Missing == nil {
		st.Missing = []string{}
	}
	if st.FolderSet && s.FreeSpace != nil {
		st.FreeBytes = s.FreeSpace(set.Folder)
	}
	st.LowSpace = st.FreeBytes >= 0 && st.FreeBytes < st.FloorBytes
	return st
}

// SaveSettings saves the folder and the free-space floor. A new folder is
// checked first, and refused while downloaded videos are in the old one
// (their saved paths are relative to it).
func (s *Service) SaveSettings(ctx context.Context, in Settings, by database.Actor) (Settings, error) {
	if in.MinFreeGB < 1 || in.MinFreeGB > MaxFreeGB {
		return Settings{}, ErrMinFree
	}
	folder := strings.TrimSpace(in.Folder)
	if folder != "" {
		folder = filepath.Clean(folder)
	}
	old := LoadSettings(ctx, s.Store.Read())
	if folder != old.Folder && folder != "" {
		if err := s.CheckFolder(ctx, folder); err != nil {
			return Settings{}, err
		}
	}
	err := s.Store.Write(ctx, func(tx *sql.Tx) error {
		cur, err := database.Setting(ctx, tx, settingsGroup, keyFolder)
		if err != nil {
			return err
		}
		if folder != cur {
			t, err := ReadTotals(ctx, tx)
			if err != nil {
				return err
			}
			if t.Videos > 0 {
				return ErrFolderHasVideos
			}
		}
		now := s.now()
		if err := database.PutSetting(tx, settingsGroup, keyFolder, folder, by, now); err != nil {
			return err
		}
		return database.PutSetting(tx, settingsGroup, keyMinFreeGB, strconv.Itoa(in.MinFreeGB), by, now)
	})
	if err != nil {
		return Settings{}, err
	}
	return LoadSettings(ctx, s.Store.Read()), nil
}
