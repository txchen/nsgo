//go:build windows

package windows

import (
	"errors"
	"sync"
	"time"
	"unsafe"

	"github.com/nsgo-dev/nsgo/internal/driver"
)

type windowsGeoRun struct {
	w       *window
	id      uint32
	gen     uint32
	locator *comObject
	op      *comObject
	handler *comHandler
	token   eventToken
}

var windowsGeoRuns = struct {
	sync.Mutex
	m map[*windowsGeoRun]struct{}
}{m: map[*windowsGeoRun]struct{}{}}

func cancelWindowsLocation(w *window, id, generation uint32) {
	windowsGeoRuns.Lock()
	defer windowsGeoRuns.Unlock()
	for run := range windowsGeoRuns.m {
		if run.w == w && run.id == id && run.gen == generation {
			delete(windowsGeoRuns.m, run)
			if run.handler != nil {
				run.locator.call(geoRemovePositionChanged, uintptr(run.token.value))
				run.handler.release()
				run.locator.release()
			}
			if run.op != nil {
				run.op.call(geoAsyncCancel)
			} else if run.handler == nil {
				run.locator.release()
			}
		}
	}
}

var (
	iidGeolocator          = parseGUID("a9c3bf62-4524-4989-8aa9-de019d2e551f")
	iidGeolocationComplete = parseGUID("7668a704-244e-5e12-8dcb-92a3299eba26")
	iidGeocoordinate       = parseGUID("ee21a3aa-976a-4c70-803d-083ea55bcbc4")
	iidPositionHandler     = parseGUID("df3c6164-4e7b-5e8e-9a7e-13da059dec1e")
	iidGeolocatorStatics   = parseGUID("9a8e7571-2df5-4591-9f87-eb5fd894e9b7")
	iidAccessComplete      = parseGUID("f3524c93-e5c7-5b88-bedb-d3e637cff271")
)

const (
	geolocatorClass          = "Windows.Devices.Geolocation.Geolocator"
	geolocatorGetPosition    = 13
	geoAsyncPutCompleted     = 6
	geoAsyncGetResults       = 8
	geoAsyncCompleted        = 1
	geoPositionCoordinate    = 6
	geoCoordinateLatitude    = 6
	geoCoordinateLongitude   = 7
	geoCoordinateAccuracy    = 9
	geoCoordinateTimestamp   = 13
	geoAddPositionChanged    = 15
	geoRemovePositionChanged = 16
	geoAsyncCancel           = 9
	geoRequestAccess         = 6
)

