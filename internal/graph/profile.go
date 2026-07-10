package graph

import (
	"gotransit/internal/osm"
)

// wayProfile is the routing interpretation of one OSM way.
type wayProfile struct {
	fwd, bwd uint8 // mode flags per direction (FCar|FBike|FFoot, plus FRoundabout/FLink/FSteps on both)
	speed    uint8 // car km/h
	keep     bool
}

type hwClass struct {
	car, bike, foot bool
	speed           uint8
}

// highway class defaults; access tags refine them.
var hwClasses = map[string]hwClass{
	"motorway":       {car: true, speed: 120},
	"motorway_link":  {car: true, speed: 60},
	"trunk":          {car: true, speed: 90},
	"trunk_link":     {car: true, speed: 50},
	"primary":        {car: true, bike: true, foot: true, speed: 65},
	"primary_link":   {car: true, bike: true, foot: true, speed: 45},
	"secondary":      {car: true, bike: true, foot: true, speed: 55},
	"secondary_link": {car: true, bike: true, foot: true, speed: 40},
	"tertiary":       {car: true, bike: true, foot: true, speed: 45},
	"tertiary_link":  {car: true, bike: true, foot: true, speed: 35},
	"unclassified":   {car: true, bike: true, foot: true, speed: 40},
	"residential":    {car: true, bike: true, foot: true, speed: 28},
	"living_street":  {car: true, bike: true, foot: true, speed: 10},
	"service":        {car: true, bike: true, foot: true, speed: 15},
	"road":           {car: true, bike: true, foot: true, speed: 30},
	"pedestrian":     {foot: true, speed: 5},
	"footway":        {foot: true, speed: 5},
	"steps":          {foot: true, speed: 4},
	"path":           {bike: true, foot: true, speed: 10},
	"cycleway":       {bike: true, foot: true, speed: 18},
	"track":          {bike: true, foot: true, speed: 15},
	"bridleway":      {foot: true, speed: 5},
}

func b2s(b []byte) string { return string(b) } // small, keeps call sites readable

// classifyWay maps OSM tags to per-direction mode flags and a car speed.
// Accepts both PBF tag views and osc map tags.
func classifyWay(tags osm.Tagged) wayProfile {
	hw := tags.Get("highway")
	if hw == nil {
		return wayProfile{}
	}
	cls, ok := hwClasses[b2s(hw)]
	if !ok {
		return wayProfile{} // construction, proposed, raceway, ...
	}
	// area=yes plazas etc. are not linear ways; skip (routing across squares
	// follows their edge ways instead)
	if v := tags.Get("area"); b2s(v) == "yes" && b2s(hw) == "pedestrian" {
		return wayProfile{}
	}

	car, bike, foot := cls.car, cls.bike, cls.foot

	// global access, then per-mode overrides
	switch b2s(tags.Get("access")) {
	case "no", "private", "military":
		car, bike, foot = false, false, false
	}
	switch b2s(tags.Get("motor_vehicle")) {
	case "no", "private", "agricultural", "forestry", "delivery":
		car = false
	case "yes", "permissive", "destination":
		car = cls.car || car
	}
	switch b2s(tags.Get("vehicle")) {
	case "no", "private":
		car, bike = false, false
	}
	switch b2s(tags.Get("bicycle")) {
	case "no", "use_sidepath", "private":
		bike = false
	case "yes", "designated", "permissive":
		bike = true
	}
	switch b2s(tags.Get("foot")) {
	case "no", "private", "use_sidepath":
		foot = false
	case "yes", "designated", "permissive":
		foot = true
	}
	if b2s(hw) == "steps" {
		bike = false
	}
	if !car && !bike && !foot {
		return wayProfile{}
	}

	var base uint8
	if car {
		base |= FCar
	}
	if bike {
		base |= FBike
	}
	if foot {
		base |= FFoot
	}
	roundabout := b2s(tags.Get("junction")) == "roundabout" || b2s(tags.Get("junction")) == "circular"
	if roundabout {
		base |= FRoundabout
	}
	if len(hw) > 5 && b2s(hw[len(hw)-5:]) == "_link" {
		base |= FLink
	}
	if b2s(hw) == "steps" {
		base |= FSteps
	}

	// oneway: applies to car and bike, never foot
	fwd, bwd := base, base
	oneway := b2s(tags.Get("oneway"))
	implied := roundabout || b2s(hw) == "motorway" || b2s(hw) == "motorway_link"
	switch {
	case oneway == "yes" || oneway == "1" || oneway == "true" || (implied && oneway == ""):
		bwd &^= FCar | FBike
	case oneway == "-1":
		fwd &^= FCar | FBike
	}
	// contraflow cycling
	switch b2s(tags.Get("oneway:bicycle")) {
	case "no":
		fwd |= base & FBike
		bwd |= base & FBike
	case "yes":
		bwd &^= FBike
	case "-1":
		fwd &^= FBike
	}

	speed := cls.speed
	if ms := int(parseMaxspeed(tags.Get("maxspeed"))); ms > 0 {
		// people rarely drive at the limit on fast roads; damp a little
		if ms > 60 {
			ms = 60 + (ms-60)*9/10
		}
		speed = uint8(ms)
	}

	return wayProfile{fwd: fwd, bwd: bwd, speed: speed, keep: true}
}

// parseMaxspeed handles "50", "50 km/h", "30 mph", "walk", "IT:urban"...
func parseMaxspeed(v []byte) uint8 {
	if len(v) == 0 {
		return 0
	}
	s := b2s(v)
	switch s {
	case "walk", "IT:walk":
		return 5
	case "IT:urban":
		return 50
	case "IT:rural":
		return 90
	case "IT:trunk":
		return 110
	case "IT:motorway":
		return 130
	case "none", "signals", "variable":
		return 0
	}
	num := 0
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		num = num*10 + int(s[i]-'0')
		i++
	}
	if num == 0 {
		return 0
	}
	rest := s[i:]
	if rest == " mph" || rest == "mph" {
		num = num * 1609 / 1000
	}
	if num > 130 {
		num = 130
	}
	return uint8(num)
}

// nameOf picks the display name for turn-by-turn: name, else ref.
func nameOf(tags osm.Tagged) string {
	if n := tags.Get("name"); len(n) > 0 {
		return string(n)
	}
	if r := tags.Get("ref"); len(r) > 0 {
		return string(r)
	}
	return ""
}
