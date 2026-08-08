package overlay

import (
	"fmt"
	"image"
	"os"
	"time"

	"github.com/fogleman/gg"
	xdraw "golang.org/x/image/draw"
	_ "image/png"
)

// Field is one label/value pair in the data strip.
type Field struct {
	Label string
	Value string
}

// Frame is everything the renderer draws. Assembling it is somebody else's
// job — see internal/frame — which keeps staleness policy out of drawing code.
type Frame struct {
	SiteName   string
	Conditions string
	CapturedAt time.Time
	Fields     []Field
}

type Renderer struct {
	fontPath     string
	boldFontPath string
	logo         image.Image
	theme        Theme
}

func NewRenderer(fontPath, boldFontPath, logoPath string, theme Theme) (*Renderer, error) {
	if _, err := os.Stat(fontPath); err != nil {
		return nil, fmt.Errorf("overlay: font: %w", err)
	}
	if boldFontPath == "" {
		boldFontPath = fontPath
	}

	r := &Renderer{fontPath: fontPath, boldFontPath: boldFontPath, theme: theme}

	if logoPath != "" {
		f, err := os.Open(logoPath)
		if err != nil {
			return nil, fmt.Errorf("overlay: logo: %w", err)
		}
		defer f.Close()
		logo, _, err := image.Decode(f)
		if err != nil {
			return nil, fmt.Errorf("overlay: logo decode: %w", err)
		}
		r.logo = logo
	}
	return r, nil
}

// SetTheme swaps the layout parameters. The preview harness calls this on
// every request so an edited theme file shows up on a browser refresh.
func (r *Renderer) SetTheme(t Theme) { r.theme = t }

func (r *Renderer) Theme() Theme { return r.theme }

// BarRect reports where the overlay bar sits for an image of the given bounds,
// so callers can crop just the strip for side-by-side legibility comparison.
func (r *Renderer) BarRect(b image.Rectangle) image.Rectangle {
	barH := int(float64(b.Dy()) * r.theme.BarHeight)
	if r.theme.Anchor == "top" {
		return image.Rect(b.Min.X, b.Min.Y, b.Max.X, b.Min.Y+barH)
	}
	return image.Rect(b.Min.X, b.Max.Y-barH, b.Max.X, b.Max.Y)
}

// Render composites the overlay onto a copy of src.
func (r *Renderer) Render(src image.Image, f Frame) (image.Image, error) {
	t := r.theme
	b := src.Bounds()
	w, h := float64(b.Dx()), float64(b.Dy())

	dc := gg.NewContextForImage(src)

	barH := h * t.BarHeight
	barY := h - barH
	if t.Anchor == "top" {
		barY = 0
	}
	pad := barH * t.Pad

	// A solid-ish bar beats a drop shadow here: a tower cam sees fresh snow,
	// blown-out sunrise and black night in the same day, and only an opaque
	// backing stays legible against all three.
	dc.SetRGBA(t.BarColor.R, t.BarColor.G, t.BarColor.B, t.BarOpacity)
	dc.DrawRectangle(0, barY, w, barH)
	dc.Fill()

	if t.HairlineOpacity > 0 {
		hairY := barY
		if t.Anchor == "top" {
			hairY = barH - h*t.HairlineWeight
		}
		dc.SetRGBA(t.TextColor.R, t.TextColor.G, t.TextColor.B, t.HairlineOpacity)
		dc.DrawRectangle(0, hairY, w, h*t.HairlineWeight)
		dc.Fill()
	}

	upper := barY + barH*t.UpperBaseline
	lower := barY + barH*t.LowerBaseline
	middle := barY + barH*0.5

	x := pad

	// Logo, rescaled once per frame to the current bar height.
	if r.logo != nil {
		lb := r.logo.Bounds()
		targetH := barH - 2*pad
		targetW := targetH * float64(lb.Dx()) / float64(lb.Dy())
		dst := image.NewRGBA(image.Rect(0, 0, int(targetW), int(targetH)))
		xdraw.CatmullRom.Scale(dst, dst.Bounds(), r.logo, lb, xdraw.Over, nil)
		dc.DrawImage(dst, int(x), int(barY+pad))
		x += targetW + pad*t.LogoGap
	}

	// Site name over conditions.
	if f.SiteName != "" || f.Conditions != "" {
		widest := 0.0

		if err := dc.LoadFontFace(r.boldFontPath, barH*t.SiteSize); err != nil {
			return nil, err
		}
		dc.SetRGB(t.TextColor.R, t.TextColor.G, t.TextColor.B)
		dc.DrawStringAnchored(f.SiteName, x, upper, 0, 0.5)
		widest, _ = dc.MeasureString(f.SiteName)

		if f.Conditions != "" {
			if err := dc.LoadFontFace(r.fontPath, barH*t.CondSize); err != nil {
				return nil, err
			}
			dc.SetRGBA(t.TextColor.R, t.TextColor.G, t.TextColor.B, t.CondOpacity)
			dc.DrawStringAnchored(f.Conditions, x, lower, 0, 0.5)
			if cw, _ := dc.MeasureString(f.Conditions); cw > widest {
				widest = cw
			}
		}
		x += widest + pad*t.BlockGap
	}

	// Timestamp pins to the right edge; the data columns fill what's left.
	rightEdge := w - pad
	if !f.CapturedAt.IsZero() {
		if err := dc.LoadFontFace(r.fontPath, barH*t.StampSize); err != nil {
			return nil, err
		}
		stamp := f.CapturedAt.Format(t.StampFormat)
		tw, _ := dc.MeasureString(stamp)
		dc.SetRGBA(t.TextColor.R, t.TextColor.G, t.TextColor.B, t.StampOpacity)
		dc.DrawStringAnchored(stamp, w-pad, middle, 0, 0.5)
		rightEdge = w - pad - tw - pad*1.5
	}

	if len(f.Fields) > 0 && rightEdge > x {
		colW := (rightEdge - x) / float64(len(f.Fields))
		for i, fld := range f.Fields {
			cx := x + colW*float64(i)

			if err := dc.LoadFontFace(r.fontPath, barH*t.LabelSize); err != nil {
				return nil, err
			}
			dc.SetRGBA(t.TextColor.R, t.TextColor.G, t.TextColor.B, t.LabelOpacity)
			dc.DrawStringAnchored(fld.Label, cx, upper, 0, 0.5)

			if err := dc.LoadFontFace(r.boldFontPath, barH*t.ValueSize); err != nil {
				return nil, err
			}
			dc.SetRGB(t.TextColor.R, t.TextColor.G, t.TextColor.B)
			dc.DrawStringAnchored(fld.Value, cx, lower, 0, 0.5)
		}
	}

	return dc.Image(), nil
}

// DrawLabel writes a caption onto an image, used by the contact sheet.
func (r *Renderer) DrawLabel(dst *image.RGBA, text string, x, y, size float64) error {
	dc := gg.NewContextForRGBA(dst)
	if err := dc.LoadFontFace(r.boldFontPath, size); err != nil {
		return err
	}
	dc.SetRGB(1, 1, 1)
	dc.DrawStringAnchored(text, x, y, 0, 0.5)
	return nil
}