// sensorLocation requests one Windows geolocation fix. The WinRT operation
// completes off the window thread; only the copied driver fix crosses back.
func (w *window) sensorLocation(id uint32, req driver.SensorRequest) {
	if req.Command == driver.SensorRequestPermission {
		w.requestWindowsLocationPermission(id, req)
		return
	}
	if req.Command == driver.SensorReadPermission {
		w.engine.Dispatch(driver.Event{Kind: driver.EventSensor, Count: id, Data: &driver.SensorReport{Generation: req.Generation, Grant: &driver.SensorGrant{Authorization: driver.SensorUnavailable}, End: true}})
		return
	}
	if req.Kind != driver.SensorLocation && req.Kind != driver.SensorBackgroundLocation || req.Command != driver.SensorLocate && req.Command != driver.SensorWatchLocation {
		w.engine.Dispatch(driver.NoSensor(&driver.MountItem{Count: id, Sensor: &req}))
		return
	}
	comApartment()
	if req.Command == driver.SensorWatchLocation {
		w.watchWindowsLocation(id, req)
		return
	}
	locator := activate(geolocatorClass, &iidGeolocator)
	if locator == nil {
		w.engine.Dispatch(driver.Event{Kind: driver.EventSensor, Count: id, Data: &driver.SensorReport{Generation: req.Generation, Error: "unavailable", End: true}})
		return
	}
	run := &windowsGeoRun{w: w, id: id, gen: req.Generation, locator: locator}
	windowsGeoRuns.Lock()
	windowsGeoRuns.m[run] = struct{}{}
	windowsGeoRuns.Unlock()
	var operation *comObject
	if failed(locator.call(geolocatorGetPosition, uintptr(unsafe.Pointer(&operation)))) || operation == nil {
		windowsGeoRuns.Lock()
		delete(windowsGeoRuns.m, run)
		windowsGeoRuns.Unlock()
		locator.release()
		w.engine.Dispatch(driver.Event{Kind: driver.EventSensor, Count: id, Data: &driver.SensorReport{Generation: req.Generation, Error: "unavailable", End: true}})
		return
	}
	run.op = operation
	completed := newHandler(&iidGeolocationComplete, func(_ uintptr, status unsafe.Pointer) {
		windowsGeoRuns.Lock()
		_, active := windowsGeoRuns.m[run]
		delete(windowsGeoRuns.m, run)
		windowsGeoRuns.Unlock()
		if !active {
			operation.release()
			locator.release()
			return
		}
		var fix driver.LocationFix
		var err error
		if uintptr(status) != geoAsyncCompleted {
			err = errors.New("geolocation operation did not complete")
		} else {
			var position *comObject
			if failed(operation.call(geoAsyncGetResults, uintptr(unsafe.Pointer(&position)))) || position == nil {
				err = errors.New("geolocation returned no position")
			} else {
				var coordinate *comObject
				if failed(position.call(geoPositionCoordinate, uintptr(unsafe.Pointer(&coordinate)))) || coordinate == nil {
					err = errors.New("geolocation returned no coordinate")
				} else {
					var lat, lon, accuracy float64
					if failed(coordinate.call(geoCoordinateLatitude, uintptr(unsafe.Pointer(&lat)))) || failed(coordinate.call(geoCoordinateLongitude, uintptr(unsafe.Pointer(&lon)))) {
						err = errors.New("geolocation coordinate is invalid")
					} else {
						accuracyValid := !failed(coordinate.call(geoCoordinateAccuracy, uintptr(unsafe.Pointer(&accuracy))))
						var rawTimestamp int64
						timestampValid := !failed(coordinate.call(geoCoordinateTimestamp, uintptr(unsafe.Pointer(&rawTimestamp))))
						when := time.Now()
						if timestampValid {
							when = time.Unix(0, (rawTimestamp-116444736000000000)*100)
						}
						fix = driver.LocationFix{Latitude: lat, Longitude: lon, HorizontalAccuracy: driver.SensorNumber{Value: accuracy, Valid: accuracyValid && accuracy >= 0}, Timestamp: when}
					}
					coordinate.release()
				}
				position.release()
			}
		}
		operation.release()
		locator.release()
		w.post(func() {
			if w.destroying {
				return
			}
			report := &driver.SensorReport{Generation: req.Generation, Location: &fix, End: true}
			if err != nil {
				report.Location = nil
				report.Error = "unavailable"
			}
			w.engine.Dispatch(driver.Event{Kind: driver.EventSensor, Count: id, Data: report})
		})
	})
	completed.agile = true
	hr := operation.call(geoAsyncPutCompleted, completed.ptr())
	completed.release()
	if failed(hr) {
		windowsGeoRuns.Lock()
		delete(windowsGeoRuns.m, run)
		windowsGeoRuns.Unlock()
		operation.release()
		locator.release()
		w.engine.Dispatch(driver.Event{Kind: driver.EventSensor, Count: id, Data: &driver.SensorReport{Generation: req.Generation, Error: "unavailable", End: true}})
	}
}

