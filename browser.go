package main

import (
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

//go:embed index.html
var frontend embed.FS

var baseDir string

type FileInfo struct {
	Name string `json:"name"`
	Type string `json:"type"` // "dir" or "file"
	Path string `json:"path"`
	Size string `json:"size,omitempty"`
}

type ListResponse struct {
	CurrentDir       string     `json:"currentDir"`
	BasePathForDisplay string   `json:"basePathForDisplay"`
	Contents         []FileInfo `json:"contents"`
	Error            string     `json:"error,omitempty"`
}

func main() {
	dirFlag := flag.String("dir", ".", "Root directory to browse (absolute or relative)")
	portFlag := flag.String("port", "8080", "Port to listen on")
	flag.Parse()

	// Resolve absolute path of the root directory
	abs, err := filepath.Abs(*dirFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Invalid directory: %v\n", err)
		os.Exit(1)
	}
	baseDir = abs
	info, err := os.Stat(baseDir)
	if err != nil || !info.IsDir() {
		fmt.Fprintf(os.Stderr, "Directory does not exist or is not a folder: %s\n", baseDir)
		os.Exit(1)
	}

	http.HandleFunc("/", serveFrontend)
	http.HandleFunc("/api/list", handleList)
	http.HandleFunc("/api/download", handleDownload)

	addr := ":" + *portFlag
	fmt.Printf("🌐 Serving directory: %s\n", baseDir)
	fmt.Printf("🚀 Starting server at http://localhost%s\n", addr)
	fmt.Println("Press Ctrl+C to stop")
	if err := http.ListenAndServe(addr, nil); err != nil {
		fmt.Fprintf(os.Stderr, "Server error: %v\n", err)
	}
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

	if fullPath == "" {
		sendJSONError(w, "Invalid path", http.StatusForbidden)
		return
	}

	info, err := os.Stat(fullPath)
	if err != nil || !info.IsDir() {
		sendJSONError(w, "Directory not found", http.StatusNotFound)
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

		if entry.IsDir() {
			contents = append(contents, FileInfo{
				Name: name,
				Type: "dir",
				Path: rel,
			})
		} else {
			info, err := entry.Info()
			sizeBytes := int64(0)
			if err == nil {
				sizeBytes = info.Size()
			}
			sizeHuman := humanSize(sizeBytes)
			contents = append(contents, FileInfo{
				Name: name,
				Type: "file",
				Path: rel,
				Size: sizeHuman,
			})
		}
	}

	// Sort: directories first, then by name
	sortContents(contents)

	resp := ListResponse{
		CurrentDir:       filepath.ToSlash(relPath),
		BasePathForDisplay: filepath.Base(baseDir),
		Contents:         contents,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func handleDownload(w http.ResponseWriter, r *http.Request) {
	fileRel := r.URL.Query().Get("file")
	fullPath := safeJoin(baseDir, fileRel)

	if fullPath == "" {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	info, err := os.Stat(fullPath)
	if err != nil || info.IsDir() {
		http.Error(w, "File not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", filepath.Base(fullPath)))
	http.ServeFile(w, r, fullPath)
}

// safeJoin prevents directory traversal attacks
func safeJoin(base, rel string) string {
	if rel == "" {
		return base
	}
	// Clean the relative path and join
	cleanRel := filepath.Clean(rel)
	// Ensure it doesn't try to escape using ".."
	full := filepath.Join(base, cleanRel)
	if !strings.HasPrefix(full, base+string(os.PathSeparator)) && full != base {
		return ""
	}
	return full
}

func sendJSONError(w http.ResponseWriter, msg string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(ListResponse{Error: msg})
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
			// Directories before files
			if items[i].Type != items[j].Type {
				if items[i].Type == "file" && items[j].Type == "dir" {
					items[i], items[j] = items[j], items[i]
				}
				continue
			}
			// Same type: alphabetical
			if strings.ToLower(items[i].Name) > strings.ToLower(items[j].Name) {
				items[i], items[j] = items[j], items[i]
			}
		}
	}
}
