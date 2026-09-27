package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseGBFSConfig(t *testing.T) {
	dir := t.TempDir()
	osm := filepath.Join(dir, "map.pbf")
	gtfs := filepath.Join(dir, "feed.zip")
	if err := os.WriteFile(osm, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gtfs, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := parse([]byte(fmt.Sprintf(`
[osm]
url = %q
[[gtfs]]
name = "t"
url = %q
[[gbfs]]
name = "share"
url = "https://example.test/gbfs.json"
language = "it"
poll = "11s"
max_age = "4m"
headers = ["Authorization: Bearer test"]
[routing]
scooter_speed_kmh = 22
battery_reserve_ratio = 0.2
battery_reserve_m = 750
`, osm, gtfs)))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.GBFS) != 1 || cfg.GBFS[0].Name != "share" || cfg.GBFS[0].Language != "it" ||
		cfg.GBFS[0].Poll != 11*time.Second || cfg.GBFS[0].MaxAge != 4*time.Minute ||
		cfg.GBFS[0].Headers["Authorization"] != "Bearer test" {
		t.Fatalf("unexpected GBFS config: %+v", cfg.GBFS)
	}
	if cfg.Routing.ScooterSpeedKmh != 22 || cfg.Routing.BatteryReserveRatio != .2 || cfg.Routing.BatteryReserveM != 750 {
		t.Fatalf("unexpected shared routing config: %+v", cfg.Routing)
	}
}
