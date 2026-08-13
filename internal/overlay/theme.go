package overlay

import (
	"encoding/json"
	"fmt"
	"os"
)

// RGB is a 0..1 color.
type RGB struct {
	R float64 `json:"r"`
	G float64 `json:"g"`
	B float64 `json:"b"`
}

// Theme holds every layout and color decision. All sizes are ratios, never
// pixels: BarHeight is a fraction of image height, and everything else is a
// fraction of the bar height. That means the same theme renders correctly
// whether the camera gives you 1080p or 4K.
type Theme struct {
	// Anchor is "bottom" or "top". A top bar is often better on a tower cam,
	// where the interesting terrain sits along the lower horizon.
	Anchor string `json:"anchor"`

	BarHeight  float64 `json:"bar_height"`  // fraction of image height
	BarColor   RGB     `json:"bar_color"`   //
	BarOpacity float64 `json:"bar_opacity"` //
	Pad        float64 `json:"pad"`         // fraction of bar height

	HairlineOpacity float64 `json:"hairline_opacity"`
	HairlineWeight  float64 `json:"hairline_weight"` // fraction of image height

	TextColor RGB `json:"text_color"`

	SiteSize  float64 `json:"site_size"` // all sizes are fractions of bar height
	CondSize  float64 `json:"cond_size"`
	LabelSize float64 `json:"label_size"`
	ValueSize float64 `json:"value_size"`
	StampSize float64 `json:"stamp_size"`

	// CreditSize sizes the standing attribution line when it sits in the bar,
	// as a fraction of bar height.
	CreditSize float64 `json:"credit_size"`

	// CreditPlacement decides where the attribution goes. "bar" tucks it under
	// the timestamp, where it is compact but easy to miss. "top-center" floats
	// it in its own box against the sky, which on a tower cam is the one region
	// reliably free of terrain. "none" omits it.
	CreditPlacement string `json:"credit_placement"`

	// CreditTopSize is the text size for "top-center", as a fraction of image
	// height — it is not tied to the bar, so it cannot be a fraction of it.
	CreditTopSize float64 `json:"credit_top_size"`

	// CreditMargin insets the box from the top edge, as a fraction of image
	// height.
	CreditMargin float64 `json:"credit_margin"`

	// The box behind the top-center credit. Sky swings from near-white at
	// midday to black overnight, so the text needs its own backing to stay
	// legible at both ends rather than relying on contrast with the scene.
	CreditBoxColor   RGB     `json:"credit_box_color"`
	CreditBoxOpacity float64 `json:"credit_box_opacity"`
	CreditBoxPad     float64 `json:"credit_box_pad"`    // fraction of text size
	CreditBoxRadius  float64 `json:"credit_box_radius"` // fraction of half box height; 1 is a pill

	CondOpacity   float64 `json:"cond_opacity"`
	LabelOpacity  float64 `json:"label_opacity"`
	StampOpacity  float64 `json:"stamp_opacity"`
	CreditOpacity float64 `json:"credit_opacity"`

	// Vertical baselines within the bar, as fractions of bar height.
	UpperBaseline float64 `json:"upper_baseline"`
	LowerBaseline float64 `json:"lower_baseline"`

	// AnnotationOpacity scales the registered annotation layer — the peak
	// outline and names. 1 renders it exactly as authored; above 1 strengthens
	// it, which a deliberately faint file usually needs; 0 turns it off.
	AnnotationOpacity float64 `json:"annotation_opacity"`

	// LogoPlacement decides where the mark goes. "bar" sits it inside the lower
	// third, sized to the bar height, which suits a wide horizontal lockup. The
	// corner values — "top-left", "top-right", "bottom-left", "bottom-right" —
	// place it as a badge sized by LogoHeight instead, which is what a tall
	// portrait crest needs: squeezed into an 11.5% bar it is too narrow to read.
	// "none" omits it.
	LogoPlacement string `json:"logo_placement"`

	// LogoHeight is the badge height as a fraction of image height. Corner
	// placements only; in the bar the mark is sized to the bar.
	LogoHeight float64 `json:"logo_height"`

	// LogoMargin insets the badge from the image edges, as a fraction of image
	// height, so it stays proportional across resolutions like everything else.
	LogoMargin float64 `json:"logo_margin"`

	// LogoOpacity fades the badge. Full strength suits an agency crest that is
	// the point; a plain watermark usually wants less.
	LogoOpacity float64 `json:"logo_opacity"`

	LogoGap  float64 `json:"logo_gap"`  // fraction of pad, after an in-bar logo
	BlockGap float64 `json:"block_gap"` // fraction of pad, after the site block
	ColGap   float64 `json:"col_gap"`   // fraction of pad, minimum gutter between data columns

	// ColSpread decides what happens to space left over once the data columns
	// have been measured. 1 spreads them across the whole strip; 0 packs them
	// against the site block. It matters most when sensors drop out and two
	// columns have to hold a span sized for five.
	ColSpread float64 `json:"col_spread"`

	StampFormat string `json:"stamp_format"`

	// MaxFields caps the data strip. Past six the lower third turns to mush,
	// especially once a station downscales it for broadcast.
	MaxFields int `json:"max_fields"`
}

