package main

import (
	"crypto/sha256"
	"crypto/subtle"
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

	"golang.org/x/image/draw"
)

//go:embed index.html
var frontend embed.FS

var baseDir string

// Pre-hashed expected credentials. Hashing both the expected and the supplied
// values lets us compare with constant-time equality on equal-length inputs,
// which avoids timing leaks regardless of the actual password length.
var (
	authEnabled  bool
	expectedUser [32]byte
	expectedPass [32]byte
	authRealm    = "Gallery Browser"
)

type FileInfo struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Path        string `json:"path"`
	Size        string `json:"size,omitempty"`
	SizeBytes   int64  `json:"sizeBytes,omitempty"`
	IsImage     bool   `json:"isImage,omitempty"`
	ModTime     string `json:"modTime,omitempty"`     // human-readable, for display
	ModTimeUnix int64  `json:"modTimeUnix,omitempty"` // for sorting
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
		expectedUser = sha256.Sum256([]byte(user))
		expectedPass = sha256.Sum256([]byte(pass))
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", serveFrontend)
	mux.HandleFunc("/api/list", handleList)
	mux.HandleFunc("/api/download", handleDownload)
	mux.HandleFunc("/api/thumbnail", handleThumbnail)

	addr := ":" + *portFlag
	fmt.Printf("🌐 Serving: %s\n", baseDir)
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

// withAuth wraps the given handler with HTTP Basic Auth when credentials are
// configured. It is a no-op pass-through otherwise.
func withAuth(next http.Handler) http.Handler {
	if !authEnabled {
		return next
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
		next.ServeHTTP(w, r)
	})
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

	var contents []FileInfo
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
				ModTime:     modHuman,
				ModTimeUnix: modUnix,
			})
		}
	}

	sortContents(contents)
	resp := ListResponse{
		CurrentDir:         filepath.ToSlash(relPath),
		BasePathForDisplay: filepath.Base(baseDir),
		Contents:           contents,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
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
	return ext == ".jpg" || ext == ".jpeg" || ext == ".png" || ext == ".gif"
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
