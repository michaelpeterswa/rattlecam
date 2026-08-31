package gateway

import (
	"bytes"
	_ "embed"
	"fmt"
	"html/template"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"
)

//go:embed index.html.tmpl
var indexTemplate string

var indexTmpl = template.Must(template.New("index").Parse(indexTemplate))

// entry is one row of the listing.
type entry struct {
	Path        string
	Title       string
	Description string
	Size        string
	Updated     string
	Available   bool
}

// group is one section of the listing.
type group struct {
	Name    string
	Note    string
	Entries []entry
}

// groupFor sorts an object into a section by what it is.
//
// Derived from the extension rather than declared per object, because a new
// object added to the route table should appear in the right section without
// anyone remembering to say which — the failure mode of a hand-maintained
// grouping is a file that quietly lists nowhere.
func groupFor(name string) (string, string) {
	switch strings.ToLower(path.Ext(name)) {
	case ".jpg", ".jpeg":
		return "Live frames", "Updated every ten minutes, straight off the camera."
	case ".mp4":
		return "Timelapses", "H.264, 1280×720, 24 fps. These accept range requests, so a <video> element can seek."
	case ".gif":
		return "Animated previews", "The same timelapses as GIF, 480 px wide, for dropping into a page as a plain <img>."
	default:
		return "Other", ""
	}
}

// groupOrder is the order sections appear, most immediate first.
var groupOrder = []string{"Live frames", "Timelapses", "Animated previews", "Other"}

// index renders the listing at /.
//
// It is built from the same route table the gateway serves from, so it cannot
// come to advertise something that is not served or omit something that is.
func (g *Gateway) index() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if g.limiter != nil && !g.limiter.allow(clientIP(r, g.trusted)) {
			w.Header().Set("Retry-After", "60")
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}

		g.mu.RLock()
		cached := make(map[string]Object, len(g.cached))
		for k, v := range g.cached {
			cached[k] = v
		}
		g.mu.RUnlock()

		byGroup := map[string][]entry{}
		type ordered struct {
			e     entry
			order int
		}
		collect := map[string][]ordered{}

		for route, served := range g.cfg.Objects {
			if served.Title == "" {
				continue // served, but deliberately not advertised
			}
			name, _ := groupFor(served.Object)

			e := entry{
				Path:        route,
				Title:       served.Title,
				Description: served.Description,
			}
			if obj, ok := cached[served.Object]; ok {
				e.Available = true
				e.Size = humanBytes(len(obj.Data))
				if !obj.Updated.IsZero() {
					e.Updated = obj.Updated.UTC().Format("2006-01-02 15:04 MST")
				}
			}
			collect[name] = append(collect[name], ordered{e, served.Order})
		}

		for name, rows := range collect {
			sort.SliceStable(rows, func(i, j int) bool { return rows[i].order < rows[j].order })
			out := make([]entry, 0, len(rows))
			for _, r := range rows {
				out = append(out, r.e)
			}
			byGroup[name] = out
		}

		groups := make([]group, 0, len(byGroup))
		for _, name := range groupOrder {
			rows, ok := byGroup[name]
			if !ok {
				continue
			}
			_, note := groupFor(exampleName(rows))
			groups = append(groups, group{Name: name, Note: note, Entries: rows})
		}

		var buf bytes.Buffer
		if err := indexTmpl.Execute(&buf, struct {
			Groups    []group
			Generated string
		}{groups, time.Now().UTC().Format("2006-01-02 15:04 MST")}); err != nil {
			g.log.Error("rendering the index failed", "error", err)
			http.Error(w, "index unavailable", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// The sizes and timestamps in here go stale, so it is not worth caching
		// for long — but it is cheap to render and a minute keeps a refresh from
		// costing anything.
		w.Header().Set("Cache-Control", "public, max-age=60")
		http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(buf.Bytes()))
	})
}

// exampleName returns any path from a group, so the section note can be looked
// up by the same rule that built the section.
func exampleName(rows []entry) string {
	if len(rows) == 0 {
		return ""
	}
	return rows[0].Path
}

// humanBytes renders a size the way a person reads it.
func humanBytes(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f kB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
