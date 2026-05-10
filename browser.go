package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"image/jpeg"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, no CGo required

	_ "github.com/gen2brain/heic" // pure-Go HEIC decoder, registers with image.Decode

	"golang.org/x/image/draw"
)

//go:embed index.html
var frontend embed.FS

var baseDir string
var db *sql.DB

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
	dirFlag := flag.String("dir", ".", "Root directory to browse")
	portFlag := flag.String("port", "8080", "Port to listen on")
	userFlag := flag.String("user", "", "Username for HTTP basic auth (also reads $GALLERY_USER). Leave empty to disable auth.")
	passFlag := flag.String("pass", "", "Password for HTTP basic auth (also reads $GALLERY_PASS).")
	dbFlag := flag.String("db", "comments.db", "Path to SQLite database file for comments")
	flag.Parse()

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
	if authEnabled {
		fmt.Printf("🔒 Auth enabled for user %q\n", user)
	} else {
		fmt.Println("⚠️  Auth disabled (set -user and -pass to enable).")
	}
	fmt.Printf("🚀 Gallery at http://localhost%s\n", addr)
	if err := http.ListenAndServe(addr, withAuth(mux)); err != nil {
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
	fileRel := r.URL.Query().Get("file")
	fullPath := safeJoin(baseDir, fileRel)
	if fullPath == "" || isDir(fullPath) || !isImageFile(fullPath) {
		http.Error(w, "Not an image", http.StatusNotFound)
		return
	}

	file, err := os.Open(fullPath)
	if err != nil {
		http.Error(w, "Cannot open image", http.StatusInternalServerError)
		return
	}
	defer file.Close()

	img, _, err := image.Decode(file)
	if err != nil {
		http.Error(w, "Unsupported image format", http.StatusBadRequest)
		return
	}

	srcBounds := img.Bounds()
	width := 300
	height := int(float64(srcBounds.Dy()) * (float64(width) / float64(srcBounds.Dx())))
	dst := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.ApproxBiLinear.Scale(dst, dst.Bounds(), img, srcBounds, draw.Over, nil)

	w.Header().Set("Content-Type", "image/jpeg")
	jpeg.Encode(w, dst, &jpeg.Options{Quality: 85})
}

// handlePreview returns an image suitable for inline browser display. For
// formats browsers handle natively (JPEG/PNG/GIF) it just streams the file.
// For HEIC/HEIF it decodes server-side and re-encodes as JPEG so the browser
// can render it without any plugin.
func handlePreview(w http.ResponseWriter, r *http.Request) {
	fileRel := r.URL.Query().Get("file")
	fullPath := safeJoin(baseDir, fileRel)
	if fullPath == "" || isDir(fullPath) || !isImageFile(fullPath) {
		http.Error(w, "Not an image", http.StatusNotFound)
		return
	}

	// Native formats: stream the file, let the browser do the work.
	if !isHeicFile(fullPath) {
		if ct := contentTypeFor(fullPath); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		http.ServeFile(w, r, fullPath)
		return
	}

	// HEIC: decode and re-encode as JPEG. The gen2brain/heic blank import
	// registers HEIC with image.Decode, so this works the same as any other
	// format.
	file, err := os.Open(fullPath)
	if err != nil {
		http.Error(w, "Cannot open image", http.StatusInternalServerError)
		return
	}
	defer file.Close()

	img, _, err := image.Decode(file)
	if err != nil {
		http.Error(w, "Unsupported image format: "+err.Error(), http.StatusUnsupportedMediaType)
		return
	}

	w.Header().Set("Content-Type", "image/jpeg")
	// Cache aggressively — the underlying file's path is stable and re-encoding
	// is expensive. Browsers will revalidate on Cmd-Shift-R if needed.
	w.Header().Set("Cache-Control", "private, max-age=3600")
	if err := jpeg.Encode(w, img, &jpeg.Options{Quality: 90}); err != nil {
		// Already wrote headers; nothing useful to do but log.
		fmt.Fprintf(os.Stderr, "preview encode failed for %s: %v\n", fullPath, err)
	}
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
