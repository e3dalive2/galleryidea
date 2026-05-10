package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	_ "image/gif"  // register GIF decoder with image.Decode
	"image/jpeg"
	_ "image/png"  // register PNG decoder with image.Decode
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, no CGo required

	"github.com/gen2brain/heic" // pure-Go HEIC decoder

	"golang.org/x/image/draw"
)

//go:embed index.html
var frontend embed.FS

var baseDir string
var db *sql.DB
var (
	thumbCacheDir   string // 300px JPEG thumbnails
	previewCacheDir string // 1080p JPEG previews
)

const (
	thumbWidth   = 300
	previewWidth = 1920 // longest-edge target for 1080p-class screens
	thumbQuality = 80
	previewJPEGQ = 85
)

// Pre-hashed expected credentials. Hashing both the expected and the supplied
// values lets us compare with constant-time equality on equal-length inputs,
// which avoids timing leaks regardless of the actual password length.
var (
	authEnabled  bool
	authUserName string // plaintext username, used to attribute comments
	expectedUser [32]byte
	expectedPass [32]byte
	authRealm    = "Gallery Browser"
)

// ctxKey is a typed key for stashing the authenticated username on the request
// context. Typed keys avoid collisions with other middleware.
type ctxKey string

const ctxUserKey ctxKey = "user"

type FileInfo struct {
	Name         string `json:"name"`
	Type         string `json:"type"`
	Path         string `json:"path"`
	Size         string `json:"size,omitempty"`
	SizeBytes    int64  `json:"sizeBytes,omitempty"`
	IsImage      bool   `json:"isImage,omitempty"`
	IsHeic       bool   `json:"isHeic,omitempty"`
	ModTime      string `json:"modTime,omitempty"`     // human-readable, for display
	ModTimeUnix  int64  `json:"modTimeUnix,omitempty"` // for sorting
	CommentCount int    `json:"commentCount"`          // 0 when no comments
}

type Comment struct {
	ID        int64  `json:"id"`
	FilePath  string `json:"filePath"`
	Author    string `json:"author"`
	Body      string `json:"body"`
	CreatedAt int64  `json:"createdAt"` // unix seconds
	Mine      bool   `json:"mine"`      // true if current user wrote it
}

type ListResponse struct {
	CurrentDir         string     `json:"currentDir"`
	BasePathForDisplay string     `json:"basePathForDisplay"`
	Contents           []FileInfo `json:"contents"`
	Error              string     `json:"error,omitempty"`
}

