package youtube

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"math"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// videoExts are the video files adopting picks up.
var videoExts = map[string]bool{".mp4": true, ".mkv": true, ".webm": true, ".m4v": true}

var (
	// reNameID is a video id in square brackets, as yt-dlp names files.
	reNameID = regexp.MustCompile(`\[([A-Za-z0-9_-]{11})\]`)
	// reNameDay is an upload date at the start of a name.
	reNameDay = regexp.MustCompile(`^(\d{4})-?(\d{2})-?(\d{2}) +`)
	// reNameIndex is Pinchflat's "NN - " at the start of a name.
	reNameIndex = regexp.MustCompile(`^\d{1,4} - `)
	// reSubLang is the language part of "<name>.<lang>.srt".
	reSubLang = regexp.MustCompile(`^[A-Za-z0-9_-]{1,35}$`)
	// reDuration is the length ffmpeg prints about its input.
	reDuration = regexp.MustCompile(`Duration: (\d+):(\d{2}):(\d{2}(?:\.\d+)?)`)
)

// ffmpegTimeout bounds one ffmpeg run of adopting.
const ffmpegTimeout = 2 * time.Minute

// adoptSource is one video file found and what its files say about it.
type adoptSource struct {
	item AdoptItem
	file string // full path
	size int64
	meta Meta
}

// target is the source's path under the person's folder: in folder, else
// its channel's folder, else one named like the folder it came from.
func (a adoptSource) target(m Meta, folder string) string {
	if folder == "" {
		folder = m.ChannelName
	}
	if folder == "" {
		folder = a.item.Folder
	}
	return SafeName(folder, NameBytes) + "/" + VideoBase(m.UploadedAt, m.Title, a.item.VideoID) + filepath.Ext(a.file)
}

// scanAdopt adds every video file under dir, rel being dir under the
// source folder. Hidden entries and symlinks are left alone.
func scanAdopt(dir, rel string, out *[]adoptSource) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		full := filepath.Join(dir, name)
		switch {
		case e.IsDir():
			if err := scanAdopt(full, path.Join(rel, name), out); err != nil {
				return err
			}
		case e.Type().IsRegular() && videoExts[strings.ToLower(filepath.Ext(name))]:
			src, err := readSource(full)
			if errors.Is(err, errNotPlain) {
				continue
			}
			if err != nil {
				return err
			}
			src.item.From, src.item.Folder = path.Join(rel, name), filepath.Base(dir)
			*out = append(*out, src)
		}
	}
	return nil
}

// readSource reads what a video's files say: its .nfo (Pinchflat, Kodi),
// then its .info.json (yt-dlp), then its name; the file's time is the
// upload date of last resort.
func readSource(file string) (adoptSource, error) {
	st, err := os.Lstat(file)
	if err != nil {
		return adoptSource{}, err
	}
	if !st.Mode().IsRegular() {
		return adoptSource{}, errNotPlain
	}
	stem := strings.TrimSuffix(file, filepath.Ext(file))
	src := adoptSource{file: file, size: st.Size()}
	m := &src.meta
	if n, ok := readNFO(stem + ".nfo"); ok {
		src.item.VideoID = n.youtubeID()
		m.Title, m.ChannelName = strings.TrimSpace(n.Title), strings.TrimSpace(n.ShowTitle)
		m.UploadedAt = parseDay(n.Aired)
		if m.UploadedAt == nil {
			m.UploadedAt = parseDay(n.Premiered)
		}
	}
	if raw, ok := readInfoJSON(stem + ".info.json"); ok {
		if src.item.VideoID == "" && reVideoID.MatchString(raw.ID) {
			src.item.VideoID = raw.ID
		}
		m.Title = or(m.Title, raw.Title)
		m.ChannelName = or(m.ChannelName, channelName(raw))
		m.ChannelID = or(m.ChannelID, raw.ChannelID)
		if m.UploadedAt == nil {
			m.UploadedAt = uploadedAt(raw)
		}
		m.DurationSeconds = seconds(raw.Duration)
	}
	title, day, id := parseName(filepath.Base(stem))
	if src.item.VideoID == "" {
		src.item.VideoID = id
	}
	m.Title = or(m.Title, title)
	if m.UploadedAt == nil {
		m.UploadedAt = day
	}
	if m.UploadedAt == nil {
		t := st.ModTime().UTC()
		m.UploadedAt = &t
	}
	return src, nil
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// nfoDoc is the part of a Kodi .nfo adopting reads.
type nfoDoc struct {
	Title     string `xml:"title"`
	ShowTitle string `xml:"showtitle"`
	Aired     string `xml:"aired"`
	Premiered string `xml:"premiered"`
	IDs       []struct {
		Type  string `xml:"type,attr"`
		Value string `xml:",chardata"`
	} `xml:"uniqueid"`
}

func (n nfoDoc) youtubeID() string {
	for _, id := range n.IDs {
		if v := strings.TrimSpace(id.Value); strings.EqualFold(id.Type, "youtube") && reVideoID.MatchString(v) {
			return v
		}
	}
	return ""
}

// readNFO reads an .nfo; false when there is none or it is not XML.
func readNFO(file string) (nfoDoc, bool) {
	b, err := readSmall(file)
	if err != nil {
		return nfoDoc{}, false
	}
	var n nfoDoc
	return n, xml.Unmarshal(b, &n) == nil
}

// readInfoJSON reads a yt-dlp .info.json; false when there is none or it
// is not JSON.
func readInfoJSON(file string) (rawInfo, bool) {
	b, err := readSmall(file)
	if err != nil {
		return rawInfo{}, false
	}
	var raw rawInfo
	return raw, json.Unmarshal(b, &raw) == nil
}

// readSmall reads a side file of up to 32 MiB.
func readSmall(file string) ([]byte, error) {
	f, _, err := openPlain(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, 32<<20))
}

