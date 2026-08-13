// Command preview renders the overlay against a still image and synthetic
// weather, so the layout can be settled before any of it runs unattended.
//
//	# render every scenario to ./out
//	preview -image tower.jpg -site "Cougar Mountain" -all
//
//	# live-reload: edit theme.json, refresh the browser
//	preview -image tower.jpg -site "Cougar Mountain" -theme theme.json -serve :8099
//
//	# stack every scenario's bar for side-by-side legibility comparison
//	preview -image tower.jpg -contact out/contact.png
package main

import (
	"errors"
	"flag"
	"fmt"
	"html/template"
	"image"
	"image/draw"
	"image/jpeg"
	"image/png"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	_ "image/gif"

	"github.com/michaelpeterswa/rattlecam/internal/frame"
	"github.com/michaelpeterswa/rattlecam/internal/light"
	"github.com/michaelpeterswa/rattlecam/internal/overlay"
	"github.com/michaelpeterswa/rattlecam/internal/wx"
)

func main() {
	var (
		imagePath  = flag.String("image", "", "source still to composite onto (required)")
		fontPath   = flag.String("font", "assets/font.ttf", "regular TTF")
		boldPath   = flag.String("bold", "assets/font-bold.ttf", "bold TTF")
		logoPath   = flag.String("logo", "assets/logo.png", "logo PNG (empty to omit)")
		annotation = flag.String("annotation", "assets/annotation.png", "registered annotation PNG, e.g. peak outlines (empty to omit)")
		themePath  = flag.String("theme", "", "theme JSON; defaults are used for anything omitted")
		dumpTheme  = flag.String("dump-theme", "", "write the full default theme here and exit")
		scenarios  = flag.String("scenarios", "", "JSON file of custom scenarios")
		scenario   = flag.String("scenario", "typical", "scenario name to render")
		all        = flag.Bool("all", false, "render every scenario")
		contact    = flag.String("contact", "", "write a stacked bar-strip comparison here")
		outDir     = flag.String("out", "out", "output directory")
		site       = flag.String("site", "Tower Cam", "site name")
		credit     = flag.String("credit", "", "standing attribution line, e.g. \"Camera brought to you by RSVU\"")
		elevation  = flag.Float64("elevation", 0, "site elevation in meters")
		tz         = flag.String("tz", "America/Los_Angeles", "timezone for the timestamp")
		staleAfter = flag.Duration("stale-after", 10*time.Minute, "observations older than this drop out")
		serveAddr  = flag.String("serve", "", "serve a live-reloading preview on this address")
		night      = flag.String("night", "auto", "night treatment: auto (measure the still, as the daemon does), on, or off")
	)
	flag.Parse()

	if *dumpTheme != "" {
		if err := overlay.DefaultTheme().Save(*dumpTheme); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("wrote %s\n", *dumpTheme)
		return
	}
	if *imagePath == "" {
		flag.Usage()
		log.Fatal("\n-image is required")
	}

	src, err := loadImage(*imagePath)
	if err != nil {
		log.Fatal(err)
	}

	loc, err := time.LoadLocation(*tz)
	if err != nil {
		log.Fatal(err)
	}

	list := wx.Scenarios
	if *scenarios != "" {
		if list, err = wx.LoadScenarios(*scenarios); err != nil {
			log.Fatal(err)
		}
	}

	theme, err := overlay.LoadTheme(*themePath)
	if err != nil {
		log.Fatal(err)
	}

	renderer, err := overlay.NewRenderer(*fontPath, *boldPath, *logoPath, theme)
	if err != nil {
		log.Fatal(err)
	}
	if *annotation != "" {
		// A missing file at the default path just means there isn't one; a path
		// given explicitly and got wrong is a real mistake. os.IsNotExist does
		// not unwrap, so this has to be errors.Is.
		if err := renderer.LoadAnnotation(*annotation); err != nil {
			if !errors.Is(err, os.ErrNotExist) || *annotation != "assets/annotation.png" {
				log.Fatal(err)
			}
		}
	}

	isNight, err := resolveNight(*night, src, light.DefaultEnter, light.DefaultExit)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("still measures %.1f mean luma; rendering the %s treatment",
		light.MeanLuma(src), map[bool]string{true: "night", false: "day"}[isNight])

	h := &harness{
		src:       src,
		renderer:  renderer,
		themePath: *themePath,
		list:      list,
		night:     isNight,
		params: frame.Params{
			SiteName:   *site,
			Credit:     *credit,
			Elevation:  *elevation,
			StaleAfter: *staleAfter,
			Location:   loc,
			MaxFields:  theme.MaxFields,
		},
	}

	if *serveAddr != "" {
		h.serve(*serveAddr)
		return
	}

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		log.Fatal(err)
	}

	if *contact != "" {
		img, err := h.contactSheet()
		if err != nil {
			log.Fatal(err)
		}
		if err := writePNG(*contact, img); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("wrote %s\n", *contact)
		if !*all {
			return
		}
	}

	targets := []string{*scenario}
	if *all {
		targets = nil
		for _, s := range h.list {
			targets = append(targets, s.Name)
		}
	}

	for _, name := range targets {
		img, err := h.render(name)
		if err != nil {
			log.Fatal(err)
		}
		path := filepath.Join(*outDir, name+".jpg")
		if err := writeJPEG(path, img, 92); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("wrote %s\n", path)
	}
}

type harness struct {
	src       image.Image
	renderer  *overlay.Renderer
	themePath string
	list      []wx.Scenario
	params    frame.Params
	night     bool
}

