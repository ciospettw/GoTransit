package gbfs

import (
	"encoding/json"
	"fmt"
	"time"
)

type rawRule struct {
	VehicleTypeIDs  []string `json:"vehicle_type_ids"`
	RideAllowed     *bool    `json:"ride_allowed"`
	StartAllowed    *bool    `json:"ride_start_allowed"`
	EndAllowed      *bool    `json:"ride_end_allowed"`
	ThroughAllowed  *bool    `json:"ride_through_allowed"`
	StationParking  *bool    `json:"station_parking"`
	MaximumSpeedKPH float64  `json:"maximum_speed_kph"`
}

func (r rawRule) normalized() Rule {
	start, end := r.StartAllowed, r.EndAllowed
	if r.RideAllowed != nil { // GBFS 2.1-2.3 combined start/end.
		if start == nil {
			start = r.RideAllowed
		}
		if end == nil {
			end = r.RideAllowed
		}
	}
	return Rule{VehicleTypeIDs: r.VehicleTypeIDs, StartAllowed: start, EndAllowed: end,
		ThroughAllowed: r.ThroughAllowed, StationParking: r.StationParking,
		MaximumSpeedKPH: r.MaximumSpeedKPH}
}

func parseGeofencing(f *Feed, b []byte, newest *time.Time, ttl *int64) error {
	if len(b) == 0 {
		return nil
	}
	var doc document
	if err := json.Unmarshal(b, &doc); err != nil {
		return fmt.Errorf("decode geofencing_zones: %w", err)
	}
	if t := parseTimestamp(doc.LastUpdated); t.After(*newest) {
		*newest = t
	}
	if doc.TTL > 0 && (*ttl == 0 || doc.TTL < *ttl) {
		*ttl = doc.TTL
	}
	var data struct {
		Geofencing struct {
			Features []struct {
				Geometry struct {
					Type        string          `json:"type"`
					Coordinates json.RawMessage `json:"coordinates"`
				} `json:"geometry"`
				Properties struct {
					Start json.RawMessage `json:"start"`
					End   json.RawMessage `json:"end"`
					Rules []rawRule       `json:"rules"`
				} `json:"properties"`
			} `json:"features"`
		} `json:"geofencing_zones"`
		GlobalRules []rawRule `json:"global_rules"`
	}
	if err := json.Unmarshal(doc.Data, &data); err != nil {
		return fmt.Errorf("decode geofencing_zones data: %w", err)
	}
	for _, rr := range data.GlobalRules {
		f.GlobalRules = append(f.GlobalRules, rr.normalized())
	}
	for _, feature := range data.Geofencing.Features {
		var coords [][][][]float64
		switch feature.Geometry.Type {
		case "MultiPolygon", "":
			if err := json.Unmarshal(feature.Geometry.Coordinates, &coords); err != nil {
				continue
			}
		case "Polygon":
			var polygon [][][]float64
			if err := json.Unmarshal(feature.Geometry.Coordinates, &polygon); err != nil {
				continue
			}
			coords = [][][][]float64{polygon}
		default:
			continue
		}
		z := Zone{Start: parseTimestamp(feature.Properties.Start), End: parseTimestamp(feature.Properties.End)}
		for _, rr := range feature.Properties.Rules {
			z.Rules = append(z.Rules, rr.normalized())
		}
		for _, polygon := range coords {
			var outPoly [][][2]float64
			for _, ring := range polygon {
				var outRing [][2]float64
				for _, point := range ring {
					if len(point) >= 2 {
						outRing = append(outRing, [2]float64{point[0], point[1]})
					}
				}
				if len(outRing) >= 4 {
					outPoly = append(outPoly, outRing)
				}
			}
			if len(outPoly) > 0 {
				z.Polygons = append(z.Polygons, outPoly)
			}
		}
		if len(z.Polygons) > 0 {
			f.Zones = append(f.Zones, z)
		}
	}
	return nil
}

func pointInZone(z Zone, lat, lon float64) bool {
	for _, polygon := range z.Polygons {
		if len(polygon) == 0 || !pointInRing(polygon[0], lon, lat) {
			continue
		}
		insideHole := false
		for _, hole := range polygon[1:] {
			if pointInRing(hole, lon, lat) {
				insideHole = true
				break
			}
		}
		if !insideHole {
			return true
		}
	}
	return false
}

func pointInRing(ring [][2]float64, x, y float64) bool {
	inside := false
	for i, j := 0, len(ring)-1; i < len(ring); j, i = i, i+1 {
		xi, yi := ring[i][0], ring[i][1]
		xj, yj := ring[j][0], ring[j][1]
		if (yi > y) != (yj > y) && x < (xj-xi)*(y-yi)/(yj-yi)+xi {
			inside = !inside
		}
	}
	return inside
}
