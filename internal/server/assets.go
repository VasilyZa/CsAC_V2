package server

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const defaultAvatar = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 96 96"><rect width="96" height="96" rx="20" fill="#e4edf7"/><circle cx="48" cy="35" r="15" fill="#45647f"/><path d="M19 77a29 26 0 0 1 58 0Z" fill="#45647f"/></svg>`

var defaultAvatarETag = fmt.Sprintf(`"%x"`, sha256.Sum256([]byte(defaultAvatar)))

func serveDefaultAvatar(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Header().Set("ETag", defaultAvatarETag)
	http.ServeContent(w, r, "default-avatar.svg", time.Time{}, strings.NewReader(defaultAvatar))
}
