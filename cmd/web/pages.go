package main

import (
	"bytes"
	"embed"
	"html/template"
	"log"
	"net/http"
	"time"

	"CoderPush/internal/report"
	"CoderPush/internal/store"
)

//go:embed templates
var templateFS embed.FS

const timeLayout = "2006-01-02 15:04:05 UTC"

// pages maps a page name (its file name without .html) to its template.
type pages map[string]*template.Template

var funcs = template.FuncMap{
	"money":    report.Money,
	"details":  report.Details,
	"versions": report.Versions,
	"plural":   report.Plural,
	"time":     func(t time.Time) string { return t.Format(timeLayout) },
	"timeOrDash": func(t *time.Time) string {
		if t == nil {
			return "-"
		}
		return t.Format(timeLayout)
	},
	"seconds": func(d time.Duration) int { return int(d / time.Second) },
	"count":   func(m map[store.ChangeKind]int, kind string) int { return m[store.ChangeKind(kind)] },
}

func parsePages() (pages, error) {
	p := pages{}
	for _, name := range []string{"dashboard", "runs", "run", "problems", "products"} {
		t, err := template.New("").Funcs(funcs).ParseFS(templateFS, "templates/layout.html", "templates/"+name+".html")
		if err != nil {
			return nil, err
		}
		p[name] = t
	}
	return p, nil
}

// view is what every page template receives.
type view struct {
	Title   string
	Refresh bool // reload every 2 s while an update runs
	Msg     string
	D       any
}

// render executes into a buffer first, so a template error becomes a
// clean 500 instead of half a page.
func (s *server) render(w http.ResponseWriter, name string, v view) {
	var buf bytes.Buffer
	if err := s.pages[name].ExecuteTemplate(&buf, "layout", v); err != nil {
		log.Printf("render %s: %v", name, err)
		http.Error(w, "could not render page", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	buf.WriteTo(w)
}