// logoInBar reports whether the mark belongs in the lower third. An empty value
// keeps the original in-bar behaviour so an older theme file still renders.
func (t Theme) logoInBar() bool {
	return t.LogoPlacement == "" || t.LogoPlacement == "bar"
}

// LogoHidden reports whether the theme suppresses the mark entirely.
func (t Theme) LogoHidden() bool { return t.LogoPlacement == "none" }

// creditInBar reports whether the attribution belongs under the timestamp. An
// empty value keeps the original in-bar behaviour so an older theme still
// renders the same way.
func (t Theme) creditInBar() bool {
	return t.CreditPlacement == "" || t.CreditPlacement == "bar"
}

// CreditHidden reports whether the theme suppresses the attribution entirely.
func (t Theme) CreditHidden() bool { return t.CreditPlacement == "none" }

func DefaultTheme() Theme {
	return Theme{
		Anchor:     "bottom",
		BarHeight:  0.115,
		BarColor:   RGB{0, 0, 0},
		BarOpacity: 0.62,
		Pad:        0.20,

		HairlineOpacity: 0.28,
		HairlineWeight:  0.0015,

		TextColor: RGB{1, 1, 1},

		SiteSize:   0.30,
		CondSize:   0.23,
		LabelSize:  0.18,
		ValueSize:  0.32,
		StampSize:  0.21,
		CreditSize: 0.17,

		CreditPlacement: "top-center",
		CreditTopSize:   0.024,
		CreditMargin:    0.030,

		CreditBoxColor:   RGB{0, 0, 0},
		CreditBoxOpacity: 0.55,
		CreditBoxPad:     0.85,
		CreditBoxRadius:  1.0,

		CondOpacity:   0.82,
		LabelOpacity:  0.65,
		StampOpacity:  0.72,
		CreditOpacity: 0.95,

		UpperBaseline: 0.36,
		LowerBaseline: 0.70,

		AnnotationOpacity: 1.0,

		LogoPlacement: "top-left",
		LogoHeight:    0.20,
		LogoMargin:    0.025,
		LogoOpacity:   1.0,

		LogoGap:   1.6,
		BlockGap:  2.0,
		ColGap:    1.4,
		ColSpread: 1.0,

		StampFormat: "Jan 2, 2006 · 3:04 PM MST",
		MaxFields:   6,
	}
}

// LoadTheme decodes a theme file over the defaults, so a partial file only
// needs to name the handful of values you're actually experimenting with.
func LoadTheme(path string) (Theme, error) {
	t := DefaultTheme()
	if path == "" {
		return t, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return t, fmt.Errorf("overlay: theme: %w", err)
	}
	if err := json.Unmarshal(data, &t); err != nil {
		return t, fmt.Errorf("overlay: theme %s: %w", path, err)
	}
	return t, nil
}

// Save writes the theme out, which is the easiest way to get a fully populated
// starting file to edit.
func (t Theme) Save(path string) error {
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}