// parseDay reads the YYYY-MM-DD date at the start of s, as an .nfo writes it.
func parseDay(s string) *time.Time {
	s = strings.TrimSpace(s)
	if len(s) < 10 {
		return nil
	}
	t, err := time.Parse("2006-01-02", s[:10])
	if err != nil {
		return nil
	}
	return &t
}

// parseName reads a title, an upload date and a video id from a file name
// without its extension: "YYYY-MM-DD Title [id]", "NN - Title" or "Title".
func parseName(stem string) (title string, day *time.Time, id string) {
	title = stem
	if all := reNameID.FindAllStringSubmatchIndex(title, -1); len(all) > 0 {
		m := all[len(all)-1]
		id = title[m[2]:m[3]]
		title = title[:m[0]] + title[m[1]:]
	}
	title = strings.TrimSpace(title)
	if m := reNameDay.FindStringSubmatch(title); m != nil {
		if t, err := time.Parse("2006-01-02", m[1]+"-"+m[2]+"-"+m[3]); err == nil {
			day = &t
			title = title[len(m[0]):]
		}
	}
	title = strings.TrimSpace(reNameIndex.ReplaceAllString(title, ""))
	return title, day, id
}

// copyFile copies from to to through "<to>.partial", checking the size
// before the rename. Errors carry no path: paths hold titles.
func copyFile(ctx context.Context, from, to string) error {
	in, st, err := openPlain(from)
	if err != nil {
		return pathFree(err)
	}
	defer in.Close()
	tmp := to + ".partial"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return pathFree(err)
	}
	n, err := io.Copy(out, ctxReader{ctx, in})
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil && n != st.Size() {
		err = errors.New("a copied video came out a different size")
	}
	if err == nil {
		err = os.Rename(tmp, to)
	}
	if err != nil {
		os.Remove(tmp)
		return pathFree(err)
	}
	return nil
}

// ctxReader stops a copy once its context ends.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// adoptExtras gives an adopted video file its thumbnail and subtitles and
// returns the files made. The thumbnail is the source's .jpg or -thumb.jpg,
// else its .webp or .png turned into a .jpg, else a frame at 10 % of the
// video. Subtitles "<name>.<lang>.srt" keep their language. Neither is
// needed: a failure leaves it out.
func adoptExtras(ctx context.Context, ffmpeg string, src adoptSource, file string, duration int) []string {
	var made []string
	if thumb := thumbOf(file); adoptThumb(ctx, ffmpeg, src, thumb, duration) == nil {
		made = append(made, thumb)
	}
	stem := strings.TrimSuffix(file, filepath.Ext(file))
	for lang, sub := range subtitlesOf(src.file) {
		to := stem + "." + lang + ".srt"
		if copyFile(ctx, sub, to) == nil {
			made = append(made, to)
		}
	}
	return made
}

