//go:build windows

package windows

import (
	"errors"
	"time"
	"unsafe"

	"github.com/nsgo-dev/nsgo/internal/driver"
)

var (
	iidGeolocator          = parseGUID("a9c3bf62-4524-4989-8aa9-de019d2e551f")
	iidGeolocationComplete = parseGUID("7668a704-244e-5e12-8dcb-92a3299eba26")
	iidGeocoordinate       = parseGUID("ee21a3aa-976a-4c70-803d-083ea55bcbc4")
)

const (
	geolocatorClass        = "Windows.Devices.Geolocation.Geolocator"
	geolocatorGetPosition  = 13
	geoAsyncPutCompleted   = 6
	geoAsyncGetResults     = 8
	geoAsyncCompleted      = 1
	geoPositionCoordinate  = 6
	geoCoordinateLatitude  = 6
	geoCoordinateLongitude = 7
	geoCoordinateAccuracy  = 9
	geoCoordinateTimestamp = 13
)

// sensorLocation requests one Windows geolocation fix. The WinRT operation
// completes off the window thread; only the copied driver fix crosses back.
func (w *window) sensorLocation(id uint32, req driver.SensorRequest) {
	if req.Kind != driver.SensorLocation && req.Kind != driver.SensorBackgroundLocation || req.Command != driver.SensorLocate {
		w.engine.Dispatch(driver.NoSensor(&driver.MountItem{Count: id, Sensor: &req}))
		return
	}
	comApartment()
	locator := activate(geolocatorClass, &iidGeolocator)
	if locator == nil {
		w.engine.Dispatch(driver.Event{Kind: driver.EventSensor, Count: id, Data: &driver.SensorReport{Generation: req.Generation, Error: "unavailable", End: true}})
		return
	}
	var operation *comObject
	if failed(locator.call(geolocatorGetPosition, uintptr(unsafe.Pointer(&operation)))) || operation == nil {
		locator.release()
		w.engine.Dispatch(driver.Event{Kind: driver.EventSensor, Count: id, Data: &driver.SensorReport{Generation: req.Generation, Error: "unavailable", End: true}})
		return
	}
	completed := newHandler(&iidGeolocationComplete, func(_ uintptr, status unsafe.Pointer) {
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
						_ = coordinate.call(geoCoordinateAccuracy, uintptr(unsafe.Pointer(&accuracy)))
						fix = driver.LocationFix{Latitude: lat, Longitude: lon, HorizontalAccuracy: driver.SensorNumber{Value: accuracy, Valid: accuracy >= 0}, Timestamp: time.Now()}
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
		operation.release()
		locator.release()
		w.engine.Dispatch(driver.Event{Kind: driver.EventSensor, Count: id, Data: &driver.SensorReport{Generation: req.Generation, Error: "unavailable", End: true}})
	}
}
