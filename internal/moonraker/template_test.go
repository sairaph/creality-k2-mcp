package moonraker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTemplateRenderValidCases(t *testing.T) {
	cases := []struct {
		name string
		tmpl Template
		args map[string]string
		want string
	}{
		{
			name: "set heater temperature extruder",
			tmpl: TemplateSetHeaterTemperature,
			args: map[string]string{"heater": "extruder", "target": "215"},
			want: "SET_HEATER_TEMPERATURE HEATER=extruder TARGET=215",
		},
		{
			name: "set heater temperature bed decimal",
			tmpl: TemplateSetHeaterTemperature,
			args: map[string]string{"heater": "heater_bed", "target": "60.5"},
			want: "SET_HEATER_TEMPERATURE HEATER=heater_bed TARGET=60.5",
		},
		{
			name: "set heater temperature zero (turn off)",
			tmpl: TemplateSetHeaterTemperature,
			args: map[string]string{"heater": "extruder", "target": "0"},
			want: "SET_HEATER_TEMPERATURE HEATER=extruder TARGET=0",
		},
		{
			name: "m106 part fan",
			tmpl: TemplateM106,
			args: map[string]string{"fan": "0", "speed": "255"},
			want: "M106 P0 S255",
		},
		{
			name: "m106 aux fan zero",
			tmpl: TemplateM106,
			args: map[string]string{"fan": "2", "speed": "0"},
			want: "M106 P2 S0",
		},
		{
			name: "m220",
			tmpl: TemplateM220,
			args: map[string]string{"percent": "120"},
			want: "M220 S120",
		},
		{
			name: "m221",
			tmpl: TemplateM221,
			args: map[string]string{"percent": "95"},
			want: "M221 S95",
		},
		{
			name: "exclude object",
			tmpl: TemplateExcludeObject,
			args: map[string]string{"name": "PART_B"},
			want: "EXCLUDE_OBJECT NAME=PART_B",
		},
		{
			name: "exclude object with dots and dashes",
			tmpl: TemplateExcludeObject,
			args: map[string]string{"name": "part-1.2_v3"},
			want: "EXCLUDE_OBJECT NAME=part-1.2_v3",
		},
		{
			name: "set heater temperature extruder at absolute max boundary",
			tmpl: TemplateSetHeaterTemperature,
			args: map[string]string{"heater": "extruder", "target": "320"},
			want: "SET_HEATER_TEMPERATURE HEATER=extruder TARGET=320",
		},
		{
			name: "set heater temperature extruder at absolute min boundary",
			tmpl: TemplateSetHeaterTemperature,
			args: map[string]string{"heater": "extruder", "target": "0"},
			want: "SET_HEATER_TEMPERATURE HEATER=extruder TARGET=0",
		},
		{
			name: "set heater temperature bed at absolute max boundary",
			tmpl: TemplateSetHeaterTemperature,
			args: map[string]string{"heater": "heater_bed", "target": "110"},
			want: "SET_HEATER_TEMPERATURE HEATER=heater_bed TARGET=110",
		},
		{
			name: "set heater temperature bed at absolute min boundary",
			tmpl: TemplateSetHeaterTemperature,
			args: map[string]string{"heater": "heater_bed", "target": "0"},
			want: "SET_HEATER_TEMPERATURE HEATER=heater_bed TARGET=0",
		},
		{
			name: "m220 at absolute min boundary",
			tmpl: TemplateM220,
			args: map[string]string{"percent": "10"},
			want: "M220 S10",
		},
		{
			name: "m220 at absolute max boundary",
			tmpl: TemplateM220,
			args: map[string]string{"percent": "200"},
			want: "M220 S200",
		},
		{
			name: "m221 at absolute min boundary",
			tmpl: TemplateM221,
			args: map[string]string{"percent": "80"},
			want: "M221 S80",
		},
		{
			name: "m221 at absolute max boundary",
			tmpl: TemplateM221,
			args: map[string]string{"percent": "120"},
			want: "M221 S120",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.tmpl.render(tc.args)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			if got != tc.want {
				t.Fatalf("render = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTemplateRenderRejectsInvalidArgs(t *testing.T) {
	cases := []struct {
		name string
		tmpl Template
		args map[string]string
	}{
		{"unknown heater", TemplateSetHeaterTemperature, map[string]string{"heater": "chamber", "target": "50"}},
		{"non-numeric target", TemplateSetHeaterTemperature, map[string]string{"heater": "extruder", "target": "hot"}},
		{"negative target", TemplateSetHeaterTemperature, map[string]string{"heater": "extruder", "target": "-1"}},
		{"missing target", TemplateSetHeaterTemperature, map[string]string{"heater": "extruder"}},
		{"nan target", TemplateSetHeaterTemperature, map[string]string{"heater": "extruder", "target": "NaN"}},
		{"inf target", TemplateSetHeaterTemperature, map[string]string{"heater": "extruder", "target": "+Inf"}},
		{"fan out of range", TemplateM106, map[string]string{"fan": "3", "speed": "100"}},
		{"fan not a number", TemplateM106, map[string]string{"fan": "x", "speed": "100"}},
		{"speed too high", TemplateM106, map[string]string{"fan": "0", "speed": "256"}},
		{"speed negative", TemplateM106, map[string]string{"fan": "0", "speed": "-1"}},
		{"speed not an integer", TemplateM106, map[string]string{"fan": "0", "speed": "12.5"}},
		{"m220 not an integer", TemplateM220, map[string]string{"percent": "fast"}},
		{"m220 negative", TemplateM220, map[string]string{"percent": "-10"}},
		{"m220 below absolute min", TemplateM220, map[string]string{"percent": "9"}},
		{"m220 above absolute max", TemplateM220, map[string]string{"percent": "201"}},
		{"m221 not an integer", TemplateM221, map[string]string{"percent": "fast"}},
		{"m221 negative", TemplateM221, map[string]string{"percent": "-10"}},
		{"m221 below absolute min", TemplateM221, map[string]string{"percent": "79"}},
		{"m221 above absolute max", TemplateM221, map[string]string{"percent": "121"}},
		{"set heater temperature extruder above absolute max", TemplateSetHeaterTemperature, map[string]string{"heater": "extruder", "target": "321"}},
		{"set heater temperature extruder far above absolute max", TemplateSetHeaterTemperature, map[string]string{"heater": "extruder", "target": "390"}},
		{"set heater temperature bed above absolute max", TemplateSetHeaterTemperature, map[string]string{"heater": "heater_bed", "target": "111"}},
		{"set heater temperature bed far above absolute max", TemplateSetHeaterTemperature, map[string]string{"heater": "heater_bed", "target": "115"}},
		{"exclude object empty name", TemplateExcludeObject, map[string]string{"name": ""}},
		{"exclude object with space", TemplateExcludeObject, map[string]string{"name": "part b"}},
		{"exclude object with gcode injection attempt", TemplateExcludeObject, map[string]string{"name": "PART_A\nM112"}},
		{"exclude object with quote", TemplateExcludeObject, map[string]string{"name": "part\"b"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.tmpl.render(tc.args); err == nil {
				t.Fatal("expected render to reject these args")
			}
		})
	}
}

func TestTemplateRenderRejectsUnknownTemplateValue(t *testing.T) {
	bogus := Template(999)
	if _, err := bogus.render(map[string]string{}); err == nil {
		t.Fatal("expected render to reject an out-of-range Template value")
	}
}

func TestRunTemplateSendsRenderedScriptAsJSON(t *testing.T) {
	var gotContentType string
	var gotBody struct {
		Script string `json:"script"`
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		if r.URL.Path != "/printer/gcode/script" {
			t.Errorf("path = %q, want %q", r.URL.Path, "/printer/gcode/script")
		}
		w.Write([]byte(`{"result": "ok"}`))
	}))
	defer ts.Close()

	c := New(ts.URL, "")
	err := c.RunTemplate(context.Background(), TemplateM220, map[string]string{"percent": "120"})
	if err != nil {
		t.Fatalf("RunTemplate: %v", err)
	}
	if gotContentType != "application/json" {
		t.Fatalf("Content-Type = %q, want %q", gotContentType, "application/json")
	}
	if gotBody.Script != "M220 S120" {
		t.Fatalf("script = %q, want %q", gotBody.Script, "M220 S120")
	}
}

func TestRunTemplateRejectsInvalidArgsWithoutSendingARequest(t *testing.T) {
	called := false
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.Write([]byte(`{"result": "ok"}`))
	}))
	defer ts.Close()

	c := New(ts.URL, "")
	err := c.RunTemplate(context.Background(), TemplateExcludeObject, map[string]string{"name": "bad name"})
	if err == nil {
		t.Fatal("expected an error for an invalid object name")
	}
	merr, ok := err.(*Error)
	if !ok {
		t.Fatalf("error type = %T, want *Error", err)
	}
	if merr.Code != CodeInvalidInput {
		t.Fatalf("Code = %q, want %q", merr.Code, CodeInvalidInput)
	}
	if called {
		t.Fatal("RunTemplate must validate before ever contacting the printer")
	}
}
