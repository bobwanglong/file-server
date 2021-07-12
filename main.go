// Copyright 2021, Joe Tsai. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE.md file.

package main

import (
	"bytes"
	"flag"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

var (
	addr     = flag.String("addr", ":8080", "The network address to listen on.")
	hide     = flag.String("hide", "/[.][^/]+(/|$)", "Regular expression of file paths to hide.\nPaths matching this pattern are excluded from directory listings,\nbut direct fetches for this path are still resolved.")
	deny     = flag.String("deny", "", "Regular expression of file paths to deny.\nPaths matching this pattern are excluded from directory listings\nand direct fetches for this path report StatusForbidden.")
	index    = flag.String("index", "", "Name of the index page to directly render for a directory.\n(e.g., 'index.html'; default none)")
	root     = flag.String("root", ".", "Directory to serve files from.")
	sendfile = flag.Bool("sendfile", true, "Allow the use of the sendfile syscall.")
	verbose  = flag.Bool("verbose", false, "Log every HTTP request.")

	hideRx *regexp.Regexp
	denyRx *regexp.Regexp
)

func main() {
	// Process command line flags.
	var err error
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Usage: %s [OPTION]...\n\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() > 0 {
		fmt.Fprintf(flag.CommandLine.Output(), "Invalid argument: %v\n\n", flag.Arg(0))
		flag.Usage()
		os.Exit(1)
	}
	if *hide != "" {
		hideRx, err = regexp.Compile(*hide)
		if err != nil {
			fmt.Fprintf(flag.CommandLine.Output(), "Invalid hide pattern: %v\n\n", *hide)
			flag.Usage()
			os.Exit(1)
		}
	}
	if *deny != "" {
		denyRx, err = regexp.Compile(*deny)
		if err != nil {
			fmt.Fprintf(flag.CommandLine.Output(), "Invalid deny pattern: %v\n\n", *deny)
			flag.Usage()
			os.Exit(1)
		}
	}
	if strings.Contains(*index, "/") || *index == "." || *index == ".." {
		fmt.Fprintf(flag.CommandLine.Output(), "Invalid index name: %v\n\n", *index)
		flag.Usage()
		os.Exit(1)
	}
	if _, err := os.Stat(*root); err != nil {
		fmt.Fprintf(flag.CommandLine.Output(), "Invalid root directory: %v\n\n", err)
		flag.Usage()
		os.Exit(1)
	}

	// Startup the file server.
	log.Printf("starting up server on %v", *addr)
	log.Fatal(http.ListenAndServe(*addr, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Never cache the server results. Consider it dynamically changing.
		w.Header().Set("Cache-Control", "no-cache, no-store, no-transform, must-revalidate, private, max-age=0")

		// For simplicity, always deal with clean paths that are absolute.
		// If the path had a trailing slash, preserve it.
		hadSlashSuffix := strings.HasSuffix(r.URL.Path, "/")
		r.URL.Path = "/" + strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if !strings.HasSuffix(r.URL.Path, "/") && hadSlashSuffix {
			r.URL.Path += "/"
		}

		// Log the request.
		if *verbose {
			log.Printf("%s %s", r.Method, r.URL.Path)
		}

		// Verify that the file exists.
		fp := filepath.Join(*root, filepath.FromSlash(r.URL.Path))
		fi, err := os.Stat(fp)
		if err != nil {
			httpError(w, err)
			return
		}
		f, err := os.Open(fp)
		if err != nil {
			httpError(w, err)
			return
		}
		defer f.Close()

		// Check that there is a trailing slash for only directories.
		if fi.IsDir() != strings.HasSuffix(r.URL.Path, "/") {
			if fi.IsDir() {
				relativeRedirect(w, r, path.Base(r.URL.Path)+"/") // directories always have slash suffix
				return
			} else {
				relativeRedirect(w, r, "../"+path.Base(r.URL.Path)) // files never have slash suffix
				return
			}
		}

		// Reject paths that match the deny pattern.
		if regexpMatch(denyRx, r.URL.Path) {
			httpError(w, os.ErrPermission)
			return
		}

		// Serve either a directory or a file.
		if fi.IsDir() {
			serveDirectory(w, r, fp, f)
		} else {
			var rs io.ReadSeeker = f
			if !*sendfile {
				// Drop the ReadFrom method to avoid the use of sendfile syscall.
				rs = struct{ io.ReadSeeker }{f}
			}
			http.ServeContent(w, r, fp, fi.ModTime(), rs)
		}
	})))
}

