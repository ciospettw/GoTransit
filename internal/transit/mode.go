package transit

// IsRailLikeRouteType reports whether a GTFS route normally runs in a rail
// environment where a usable VehiclePosition cannot be assumed. It covers
// the base GTFS subway/metro, rail and monorail modes plus the corresponding
// extended railway and urban-rail families.
//
// A rail-like trip can still publish a VehiclePosition. Callers that decide
// how to track a particular trip must prefer that live position when present
// and fall back to its (possibly TripUpdate-adjusted) timetable otherwise.
func IsRailLikeRouteType(routeType int) bool {
	switch {
	case routeType == 1, // Subway, Metro
		routeType == 2,  // Rail
		routeType == 12: // Monorail
		return true
	case routeType >= 100 && routeType <= 117: // extended Railway Service
		return true
	case routeType >= 400 && routeType <= 405: // extended Urban Railway Service
		return true
	default:
		return false
	}
}