// resolveNight turns the -night flag into a decision.
//
// "auto" measures the still exactly as the daemon measures each frame, so
// pointing the harness at a night capture shows the night treatment without
// anyone having to know that is what they are looking at. The explicit settings
// exist to force the other treatment onto the same still for comparison.
func resolveNight(mode string, src image.Image, enter, exit float64) (bool, error) {
	switch mode {
	case "on":
		return true, nil
	case "off":
		return false, nil
	case "auto":
		d := light.Detector{Enter: enter, Exit: exit}
		night, _ := d.Observe(light.MeanLuma(src))
		return night, nil
	default:
		return false, fmt.Errorf("-night: %q is not auto, on or off", mode)
	}
}

func (h *harness) find(name string) (wx.Scenario, error) {
	for _, s := range h.list {
		if s.Name == name {
			return s, nil
		}
	}
	return wx.Scenario{}, fmt.Errorf("unknown scenario %q", name)
}

func (h *harness) render(name string) (image.Image, error) {
	s, err := h.find(name)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	f := frame.Build(h.params, s.Reading(now), s.Conditions, now)
	f.Night = h.night
	return h.renderer.Render(h.src, f)
}

// reloadTheme re-reads the theme file so the serve mode picks up edits without
// a restart. This is the whole point of moving layout out of Go constants.
func (h *harness) reloadTheme() {
	if h.themePath == "" {
		return
	}
	t, err := overlay.LoadTheme(h.themePath)
	if err != nil {
		log.Printf("theme reload failed, keeping previous: %v", err)
		return
	}
	h.renderer.SetTheme(t)
	h.params.MaxFields = t.MaxFields
}

// contactSheet stacks each scenario's bar region at native resolution. Since
// the background is identical across scenarios, the only variable is the data,
// which makes column drift and text collisions obvious at a glance.
func (h *harness) contactSheet() (image.Image, error) {
	strip := h.renderer.BarRect(h.src.Bounds())
	labelH := strip.Dy() / 3
	if labelH < 24 {
		labelH = 24
	}
	rowH := strip.Dy() + labelH

	sheet := image.NewRGBA(image.Rect(0, 0, strip.Dx(), rowH*len(h.list)))
	draw.Draw(sheet, sheet.Bounds(), image.Black, image.Point{}, draw.Src)

	for i, s := range h.list {
		img, err := h.render(s.Name)
		if err != nil {
			return nil, err
		}
		// The renderer composites through gg, which always hands back an
		// *image.RGBA. Asserting the concrete type rather than a SubImage
		// interface keeps the failure legible if that ever stops being true.
		sub, ok := img.(*image.RGBA)
		if !ok {
			return nil, fmt.Errorf("contact sheet: want *image.RGBA from renderer, got %T", img)
		}

		y := i * rowH
		label := s.Name
		if s.Note != "" {
			label += " — " + s.Note
		}
		if err := h.renderer.DrawLabel(sheet, label, 12, float64(y)+float64(labelH)/2, float64(labelH)*0.5); err != nil {
			return nil, err
		}
		draw.Draw(sheet,
			image.Rect(0, y+labelH, strip.Dx(), y+rowH),
			sub.SubImage(strip), strip.Min, draw.Src)
	}
	return sheet, nil
}

func (h *harness) serve(addr string) {
	tmpl := template.Must(template.New("p").Parse(previewHTML))

	http.HandleFunc("/frame.jpg", func(w http.ResponseWriter, r *http.Request) {
		h.reloadTheme()
		name := r.URL.Query().Get("s")
		if name == "" {
			name = h.list[0].Name
		}
		img, err := h.render(name)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Cache-Control", "no-store")
		_ = jpeg.Encode(w, img, &jpeg.Options{Quality: 92})
	})

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("s")
		if name == "" {
			name = h.list[0].Name
		}
		_ = tmpl.Execute(w, map[string]any{
			"Scenarios": h.list,
			"Current":   name,
			"Stamp":     time.Now().UnixNano(),
		})
	})

	log.Printf("preview on http://localhost%s — edit %s and refresh", addr, h.themePath)
	log.Fatal(http.ListenAndServe(addr, nil))
}

const previewHTML = `<!doctype html>
<meta charset="utf-8">
<title>rattlecam preview</title>
<style>
  body { margin: 0; background: #14161a; color: #d8dde4;
         font: 14px system-ui, sans-serif; }
  nav { padding: 12px 16px; display: flex; gap: 8px; flex-wrap: wrap;
        border-bottom: 1px solid #262b33; }
  a { color: #9aa5b4; text-decoration: none; padding: 5px 11px;
      border: 1px solid #2c323b; border-radius: 5px; }
  a.on { color: #14161a; background: #d8dde4; border-color: #d8dde4; }
  img { display: block; width: 100%; height: auto; }
  p { padding: 10px 16px; color: #6f7986; margin: 0; }
</style>
<nav>
{{$c := .Current}}{{range .Scenarios}}
  <a href="/?s={{.Name}}" class="{{if eq .Name $c}}on{{end}}">{{.Name}}</a>
{{end}}
</nav>
<img src="/frame.jpg?s={{.Current}}&amp;t={{.Stamp}}">
<p>Edit the theme file and refresh to re-render.</p>
`

func loadImage(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only
	img, _, err := image.Decode(f)
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	return img, nil
}

func writeJPEG(path string, img image.Image, quality int) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := jpeg.Encode(f, img, &jpeg.Options{Quality: quality}); err != nil {
		f.Close() //nolint:errcheck // the encode failure is the one worth reporting
		return err
	}
	// Close is where buffered data is flushed, so on a write path its error has
	// to be returned: ignoring it reports success for a file that never landed.
	return f.Close()
}

func writePNG(path string, img image.Image) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := png.Encode(f, img); err != nil {
		f.Close() //nolint:errcheck // the encode failure is the one worth reporting
		return err
	}
	return f.Close()
}