func (w *window) requestWindowsLocationPermission(id uint32, req driver.SensorRequest) {
	if req.Kind != driver.SensorLocation && req.Kind != driver.SensorBackgroundLocation {
		w.engine.Dispatch(driver.NoSensor(&driver.MountItem{Count: id, Sensor: &req}))
		return
	}
	comApartment()
	statics := activationFactory(geolocatorClass, &iidGeolocatorStatics)
	if statics == nil {
		w.engine.Dispatch(driver.NoSensor(&driver.MountItem{Count: id, Sensor: &req}))
		return
	}
	var operation *comObject
	if failed(statics.call(geoRequestAccess, uintptr(unsafe.Pointer(&operation)))) || operation == nil {
		statics.release()
		w.engine.Dispatch(driver.NoSensor(&driver.MountItem{Count: id, Sensor: &req}))
		return
	}
	completed := newHandler(&iidAccessComplete, func(_ uintptr, status unsafe.Pointer) {
		var value int32
		if uintptr(status) != geoAsyncCompleted || failed(operation.call(geoAsyncGetResults, uintptr(unsafe.Pointer(&value)))) {
			value = 0
		}
		operation.release()
		statics.release()
		w.post(func() {
			if !w.destroying {
				auth := driver.SensorDenied
				if value == 1 {
					auth = driver.SensorGranted
				}
				w.engine.Dispatch(driver.Event{Kind: driver.EventSensor, Count: id, Data: &driver.SensorReport{Generation: req.Generation, Grant: &driver.SensorGrant{Authorization: auth}, End: true}})
			}
		})
	})
	completed.agile = true
	hr := operation.call(geoAsyncPutCompleted, completed.ptr())
	completed.release()
	if failed(hr) {
		operation.release()
		statics.release()
		w.engine.Dispatch(driver.NoSensor(&driver.MountItem{Count: id, Sensor: &req}))
	}
}

func (w *window) watchWindowsLocation(id uint32, req driver.SensorRequest) {
	locator := activate(geolocatorClass, &iidGeolocator)
	if locator == nil {
		w.engine.Dispatch(driver.NoSensor(&driver.MountItem{Count: id, Sensor: &req}))
		return
	}
	run := &windowsGeoRun{w: w, id: id, gen: req.Generation, locator: locator}
	run.handler = newHandler(&iidPositionHandler, func(_ uintptr, args unsafe.Pointer) {
		if args == nil {
			return
		}
		var position *comObject
		if failed((*comObject)(args).call(6, uintptr(unsafe.Pointer(&position)))) || position == nil {
			return
		}
		fix, err := windowsGeoPosition(position)
		position.release()
		if err != nil {
			return
		}
		windowsGeoRuns.Lock()
		_, active := windowsGeoRuns.m[run]
		windowsGeoRuns.Unlock()
		if !active {
			return
		}
		w.post(func() {
			if !w.destroying {
				w.engine.Dispatch(driver.Event{Kind: driver.EventSensor, Count: id, Data: &driver.SensorReport{Generation: req.Generation, Location: &fix}})
			}
		})
	})
	run.handler.agile = true
	if failed(locator.call(geoAddPositionChanged, run.handler.ptr(), uintptr(unsafe.Pointer(&run.token)))) {
		run.handler.release()
		locator.release()
		w.engine.Dispatch(driver.NoSensor(&driver.MountItem{Count: id, Sensor: &req}))
		return
	}
	windowsGeoRuns.Lock()
	windowsGeoRuns.m[run] = struct{}{}
	windowsGeoRuns.Unlock()
}

func windowsGeoPosition(position *comObject) (driver.LocationFix, error) {
	var coordinate *comObject
	if failed(position.call(geoPositionCoordinate, uintptr(unsafe.Pointer(&coordinate)))) || coordinate == nil {
		return driver.LocationFix{}, errors.New("geolocation returned no coordinate")
	}
	defer coordinate.release()
	var lat, lon, accuracy float64
	if failed(coordinate.call(geoCoordinateLatitude, uintptr(unsafe.Pointer(&lat)))) || failed(coordinate.call(geoCoordinateLongitude, uintptr(unsafe.Pointer(&lon)))) {
		return driver.LocationFix{}, errors.New("geolocation coordinate is invalid")
	}
	accuracyValid := !failed(coordinate.call(geoCoordinateAccuracy, uintptr(unsafe.Pointer(&accuracy))))
	var rawTimestamp int64
	timestampValid := !failed(coordinate.call(geoCoordinateTimestamp, uintptr(unsafe.Pointer(&rawTimestamp))))
	when := time.Now()
	if timestampValid {
		when = time.Unix(0, (rawTimestamp-116444736000000000)*100)
	}
	return driver.LocationFix{Latitude: lat, Longitude: lon, HorizontalAccuracy: driver.SensorNumber{Value: accuracy, Valid: accuracyValid && accuracy >= 0}, Timestamp: when}, nil
}