func adoptThumb(ctx context.Context, ffmpeg string, src adoptSource, dst string, duration int) error {
	stem := strings.TrimSuffix(src.file, filepath.Ext(src.file))
	for _, c := range []string{".jpg", "-thumb.jpg", ".jpeg", "-thumb.jpeg"} {
		if regular(stem + c) {
			return copyFile(ctx, stem+c, dst)
		}
	}
	if ffmpeg == "" {
		return errors.New("no ffmpeg")
	}
	for _, c := range []string{".webp", "-thumb.webp", ".png", "-thumb.png"} {
		if regular(stem + c) {
			return ffmpegJPEG(ctx, ffmpeg, dst, "-i", stem+c)
		}
	}
	at := strconv.FormatFloat(float64(duration)/10, 'f', 2, 64)
	return ffmpegJPEG(ctx, ffmpeg, dst, "-ss", at, "-i", src.file)
}

// regular reports whether file is a plain file: a symlink is not, so
// nothing outside the source folder is read through one.
func regular(file string) bool {
	st, err := os.Lstat(file)
	return err == nil && st.Mode().IsRegular()
}

// errNotPlain refuses a source file that is a symlink or not a file.
var errNotPlain = errors.New("not a plain file")

// openPlain opens a source file only when it is a plain file and not a
// symlink, and checks that what was opened is what was looked at.
func openPlain(file string) (*os.File, os.FileInfo, error) {
	before, err := os.Lstat(file)
	if err != nil {
		return nil, nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, nil, errNotPlain
	}
	f, err := os.Open(file)
	if err != nil {
		return nil, nil, err
	}
	st, err := f.Stat()
	if err != nil || !os.SameFile(before, st) {
		f.Close()
		if err == nil {
			err = errNotPlain
		}
		return nil, nil, err
	}
	return f, st, nil
}

// ffmpegJPEG writes the first frame of the input as a .jpg at dst.
func ffmpegJPEG(ctx context.Context, ffmpeg, dst string, input ...string) error {
	ctx, cancel := context.WithTimeout(ctx, ffmpegTimeout)
	defer cancel()
	tmp := dst + ".partial"
	args := append([]string{"-nostdin", "-hide_banner", "-loglevel", "error", "-y"}, input...)
	args = append(args, "-frames:v", "1", "-c:v", "mjpeg", "-f", "image2", "-update", "1", tmp)
	if err := exec.CommandContext(ctx, ffmpeg, args...).Run(); err != nil {
		os.Remove(tmp)
		return err
	}
	if !regular(tmp) {
		return errors.New("ffmpeg made no thumbnail")
	}
	return os.Rename(tmp, dst)
}

// videoSeconds is a video's length as ffmpeg reads it, 0 when unknown.
func videoSeconds(ctx context.Context, ffmpeg, file string) int {
	if ffmpeg == "" {
		return 0
	}
	ctx, cancel := context.WithTimeout(ctx, ffmpegTimeout)
	defer cancel()
	// With no output ffmpeg exits non-zero after printing what it read.
	out, _ := exec.CommandContext(ctx, ffmpeg, "-nostdin", "-hide_banner", "-i", file).CombinedOutput()
	m := reDuration.FindSubmatch(out)
	if m == nil {
		return 0
	}
	h, _ := strconv.Atoi(string(m[1]))
	mins, _ := strconv.Atoi(string(m[2]))
	sec, _ := strconv.ParseFloat(string(m[3]), 64)
	return int(math.Round(float64(h*3600+mins*60) + sec))
}

// subtitlesOf finds the subtitles next to a video file, "<name>.<lang>.srt",
// by language.
func subtitlesOf(file string) map[string]string {
	dir, base := filepath.Split(file)
	stem := strings.TrimSuffix(base, filepath.Ext(base)) + "."
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := map[string]string{}
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() || !strings.HasPrefix(name, stem) || !strings.HasSuffix(name, ".srt") {
			continue
		}
		if lang := strings.TrimSuffix(name[len(stem):], ".srt"); reSubLang.MatchString(lang) {
			out[lang] = filepath.Join(dir, name)
		}
	}
	return out
}