func serveDirectory(w http.ResponseWriter, r *http.Request, fp string, f *os.File) {
	// Serve the index page directly (if possible).
	if *index != "" {
		fp2 := filepath.Join(fp, *index)
		fi2, err := os.Stat(fp2)
		if err == nil {
			f2, err := os.Open(fp2)
			if err != nil {
				httpError(w, err)
				return
			}
			defer f2.Close()
			http.ServeContent(w, r, fp2, fi2.ModTime(), f2)
			return
		} else if !os.IsNotExist(err) {
			httpError(w, err)
			return
		}
	}

	// Read the directory entries, resolving any symbolic links,
	// and sorting all the entries by name.
	fis, err := f.Readdir(0)
	if err != nil {
		httpError(w, err)
		return
	}
	for i, fi := range fis {
		if fi.Mode()*os.ModeSymlink > 0 {
			if fi, _ := os.Stat(filepath.Join(fp, fi.Name())); fi != nil {
				fis[i] = fi // best effort resolution
			}
		}
	}
	sort.Slice(fis, func(i, j int) bool {
		return fis[i].Name() < fis[j].Name()
	})

	// Format the header.
	var bb bytes.Buffer
	bb.WriteString("<html lang=\"en\">\n")
	bb.WriteString("<head>\n")
	bb.WriteString("<title>" + html.EscapeString(r.URL.Path) + "</title>\n")
	bb.WriteString("<style>\n")
	bb.WriteString("body { font-family: monospace; }\n")
	bb.WriteString("h1 { margin: 0; }\n")
	bb.WriteString("th, td { text-align: left; }\n")
	bb.WriteString("th, td { padding-right: 2em; }\n")
	bb.WriteString("th { padding-bottom: 0.5em; }\n")
	bb.WriteString("a, a:visited, a:hover, a:active { color: blue; }\n")
	bb.WriteString("</style>\n")
	bb.WriteString("</head>\n")
	bb.WriteString("<body>\n")

	// Format the title.
	bb.WriteString("<h1>")
	names := strings.Split(strings.TrimSuffix(r.URL.Path, "/"), "/")
	for i, name := range names {
		if i > 0 {
			bb.WriteString(" ")
		}
		urlString := "." + strings.Repeat("/..", len(names)-1-i)
		bb.WriteString(`<a href="` + html.EscapeString(urlString) + `">` + html.EscapeString(name+"/") + `</a>`)
	}
	bb.WriteString("</h1>\n")

	bb.WriteString("<hr>\n")

	// Format the list of files and folders.
	bb.WriteString("<table>\n")
	bb.WriteString("<thead>\n")
	bb.WriteString("<tr>\n")
	bb.WriteString("<th>Name</th>\n")
	bb.WriteString("<th>Size</th>\n")
	bb.WriteString("<th>Last Modified</th>\n")
	bb.WriteString("</tr>\n")
	bb.WriteString("</thead>\n")
	bb.WriteString("<tbody>\n")
	now := time.Now()
	for _, fi := range fis {
		name := fi.Name()
		urlPath := path.Join(r.URL.Path, name)
		if fi.IsDir() {
			name += "/"
			urlPath += "/"
		}
		urlString := (&url.URL{Path: name}).String()
		if regexpMatch(hideRx, urlPath) || regexpMatch(denyRx, urlPath) {
			continue
		}
		bb.WriteString("<tr>\n")
		bb.WriteString("<td>")
		bb.WriteString(`<a href="` + html.EscapeString(urlString) + `">` + html.EscapeString(name) + `</a>`)
		bb.WriteString("</td>\n")
		bb.WriteString("<td>")
		if fi.Mode().IsRegular() {
			bb.WriteString(html.EscapeString(formatSize(fi.Size())))
		}
		bb.WriteString("</td>\n")
		bb.WriteString("<td>")
		bb.WriteString(html.EscapeString(formatTime(fi.ModTime(), now)))
		bb.WriteString("</td>\n")
		bb.WriteString("</tr>\n")
	}
	bb.WriteString("</tbody>\n")
	bb.WriteString("</table>\n")

	// Format the footer.
	bb.WriteString("</body>\n")
	bb.WriteString("</html>\n")
	w.Write(bb.Bytes())
}

func relativeRedirect(w http.ResponseWriter, r *http.Request, urlPath string) {
	if q := r.URL.RawQuery; q != "" {
		urlPath += "?" + q
	}
	w.Header().Set("Location", urlPath)
	w.WriteHeader(http.StatusMovedPermanently)
}

// regexpMatch is identical to r.MatchString(s),
// but reports false if r is nil.
func regexpMatch(r *regexp.Regexp, s string) bool {
	return r != nil && r.MatchString(s)
}

// formatSize returns the formatted size with IEC prefixes.
// E.g., 81533654 => 77.8MiB
func formatSize(i int64) string {
	units := "=KMGTPEZY"
	n := float64(i)
	for n >= 1024 {
		n /= 1024
		units = units[1:]
	}
	if units[0] == '=' {
		return fmt.Sprintf("%dB", int(n))
	} else {
		return fmt.Sprintf("%0.1f%ciB", n, units[0])
	}
}

// formatTime formats the timestamp with second granularity.
// Timestamps within 12 hours of now only print the time (e.g., "3:04 PM"),
// otherwise it is formatted as only the date (e.g., "Jan 2, 2006").
func formatTime(ts, now time.Time) string {
	if d := ts.Sub(now); -12*time.Hour < d && d < 12*time.Hour {
		return ts.Format("3:04 PM")
	} else {
		return ts.Format("Jan 2, 2006")
	}
}

func httpError(w http.ResponseWriter, err error) {
	switch {
	case os.IsNotExist(err):
		http.Error(w, "404 page not found", http.StatusNotFound)
	case os.IsPermission(err):
		http.Error(w, "403 Forbidden", http.StatusForbidden)
	default:
		http.Error(w, "500 Internal Server Error", http.StatusInternalServerError)
	}
}
