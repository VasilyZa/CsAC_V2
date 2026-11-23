// Package stickerart serves the unchanged v1 built-in raster images.
package stickerart

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

const Prefix = "/v2/media/builtin-stickers/"

//go:embed assets/*.jpg assets/*.png assets/*.gif
var artwork embed.FS

type indexedAsset struct {
	file, mime, etag string
}

var index = buildIndex()

func buildIndex() map[string]indexedAsset {
	out := make(map[string]indexedAsset)
	entries, err := fs.ReadDir(artwork, "assets")
	if err != nil {
		panic(err)
	}
	for _, entry := range entries {
		filename := entry.Name()
		ext := path.Ext(filename)
		mimetype := map[string]string{".jpg": "image/jpeg", ".png": "image/png", ".gif": "image/gif"}[ext]
		code := strings.TrimSuffix(filename, ext)
		if mimetype == "" || out[code].file != "" {
			panic("invalid built-in sticker catalog")
		}
		f, err := artwork.Open("assets/" + filename)
		if err != nil {
			panic(err)
		}
		h := sha256.New()
		_, err = io.Copy(h, f)
		_ = f.Close()
		if err != nil {
			panic(err)
		}
		out[code] = indexedAsset{filename, mimetype, `"` + hex.EncodeToString(h.Sum(nil)) + `"`}
	}
	return out
}

func DownloadPath(code string) string {
	if _, exists := index[code]; !exists {
		return ""
	}
	return Prefix + code
}

func Serve(w http.ResponseWriter, r *http.Request) {
	code, starts := strings.CutPrefix(r.URL.EscapedPath(), Prefix)
	asset, found := index[code]
	if !starts || !found {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	f, err := artwork.Open("assets/" + asset.file)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Type", asset.mime)
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Header().Set("ETag", asset.etag)
	content, ok := f.(io.ReadSeeker)
	if !ok {
		http.Error(w, "Sticker unavailable", http.StatusInternalServerError)
		return
	}
	http.ServeContent(w, r, asset.file, time.Time{}, content)
}