func main() {
	// Subprocess HEIC decoder mode. The parent server spawns us with this flag
	// to isolate HEIC decoding from the main process — a SIGSEGV in WASM/HEIC
	// then takes down only the child, not the gallery.
	decodeHeicFlag := flag.String("decode-heic", "", "(internal) decode given HEIC file to JPEG on stdout; used by the parent process")

	dirFlag := flag.String("dir", ".", "Root directory to browse")
	portFlag := flag.String("port", "8080", "Port to listen on")
	userFlag := flag.String("user", "", "Username for HTTP basic auth (also reads $GALLERY_USER). Leave empty to disable auth.")
	passFlag := flag.String("pass", "", "Password for HTTP basic auth (also reads $GALLERY_PASS).")
	dbFlag := flag.String("db", "comments.db", "Path to SQLite database file for comments")
	cacheFlag := flag.String("cache-dir", "", "Directory for image caches (thumbnails + 1080p previews). Default: <dir of -db>/cache")
	flag.Parse()

	if *decodeHeicFlag != "" {
		decodeHeicSubprocess(*decodeHeicFlag)
		return // unreachable in practice; subprocess always exits inside the call
	}

	abs, err := filepath.Abs(*dirFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Invalid directory: %v\n", err)
		os.Exit(1)
	}
	baseDir = abs
	info, err := os.Stat(baseDir)
	if err != nil || !info.IsDir() {
		fmt.Fprintf(os.Stderr, "Directory does not exist: %s\n", baseDir)
		os.Exit(1)
	}

	// Resolve credentials: flags win, env vars are the fallback so passwords
	// don't have to appear in shell history or `ps` output.
	user := *userFlag
	pass := *passFlag
	if user == "" {
		user = os.Getenv("GALLERY_USER")
	}
	if pass == "" {
		pass = os.Getenv("GALLERY_PASS")
	}
	if user != "" || pass != "" {
		if user == "" || pass == "" {
			fmt.Fprintln(os.Stderr, "Error: -user and -pass must both be set (or neither).")
			os.Exit(1)
		}
		authEnabled = true
		authUserName = user
		expectedUser = sha256.Sum256([]byte(user))
		expectedPass = sha256.Sum256([]byte(pass))
	}

	if err := initDB(*dbFlag); err != nil {
		fmt.Fprintf(os.Stderr, "Cannot open comments database: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	// Image caches: thumbnails (300px) and previews (1920px). Both speed up
	// repeat views and let slow disks/HEIC decoding pay off only once per file.
	cacheRoot := *cacheFlag
	if cacheRoot == "" {
		cacheRoot = filepath.Join(filepath.Dir(*dbFlag), "cache")
	}
	thumbCacheDir = filepath.Join(cacheRoot, "thumbs")
	previewCacheDir = filepath.Join(cacheRoot, "previews")
	for _, d := range []string{thumbCacheDir, previewCacheDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "Cannot create cache dir %s: %v\n", d, err)
			thumbCacheDir, previewCacheDir = "", "" // disable caching, keep going
			break
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", serveFrontend)
	mux.HandleFunc("/api/list", handleList)
	mux.HandleFunc("/api/download", handleDownload)
	mux.HandleFunc("/api/thumbnail", handleThumbnail)
	mux.HandleFunc("/api/preview", handlePreview) // converts HEIC to JPEG; passthrough for others
	mux.HandleFunc("/api/comments", handleComments) // GET, POST, DELETE

	addr := ":" + *portFlag
	fmt.Printf("🌐 Serving: %s\n", baseDir)
	fmt.Printf("💬 Comments DB: %s\n", *dbFlag)
	if thumbCacheDir != "" {
		fmt.Printf("🗂️  Image cache: %s\n", cacheRoot)
	} else {
		fmt.Println("⚠️  Image cache disabled (cache dir creation failed)")
	}
	if authEnabled {
		fmt.Printf("🔒 Auth enabled for user %q\n", user)
	} else {
		fmt.Println("⚠️  Auth disabled (set -user and -pass to enable).")
	}
	fmt.Printf("🚀 Gallery at http://localhost%s\n", addr)
	if err := http.ListenAndServe(addr, withRecover(withAuth(mux))); err != nil {
		fmt.Fprintf(os.Stderr, "Server error: %v\n", err)
		os.Exit(1)
	}
}

func initDB(path string) error {
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	// Enable WAL for better concurrent reads, and foreign keys (cheap, future-proof).
	if _, err := conn.Exec(`PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON;`); err != nil {
		return err
	}
	schema := `
		CREATE TABLE IF NOT EXISTS comments (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			file_path  TEXT NOT NULL,
			author     TEXT NOT NULL,
			body       TEXT NOT NULL,
			created_at INTEGER NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_comments_file ON comments(file_path, created_at);
	`
	if _, err := conn.Exec(schema); err != nil {
		return err
	}
	db = conn
	return nil
}

// withAuth wraps the given handler with HTTP Basic Auth when credentials are
// configured. On success the authenticated username is stashed on the request
// context so handlers can attribute actions (like comments) to the user.
func withAuth(next http.Handler) http.Handler {
	if !authEnabled {
		// Auth disabled: still set a generic username so comments work.
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := contextWithUser(r.Context(), "anonymous")
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok {
			requireAuth(w)
			return
		}
		gotUser := sha256.Sum256([]byte(user))
		gotPass := sha256.Sum256([]byte(pass))
		// subtle.ConstantTimeCompare returns 1 only when the byte slices are
		// equal AND the same length — true here because they're SHA-256 hashes.
		userOK := subtle.ConstantTimeCompare(gotUser[:], expectedUser[:]) == 1
		passOK := subtle.ConstantTimeCompare(gotPass[:], expectedPass[:]) == 1
		if !(userOK && passOK) {
			requireAuth(w)
			return
		}
		// Use the canonical configured username, not whatever case the client
		// typed. Keeps "alice" and "Alice" from looking like two users in the DB.
		ctx := contextWithUser(r.Context(), authUserName)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func contextWithUser(parent context.Context, user string) context.Context {
	return context.WithValue(parent, ctxUserKey, user)
}

func userFromRequest(r *http.Request) string {
	if v, ok := r.Context().Value(ctxUserKey).(string); ok {
		return v
	}
	return ""
}

func requireAuth(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Basic realm=%q, charset="UTF-8"`, authRealm))
	http.Error(w, "Unauthorized", http.StatusUnauthorized)
}

func serveFrontend(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := frontend.ReadFile("index.html")
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
}

func handleList(w http.ResponseWriter, r *http.Request) {
	relPath := r.URL.Query().Get("dir")
	fullPath := safeJoin(baseDir, relPath)
	if fullPath == "" || !isDir(fullPath) {
		sendJSONError(w, "Invalid directory", http.StatusNotFound)
		return
	}

	entries, err := os.ReadDir(fullPath)
	if err != nil {
		sendJSONError(w, "Cannot read directory", http.StatusInternalServerError)
		return
	}

	// Use an empty (non-nil) slice so empty folders serialize as `[]` rather
	// than `null`. The frontend reads `.length` on this and crashes on null.
	contents := make([]FileInfo, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		subPath := filepath.Join(fullPath, name)
		rel, _ := filepath.Rel(baseDir, subPath)
		rel = filepath.ToSlash(rel)

		info, _ := entry.Info()
		var modUnix int64
		var modHuman string
		if info != nil {
			t := info.ModTime()
			modUnix = t.Unix()
			modHuman = t.Format("2006-01-02 15:04")
		}

		if entry.IsDir() {
			contents = append(contents, FileInfo{
				Name:        name,
				Type:        "dir",
				Path:        rel,
				ModTime:     modHuman,
				ModTimeUnix: modUnix,
			})
		} else {
			sizeBytes := int64(0)
			if info != nil {
				sizeBytes = info.Size()
			}
			contents = append(contents, FileInfo{
				Name:        name,
				Type:        "file",
				Path:        rel,
				Size:        humanSize(sizeBytes),
				SizeBytes:   sizeBytes,
				IsImage:     isImageFile(name),
				IsHeic:      isHeicFile(name),
				ModTime:     modHuman,
				ModTimeUnix: modUnix,
			})
		}
	}

	sortContents(contents)

	// Annotate each entry with its comment count. A single query for the
	// whole directory is much cheaper than one query per file.
	if err := annotateCommentCounts(r.Context(), contents); err != nil {
		// Non-fatal: counts just won't show. Log to stderr and carry on.
		fmt.Fprintf(os.Stderr, "comment-count query failed: %v\n", err)
	}

	resp := ListResponse{
		CurrentDir:         filepath.ToSlash(relPath),
		BasePathForDisplay: filepath.Base(baseDir),
		Contents:           contents,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// annotateCommentCounts fills in CommentCount for every FileInfo using a single
// SQL query keyed on the path list.
func annotateCommentCounts(ctx context.Context, items []FileInfo) error {
	if db == nil || len(items) == 0 {
		return nil
	}
	// Build "(?, ?, ?, ...)" placeholder list and matching args slice.
	placeholders := make([]string, len(items))
	args := make([]interface{}, len(items))
	for i, it := range items {
		placeholders[i] = "?"
		args[i] = it.Path
	}
	q := `SELECT file_path, COUNT(*) FROM comments
	      WHERE file_path IN (` + strings.Join(placeholders, ",") + `)
	      GROUP BY file_path`
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	counts := make(map[string]int, len(items))
	for rows.Next() {
		var p string
		var c int
		if err := rows.Scan(&p, &c); err != nil {
			return err
		}
		counts[p] = c
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range items {
		items[i].CommentCount = counts[items[i].Path]
	}
	return nil
}

func handleDownload(w http.ResponseWriter, r *http.Request) {
	fileRel := r.URL.Query().Get("file")
	fullPath := safeJoin(baseDir, fileRel)
	if fullPath == "" || isDir(fullPath) {
		http.Error(w, "File not found", http.StatusNotFound)
		return
	}

	// Force download only when explicitly requested via ?download=1.
	// Otherwise serve inline so browsers can render images, PDFs, text, etc.
	disposition := "inline"
	if r.URL.Query().Get("download") == "1" {
		disposition = "attachment"
	}
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`%s; filename=%q`, disposition, filepath.Base(fullPath)))

	// Set an explicit Content-Type for images so browsers don't sniff it as
	// application/octet-stream (which also triggers a download in some setups).
	if ct := contentTypeFor(fullPath); ct != "" {
		w.Header().Set("Content-Type", ct)
	}

	http.ServeFile(w, r, fullPath)
}

func handleThumbnail(w http.ResponseWriter, r *http.Request) {
	serveResizedImage(w, r, thumbCacheDir, thumbWidth, thumbQuality)
}

// handlePreview serves a 1080p-class JPEG. Originals (often 12+ MB iPhone
// photos) are too big to push down a slow link for every preview, so we
// resize and cache them too. The download button uses /api/download for the
// original.
func handlePreview(w http.ResponseWriter, r *http.Request) {
	serveResizedImage(w, r, previewCacheDir, previewWidth, previewJPEGQ)
}

// serveResizedImage is the shared cache-aware resizer. It:
//   1. Validates the path and confirms it's an image.
//   2. Hashes (path + mtime + size) for the cache key.
//   3. Streams the cached JPEG straight from disk on hit.
//   4. On miss: decodes (subprocess for HEIC), resizes if needed, encodes
//      as JPEG, atomically writes the cache file, then serves it.
//
// The same function powers thumbnails (300px) and previews (1920px) — the
// only difference is the target width, JPEG quality, and cache directory.
func serveResizedImage(w http.ResponseWriter, r *http.Request, cacheDir string, targetWidth, quality int) {
	fileRel := r.URL.Query().Get("file")
	fullPath := safeJoin(baseDir, fileRel)
	if fullPath == "" || isDir(fullPath) || !isImageFile(fullPath) {
		http.Error(w, "Not an image", http.StatusNotFound)
		return
	}
	stat, err := os.Stat(fullPath)
	if err != nil {
		http.Error(w, "Cannot stat image", http.StatusInternalServerError)
		return
	}

	// 1. Cache hit?
	cachePath := cachedPathFor(cacheDir, fullPath, stat, targetWidth)
	if cachePath != "" {
		if f, err := os.Open(cachePath); err == nil {
			defer f.Close()
			w.Header().Set("Content-Type", "image/jpeg")
			w.Header().Set("Cache-Control", "private, max-age=3600")
			io.Copy(w, f)
			return
		}
	}

	// 2. Decode (HEIC out-of-process; everything else in-process).
	var src image.Image
	if isHeicFile(fullPath) {
		jpegBytes, err := decodeHeicToJPEG(fullPath)
		if err != nil {
			http.Error(w, "Unsupported image: "+err.Error(), http.StatusUnsupportedMediaType)
			return
		}
		src, err = decodeImage(bytes.NewReader(jpegBytes))
		if err != nil {
			http.Error(w, "Bad HEIC subprocess output: "+err.Error(), http.StatusInternalServerError)
			return
		}
	} else {
		f, err := os.Open(fullPath)
		if err != nil {
			http.Error(w, "Cannot open image", http.StatusInternalServerError)
			return
		}
		defer f.Close()
		src, err = decodeImage(f)
		if err != nil {
			http.Error(w, "Unsupported image format: "+err.Error(), http.StatusBadRequest)
			return
		}
	}

	// 3. Resize if larger than target. Skip resizing for already-small images
	// to avoid quality loss from upscaling.
	dst := resizeMaxWidth(src, targetWidth)

	// 4. Encode JPEG once, write to both response and cache file. MultiWriter
	// is cheaper than encoding twice.
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "private, max-age=3600")

	if cachePath != "" {
		if tmp, err := os.CreateTemp(cacheDir, ".tmp-*"); err == nil {
			mw := io.MultiWriter(w, tmp)
			if encErr := jpeg.Encode(mw, dst, &jpeg.Options{Quality: quality}); encErr != nil {
				fmt.Fprintf(os.Stderr, "encode failed for %s: %v\n", fullPath, encErr)
				tmp.Close()
				os.Remove(tmp.Name())
				return
			}
			tmp.Close()
			if err := os.Rename(tmp.Name(), cachePath); err != nil {
				os.Remove(tmp.Name())
			}
			return
		}
	}
	if err := jpeg.Encode(w, dst, &jpeg.Options{Quality: quality}); err != nil {
		fmt.Fprintf(os.Stderr, "encode failed for %s: %v\n", fullPath, err)
	}
}

// resizeMaxWidth returns src resized so its longest edge is at most max.
// Already-small images are returned unchanged. Aspect ratio preserved.
func resizeMaxWidth(src image.Image, max int) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	long := w
	if h > w {
		long = h
	}
	if long <= max {
		return src
	}
	scale := float64(max) / float64(long)
	nw, nh := int(float64(w)*scale), int(float64(h)*scale)
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	draw.ApproxBiLinear.Scale(dst, dst.Bounds(), src, b, draw.Over, nil)
	return dst
}

// cachedPathFor returns the disk cache path for a (file, target-width) pair.
// Empty string means caching is disabled. The width is part of the key so
// thumbnail and preview caches never collide even though they share the
// directory tree's parent.
func cachedPathFor(cacheDir, fullPath string, info os.FileInfo, width int) string {
	if cacheDir == "" {
		return ""
	}
	h := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%d|%d", fullPath, info.ModTime().UnixNano(), info.Size(), width)))
	return filepath.Join(cacheDir, fmt.Sprintf("%x.jpg", h[:16]))
}

// heicSem caps concurrent HEIC decodes. The WASM runtime is memory-hungry —
// a few iPhone-sized HEICs decoded in parallel can blow past several GB.
// Bounded concurrency keeps memory predictable and prevents OOM kills.
var heicSem = make(chan struct{}, 2)

// decodeHeicToJPEG runs a child process to decode the HEIC file at fullPath
// and returns the resulting JPEG bytes. Running in a subprocess isolates
// SIGSEGVs and unrecoverable runtime crashes from the WASM-based HEIC decoder
// — the child dies, the parent server stays up.
func decodeHeicToJPEG(fullPath string) ([]byte, error) {
	heicSem <- struct{}{}
	defer func() { <-heicSem }()

	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("os.Executable: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, self, "-decode-heic", fullPath)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// stderr may carry a useful "unsupported feature X" message from libheif.
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("HEIC decode failed: %s", msg)
	}
	if stdout.Len() == 0 {
		return nil, fmt.Errorf("HEIC decoder produced no output")
	}
	return stdout.Bytes(), nil
}

// decodeImage decodes a non-HEIC image. HEIC callers should use
// decodeHeicToJPEG and feed the resulting JPEG bytes back into image.Decode
// only when they need an image.Image (e.g. to resize for a thumbnail).
func decodeImage(r io.Reader) (image.Image, error) {
	im, _, err := image.Decode(r)
	return im, err
}

// decodeHeicSubprocess is the entry point for the child process spawned with
// `-decode-heic <file>`. It writes JPEG bytes to stdout and exits. A panic
// here only kills the child; the parent server reads exit status and stderr.
func decodeHeicSubprocess(path string) {
	if !isHeicFile(path) {
		fmt.Fprintf(os.Stderr, "decode-heic called on non-HEIC path: %s\n", path)
		os.Exit(2)
	}
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open: %v\n", err)
		os.Exit(1)
	}
	defer f.Close()

	defer func() {
		if rec := recover(); rec != nil {
			fmt.Fprintf(os.Stderr, "decoder panic: %v\n", rec)
			os.Exit(1)
		}
	}()

	img, err := heic.Decode(f)
	if err != nil {
		fmt.Fprintf(os.Stderr, "heic.Decode: %v\n", err)
		os.Exit(1)
	}
	if err := jpeg.Encode(os.Stdout, img, &jpeg.Options{Quality: 90}); err != nil {
		fmt.Fprintf(os.Stderr, "jpeg.Encode: %v\n", err)
		os.Exit(1)
	}
	os.Exit(0)
}

// Recovered HTTP middleware: any panic inside the handler chain becomes a
// 500 instead of crashing the process.
func withRecover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				fmt.Fprintf(os.Stderr, "panic in %s %s: %v\n", r.Method, r.URL.Path, rec)
				http.Error(w, "Internal server error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// handleComments routes by HTTP method:
//   GET    /api/comments?file=<path>   list comments for a file
//   POST   /api/comments               body {file, body} -> create
//   DELETE /api/comments?id=<n>        delete (only if author == caller)
const maxCommentLen = 4000

func handleComments(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		listComments(w, r)
	case http.MethodPost:
		createComment(w, r)
	case http.MethodDelete:
		deleteComment(w, r)
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func listComments(w http.ResponseWriter, r *http.Request) {
	fileRel := r.URL.Query().Get("file")
	if !validFileRef(fileRel) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid file"})
		return
	}
	rows, err := db.QueryContext(r.Context(),
		`SELECT id, file_path, author, body, created_at FROM comments
		 WHERE file_path = ? ORDER BY created_at ASC, id ASC`, fileRel)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "DB error"})
		return
	}
	defer rows.Close()
	me := userFromRequest(r)
	out := []Comment{} // not nil, so JSON renders [] not null
	for rows.Next() {
		var c Comment
		if err := rows.Scan(&c.ID, &c.FilePath, &c.Author, &c.Body, &c.CreatedAt); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "DB scan"})
			return
		}
		c.Mine = c.Author == me
		out = append(out, c)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"comments": out})
}

func createComment(w http.ResponseWriter, r *http.Request) {
	var in struct {
		File string `json:"file"`
		Body string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid JSON"})
		return
	}
	if !validFileRef(in.File) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid file"})
		return
	}
	body := strings.TrimSpace(in.Body)
	if body == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Empty comment"})
		return
	}
	if len(body) > maxCommentLen {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Comment too long"})
		return
	}
	author := userFromRequest(r)
	if author == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "No user"})
		return
	}
	now := time.Now().Unix()
	res, err := db.ExecContext(r.Context(),
		`INSERT INTO comments (file_path, author, body, created_at) VALUES (?, ?, ?, ?)`,
		in.File, author, body, now)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "DB error"})
		return
	}
	id, _ := res.LastInsertId()
	writeJSON(w, http.StatusCreated, Comment{
		ID: id, FilePath: in.File, Author: author, Body: body, CreatedAt: now, Mine: true,
	})
}

func deleteComment(w http.ResponseWriter, r *http.Request) {
	idStr := r.URL.Query().Get("id")
	if idStr == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Missing id"})
		return
	}
	me := userFromRequest(r)
	// Only delete if the author matches — guarded inside the SQL itself.
	res, err := db.ExecContext(r.Context(),
		`DELETE FROM comments WHERE id = ? AND author = ?`, idStr, me)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "DB error"})
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// Either the id didn't exist or it isn't yours — same response either way.
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "Not found or not yours"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// validFileRef checks the file/folder reference points inside baseDir.
// Reused for comment endpoints so attackers can't store comments against
// "../../etc/passwd" and have them surface elsewhere.
func validFileRef(rel string) bool {
	if rel == "" {
		return false
	}
	full := safeJoin(baseDir, rel)
	if full == "" {
		return false
	}
	_, err := os.Stat(full)
	return err == nil
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// Helper functions (same as before)
func safeJoin(base, rel string) string {
	if rel == "" {
		return base
	}
	cleanRel := filepath.Clean(rel)
	full := filepath.Join(base, cleanRel)
	if !strings.HasPrefix(full, base+string(os.PathSeparator)) && full != base {
		return ""
	}
	return full
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func isImageFile(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".jpg", ".jpeg", ".png", ".gif", ".heic", ".heif":
		return true
	}
	return false
}

func isHeicFile(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	return ext == ".heic" || ext == ".heif"
}

func contentTypeFor(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".svg":
		return "image/svg+xml"
	}
	return ""
}

func humanSize(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

func sortContents(items []FileInfo) {
	for i := 0; i < len(items)-1; i++ {
		for j := i + 1; j < len(items); j++ {
			if items[i].Type != items[j].Type {
				if items[i].Type == "file" && items[j].Type == "dir" {
					items[i], items[j] = items[j], items[i]
				}
				continue
			}
			if strings.ToLower(items[i].Name) > strings.ToLower(items[j].Name) {
				items[i], items[j] = items[j], items[i]
			}
		}
	}
}

func sendJSONError(w http.ResponseWriter, msg string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(ListResponse{Error: msg})
}
