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

	CondOpacity  float64 `json:"cond_opacity"`
	LabelOpacity float64 `json:"label_opacity"`
	StampOpacity float64 `json:"stamp_opacity"`

	// Vertical baselines within the bar, as fractions of bar height.
	UpperBaseline float64 `json:"upper_baseline"`
	LowerBaseline float64 `json:"lower_baseline"`

	LogoGap  float64 `json:"logo_gap"`  // fraction of pad, after the logo
	BlockGap float64 `json:"block_gap"` // fraction of pad, after the site block

	StampFormat string `json:"stamp_format"`

	// MaxFields caps the data strip. Past six the lower third turns to mush,
	// especially once a station downscales it for broadcast.
	MaxFields int `json:"max_fields"`
}

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

		SiteSize:  0.30,
		CondSize:  0.23,
		LabelSize: 0.18,
		ValueSize: 0.32,
		StampSize: 0.21,

		CondOpacity:  0.82,
		LabelOpacity: 0.65,
		StampOpacity: 0.72,

		UpperBaseline: 0.36,
		LowerBaseline: 0.70,

		LogoGap:  1.6,
		BlockGap: 2.0,

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
