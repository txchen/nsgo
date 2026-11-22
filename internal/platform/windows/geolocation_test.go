//go:build windows

package windows

import "testing"

func TestGeolocatorABIMetadata(t *testing.T) {
	if iidGeolocator != parseGUID("a9c3bf62-4524-4989-8aa9-de019d2e551f") || iidGeolocationComplete != parseGUID("7668a704-244e-5e12-8dcb-92a3299eba26") {
		t.Fatal("Windows geolocation interface IDs changed")
	}
	if geolocatorGetPosition != 13 || geoAsyncPutCompleted != 6 || geoAsyncGetResults != 8 || geoPositionCoordinate != 6 || geoCoordinateLatitude != 6 || geoCoordinateLongitude != 7 || geoCoordinateAccuracy != 9 {
		t.Fatal("Windows geolocation vtable slots changed")
	}
}

func TestGeolocationStopDropsStaleGeneration(t *testing.T) {
	w := &window{}
	run := &windowsGeoRun{w: w, id: 9, gen: 4}
	windowsGeoRuns.Lock()
	windowsGeoRuns.m[run] = struct{}{}
	windowsGeoRuns.Unlock()
	cancelWindowsLocation(w, 9, 3)
	windowsGeoRuns.Lock()
	_, stillThere := windowsGeoRuns.m[run]
	windowsGeoRuns.Unlock()
	if !stillThere {
		t.Fatal("a stop for an older generation canceled the current request")
	}
	cancelWindowsLocation(w, 9, 4)
	windowsGeoRuns.Lock()
	_, stillThere = windowsGeoRuns.m[run]
	windowsGeoRuns.Unlock()
	if stillThere {
		t.Fatal("matching stop left the request active")
	}
}
