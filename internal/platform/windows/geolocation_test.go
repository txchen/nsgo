//go:build windows

package windows

import (
	"sync/atomic"
	"syscall"
	"testing"
)

func TestGeolocatorABIMetadata(t *testing.T) {
	if iidGeolocator != parseGUID("a9c3bf62-4524-4989-8aa9-de019d2e551f") || iidGeolocationComplete != parseGUID("7668a704-244e-5e12-8dcb-92a3299eba26") || iidPositionHandler != parseGUID("df3c6164-4e7b-5e8e-9a7e-13da059dec1e") {
		t.Fatal("Windows geolocation interface IDs changed")
	}
	if geolocatorGetPosition != 13 || geoAsyncPutCompleted != 6 || geoAsyncGetResults != 8 || geoPositionCoordinate != 6 || geoCoordinateLatitude != 6 || geoCoordinateLongitude != 7 || geoCoordinateAccuracy != 9 || geoAddPositionChanged != 15 || geoRemovePositionChanged != 16 {
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

func TestGeolocationStopCallsAsyncCancel(t *testing.T) {
	var canceled atomic.Int32
	fakeCancel := syscall.NewCallback(func(uintptr) uintptr { canceled.Add(1); return 0 })
	fakeClose := syscall.NewCallback(func(uintptr) uintptr { return 0 })
	fakeRelease := syscall.NewCallback(func(uintptr) uintptr { return 0 })
	var infoTable [128]uintptr
	infoTable[2], infoTable[9], infoTable[10] = fakeRelease, fakeCancel, fakeClose
	info := &comObject{vtbl: &infoTable}
	// A one-shot operation is retained until its late completion releases it;
	// Stop must call IAsyncInfo.Cancel instead of releasing it underneath that
	// callback.
	w := &window{}
	run := &windowsGeoRun{w: w, id: 12, gen: 7, locator: &comObject{vtbl: &infoTable}, op: &comObject{vtbl: &infoTable}, asyncInfo: info}
	windowsGeoRuns.Lock()
	windowsGeoRuns.m[run] = struct{}{}
	windowsGeoRuns.Unlock()
	cancelWindowsLocation(w, 12, 7)
	if canceled.Load() != 1 {
		t.Fatalf("IAsyncInfo.Cancel calls = %d, want 1", canceled.Load())
	}
}
