package wx

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/michaelpeterswa/rattlecam/internal/aqi"
)

// Scenario is a synthetic observation plus the conditions text that would
// accompany it. Values are in the same native units Influx stores, so a
// scenario exercises every conversion the real pipeline performs.
type Scenario struct {
	Name       string             `json:"name"`
	Note       string             `json:"note,omitempty"`
	Conditions string             `json:"conditions"`
	AgeMinutes float64            `json:"age_minutes"`   // how stale the observation is
	Fields     map[string]float64 `json:"fields"`        // nil means "station offline"
	Air        *Air               `json:"air,omitempty"` // nil means "no air quality feed"
}

// Air is the synthetic air quality reading that accompanies a scenario. It
// arrives from a different source on a different schedule than the station,
// so it carries its own age.
type Air struct {
	AQI        int     `json:"aqi"`
	Level      string  `json:"level"`
	PM25       float64 `json:"pm25"`
	AgeMinutes float64 `json:"age_minutes"`
}

// AirReading converts the scenario's air quality into what the daemon's
// poller returns.
func (s Scenario) AirReading(now time.Time) *aqi.Reading {
	if s.Air == nil {
		return nil
	}
	return &aqi.Reading{
		ObservedAt: now.Add(-time.Duration(s.Air.AgeMinutes * float64(time.Minute))),
		AQI:        s.Air.AQI,
		Level:      s.Air.Level,
		PM25:       s.Air.PM25,
		HasPM25:    true,
	}
}

// Reading converts the scenario into the same type the Influx source returns.
func (s Scenario) Reading(now time.Time) *Reading {
	if s.Fields == nil {
		return nil
	}
	f := make(map[string]float64, len(s.Fields))
	for k, v := range s.Fields {
		f[k] = v
	}
	return &Reading{
		ObservedAt: now.Add(-time.Duration(s.AgeMinutes * float64(time.Minute))),
		Fields:     f,
	}
}

// Scenarios are the cases worth looking at before committing to a layout.
// Most of these exist because they're the ones that break a design you tuned
// against a single pleasant afternoon reading.
var Scenarios = []Scenario{
	{
		Name:       "typical",
		Note:       "mild PNW afternoon, nothing unusual",
		Conditions: "Partly Cloudy",
		Fields: map[string]float64{
			"temp": 18.3, "humidity": 62, "dew_point": 10.9, "p": 1014.2,
			"wind_avg": 3.1, "wind_gust": 4.4, "wind_lull": 1.8, "wind_direction": 225,
			"uv": 3.2, "solar_radiation": 480, "precipitation": 0, "precipitation_type": 0,
			"strike_count": 0, "strike_distance": 0,
		},
		Air: &Air{AQI: 42, Level: "Good", PM25: 9.4},
	},
	{
		Name:       "wide-values",
		Note:       "widest plausible strings — the layout must survive this",
		Conditions: "Thunderstorm in Vicinity Heavy Rain",
		Fields: map[string]float64{
			"temp": -17.8, "humidity": 100, "dew_point": -19.4, "p": 1043.7,
			"wind_avg": 21.5, "wind_gust": 33.9, "wind_lull": 14.2, "wind_direction": 292,
			"uv": 11.4, "solar_radiation": 1180, "precipitation": 12.7, "precipitation_type": 1,
			"strike_count": 47, "strike_distance": 3,
		},
		Air: &Air{AQI: 150, Level: "Unhealthy for Sensitive Groups", PM25: 55.4},
	},
	{
		Name:       "calm",
		Note:       "no gust delta, so the gust suffix should disappear",
		Conditions: "Clear",
		Fields: map[string]float64{
			"temp": 24.6, "humidity": 41, "dew_point": 10.5, "p": 1018.9,
			"wind_avg": 0.4, "wind_gust": 0.6, "wind_lull": 0.2, "wind_direction": 0,
			"uv": 7.1, "solar_radiation": 890,
		},
		Air: &Air{AQI: 18, Level: "Good", PM25: 4.1},
	},
	{
		Name:       "night",
		Note:       "dark frame, no solar — check bar contrast against a black sky",
		Conditions: "Fair",
		Fields: map[string]float64{
			"temp": 6.2, "humidity": 91, "dew_point": 4.9, "p": 1009.4,
			"wind_avg": 1.2, "wind_gust": 2.0, "wind_direction": 45,
			"uv": 0, "solar_radiation": 0,
		},
		Air: &Air{AQI: 31, Level: "Good", PM25: 7.3},
	},
	{
		Name:       "partial",
		Note:       "sensor dropout — several fields absent, columns must reflow",
		Conditions: "Mostly Cloudy",
		Fields: map[string]float64{
			"temp": 11.0, "humidity": 77,
		},
	},
	{
		Name:       "smoke",
		Note:       "wildfire smoke — the index is the story, and the station is fine",
		Conditions: "Smoke",
		Fields: map[string]float64{
			"temp": 27.1, "humidity": 24, "dew_point": 5.0, "p": 1011.3,
			"wind_avg": 1.1, "wind_gust": 1.9, "wind_direction": 90,
			"uv": 2.0, "solar_radiation": 210,
		},
		Air: &Air{AQI: 214, Level: "Very Unhealthy", PM25: 164.2},
	},
	{
		Name:       "stale-air",
		Note:       "station fresh but the air quality feed has gone quiet — the field drops alone",
		Conditions: "Haze",
		Fields: map[string]float64{
			"temp": 21.4, "humidity": 48, "dew_point": 9.8, "p": 1013.0,
			"wind_avg": 2.2, "wind_gust": 3.1, "wind_direction": 200,
		},
		Air: &Air{AQI: 77, Level: "Moderate", PM25: 24.0, AgeMinutes: 45},
	},
	{
		Name:       "stale",
		Note:       "observation past the threshold — all data fields drop out",
		Conditions: "Overcast",
		AgeMinutes: 45,
		Fields: map[string]float64{
			"temp": 14.0, "humidity": 55, "dew_point": 5.1, "p": 1012.0,
			"wind_avg": 2.5, "wind_gust": 3.0, "wind_direction": 180,
		},
	},
	{
		Name:       "offline",
		Note:       "station silent — image still publishes, bare of data",
		Conditions: "Light Rain",
	},
	{
		Name: "no-conditions",
		Note: "api.weather.gov unreachable — site name sits alone",
		Fields: map[string]float64{
			"temp": 9.4, "humidity": 84, "dew_point": 6.8, "p": 1002.1,
			"wind_avg": 8.9, "wind_gust": 15.2, "wind_direction": 158,
		},
		Air: &Air{AQI: 58, Level: "Moderate", PM25: 15.2},
	},
}

// ScenarioByName looks up a built-in scenario.
func ScenarioByName(name string) (Scenario, error) {
	for _, s := range Scenarios {
		if s.Name == name {
			return s, nil
		}
	}
	return Scenario{}, fmt.Errorf("wx: unknown scenario %q (try %v)", name, ScenarioNames())
}

func ScenarioNames() []string {
	names := make([]string, len(Scenarios))
	for i, s := range Scenarios {
		names[i] = s.Name
	}
	sort.Strings(names)
	return names
}

// LoadScenarios reads a JSON array of scenarios, so you can drop in a real
// observation you pulled off the station and render against that instead.
func LoadScenarios(path string) ([]Scenario, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []Scenario
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("wx: scenarios %s: %w", path, err)
	}
	return out, nil
}
