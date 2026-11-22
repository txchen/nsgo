package android

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/nsgo-dev/nsgo/internal/driver"
)

func TestLegacyRuntimeCannotTurnFitnessIntoLocationPermission(t *testing.T) {
	batch := &driver.MountBatch{Items: []driver.MountItem{
		{Kind: driver.ItemSensor, Count: 1, Sensor: &driver.SensorRequest{Kind: driver.SensorLocation, Command: driver.SensorReadPermission}},
		{Kind: driver.ItemSensor, Count: 2, Sensor: &driver.SensorRequest{Kind: driver.SensorActivityPermission, Command: driver.SensorRequestPermission, Generation: 4}},
		{Kind: driver.ItemSensor, Count: 3, Sensor: &driver.SensorRequest{Kind: driver.SensorLocation, Command: driver.SensorRequestPreciseLocation, Generation: 5}},
		{Kind: driver.ItemSensor, Count: 4, Sensor: &driver.SensorRequest{Kind: driver.SensorSteps, Command: driver.SensorStop}},
	}}
	var answers []driver.Event
	out := filterExtendedSensors(false, batch, func(ev driver.Event) { answers = append(answers, ev) })
	if len(batch.Items) != 4 || len(out.Items) != 1 || out.Items[0].Count != 1 || len(answers) != 2 {
		t.Fatal("legacy wire received a new sensor kind or the original batch changed")
	}
	grant := answers[0].Data.(*driver.SensorReport)
	if answers[0].Count != 2 || grant.Grant == nil || grant.Grant.Authorization != driver.SensorUnavailable || grant.Generation != 4 {
		t.Fatal("fitness did not answer unavailable locally")
	}
	if report := answers[1].Data.(*driver.SensorReport); answers[1].Count != 3 || report.Error != "unavailable" || report.Generation != 5 {
		t.Fatal("accuracy request reached a legacy runtime")
	}
	if filterExtendedSensors(true, batch, func(driver.Event) { t.Fatal("capable runtime answered before dispatch") }) != batch {
		t.Fatal("capable runtime's batch was copied")
	}
	if capExtendedSensors&(capFetchToFile|capHapticPatterns|capAudio|capDiscoverServices|capExtendedMessages) != 0 {
		t.Fatal("advanced sensor capability overlaps an existing runtime feature")
	}
	protocol, err := os.ReadFile("java/dev/oneo/nsgo/Protocol.java")
	if err != nil {
		t.Fatal(err)
	}
	view, err := os.ReadFile("java/dev/oneo/nsgo/NsgoView.java")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(protocol), "CAP_EXTENDED_SENSORS = 128") || capExtendedSensors != 128 || !strings.Contains(string(view), "| Protocol.CAP_EXTENDED_SENSORS;") {
		t.Fatal("runtime capability advertisement differs from the Go gate")
	}
}

func TestExtendedSensorRequestsCarryTheirOwnOptions(t *testing.T) {
	var wire writer
	writeSensorRequest(&wire, 7, driver.SensorRequest{Kind: driver.SensorGeofences, Command: driver.SensorWatchGeofences, Generation: 9, Geofences: driver.GeofenceOptions{Regions: []driver.GeofenceRegion{{ID: "own region", Latitude: 51.5, Longitude: -0.12, Radius: 80}}}, Purpose: "owned-purpose"})
	r := reader{buf: wire.buf}
	if r.u32() != 7 || r.byte() != byte(driver.SensorWatchGeofences) || r.byte() != byte(driver.SensorGeofences) || r.u32() != 9 {
		t.Fatal("extended session identity changed")
	}
	r.u64()
	r.u64()
	r.u64()
	r.u64()
	r.byte()
	r.str()
	r.str()
	r.byte()
	r.u64()
	var opts struct {
		Geofences driver.GeofenceOptions
		Purpose   string
	}
	if err := json.Unmarshal([]byte(r.str()), &opts); err != nil {
		t.Fatal(err)
	}
	if len(r.buf) != 0 || len(opts.Geofences.Regions) != 1 || opts.Geofences.Regions[0].ID != "own region" || opts.Purpose != "owned-purpose" {
		t.Fatal("extended options lost their boundaries")
	}
}

func TestExtendedSensorPipeSeparatesRegionsAndLatestSamples(t *testing.T) {
	var receiver sensorReports
	var events []driver.Event
	for i := 0; i < 100; i++ {
		for _, id := range []string{"a", "b"} {
			payload, _ := json.Marshal(struct {
				ID uint32 `json:"id"`
				driver.SensorReport
			}{11, driver.SensorReport{Generation: 3, Geofence: &driver.GeofenceSample{Region: id, State: driver.GeofenceInside}}})
			receiver.receive(payload, func(ev driver.Event) { events = append(events, ev) })
		}
	}
	if len(events) != 2 {
		t.Fatalf("queued %d events for two regions", len(events))
	}
	for _, event := range events {
		r, ok := event.Data.(*driver.SensorPacket).Take()
		if !ok || r.Geofence.Dropped != 99 || r.Generation != 3 {
			t.Fatal("region back-pressure lost data or a generation")
		}
	}
}
